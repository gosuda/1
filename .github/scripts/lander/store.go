// Package lander implements the single-writer landing service for gosuda/0.
//
// The lander is level-triggered: every cycle compares refs and trees and performs whatever work
// is outstanding, so lost, duplicated, or delayed triggers never change the outcome.
package lander

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Access selects the narrowest credential a git operation needs.
type Access int

const (
	// Read needs contents:read.
	Read Access = iota
	// Write needs contents:write.
	Write
	// WriteWorkflows needs contents:write and workflows:write (mirror export).
	WriteWorkflows
)

// TokenSource mints installation tokens scoped to a single repository ("owner/name").
type TokenSource interface {
	Token(ctx context.Context, repo string, access Access) (string, error)
}

// Remote is a git remote. Repo ("owner/name") scopes tokens; it may be empty for token-less
// remotes such as local paths used in tests.
type Remote struct {
	URL  string
	Repo string
}

// Store wraps one bare repository that caches every remote the lander touches. Remotes live in
// disjoint ref namespaces (see refs.go), so a single object database serves all of them and the
// lander never depends on partial-clone promisors.
type Store struct {
	Dir    string
	Tokens TokenSource
	Log    *slog.Logger
	// Identity is the committer on every commit the lander creates. Empty means LegacyIdentity.
	Identity Identity
	// Signing, when non-nil, signs commits created by CommitTree.
	Signing *Signing
}

// Identity is the name and email on commits the lander creates (committer always, author for
// commits the lander itself writes: overlay syncs, baselines, front page, staged roots).
type Identity struct {
	Name  string
	Email string
}

// LegacyIdentity was used before the bot identity; verification must keep accepting it for
// commits already in the history.
var LegacyIdentity = Identity{Name: "Suda Lander", Email: "lander@gosuda.org"}

func (s *Store) identity() Identity {
	if s.Identity.Name == "" || s.Identity.Email == "" {
		return LegacyIdentity
	}
	return s.Identity
}

// Signing configures SSH commit signing for commits the lander authors.
type Signing struct {
	KeyFile        string // private key (or public key with agent)
	AllowedSigners string // allowed signers file used for tip verification
}

// isAncestor reports whether commit a is an ancestor of (or equal to) commit b. A commit missing from
// the store is not an ancestor.
func (s *Store) isAncestor(ctx context.Context, a, b string) bool {
	_, err := s.Git(ctx, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Git runs git in the store without remote credentials.
func (s *Store) Git(ctx context.Context, args ...string) (string, error) {
	return s.run(ctx, nil, nil, nil, args...)
}

// GitEnv runs git with extra environment variables.
func (s *Store) GitEnv(ctx context.Context, env []string, args ...string) (string, error) {
	return s.run(ctx, nil, env, nil, args...)
}

// GitStdin runs git with the given stdin.
func (s *Store) GitStdin(ctx context.Context, stdin string, args ...string) (string, error) {
	return s.run(ctx, nil, nil, strings.NewReader(stdin), args...)
}

// Project metadata is read in the lander before sandbox resource limits apply. Bound both the
// number of responses and their total body bytes, including repeated requests for the same OID.
const (
	maxManifestBlobs      = 16384
	maxManifestBlobBytes  = 1 << 20
	maxManifestBatchBytes = 32 << 20
	maxBatchStderrBytes   = 64 << 10
)

// readBlobs reads full blob object IDs from ListTree in one Git process. Results preserve request
// order and exact bytes. Invalid, missing, non-blob, or over-budget objects fail the entire read.
func (s *Store) readBlobs(ctx context.Context, entries []TreeEntry) ([]string, error) {
	if len(entries) > maxManifestBlobs {
		return nil, fmt.Errorf("read blobs: manifest count exceeds limit of %d", maxManifestBlobs)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, e := range entries {
		if (len(e.OID) != 40 && len(e.OID) != 64) || strings.Trim(e.OID, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("read %s: invalid object ID %q", e.Path, e.OID)
		}
		input.WriteString(e.OID)
		input.WriteByte('\n')
	}
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"cat-file", "--batch", "--buffer"}
	cmd := s.gitCommand(batchCtx, nil, args...)
	cmd.Stdin = strings.NewReader(input.String())
	var stderr cappedGitStderr
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("read blobs: open Git stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read blobs: %w", &GitError{Args: args, Err: err})
	}
	bodies, parseErr := parseBlobBatch(out, entries)
	if parseErr != nil {
		cancel() // Stop Git instead of draining an oversized or invalid response.
	}
	waitErr := cmd.Wait() // Reap the child and finish stderr collection on every started path.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if waitErr != nil {
		gitErr := &GitError{Args: args, Err: waitErr, Stderr: stderr.String()}
		return nil, fmt.Errorf("read blobs: %w", errors.Join(parseErr, gitErr))
	}
	if parseErr != nil {
		return nil, parseErr
	}
	return bodies, nil
}

// parseBlobBatch consumes one size-delimited response per entry and then requires EOF. Each
// iteration leaves the reader at the next header. Check the byte budget before allocating or
// reading a body; contents cannot become framing, and rejected responses are never drained.
func parseBlobBatch(out io.Reader, entries []TreeEntry) ([]string, error) {
	if len(entries) > maxManifestBlobs {
		return nil, fmt.Errorf("read blobs: manifest count exceeds limit of %d", maxManifestBlobs)
	}
	// A SHA-256 OID, type, and decimal size fit well within this fixed header buffer.
	r := bufio.NewReaderSize(out, 256)
	var bodies []string
	if len(entries) > 0 {
		bodies = make([]string, 0, len(entries))
	}
	total := 0
	for _, e := range entries {
		header, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("read %s: batch header exceeds limit", e.Path)
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: truncated batch header: %w", e.Path, err)
		}
		fields := strings.Split(string(header[:len(header)-1]), " ")
		if len(fields) == 2 && fields[0] == e.OID && fields[1] == "missing" {
			return nil, fmt.Errorf("read %s: object %s is missing", e.Path, e.OID)
		}
		if len(fields) != 3 || fields[0] != e.OID || fields[1] != "blob" {
			return nil, fmt.Errorf("read %s: unexpected batch header %q", e.Path, header)
		}
		declared, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("read %s: invalid blob size %q: %w", e.Path, fields[2], err)
		}
		if declared > maxManifestBlobBytes {
			return nil, fmt.Errorf("read %s: blob size %d exceeds limit of %d bytes", e.Path, declared, maxManifestBlobBytes)
		}
		size := int(declared) // Conversion is safe after the per-blob limit.
		if size > maxManifestBatchBytes-total {
			return nil, fmt.Errorf("read %s: blob batch exceeds limit of %d bytes", e.Path, maxManifestBatchBytes)
		}
		total += size
		body := make([]byte, size+1) // Git's newline separator is required even for an empty blob.
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("read %s: truncated blob of size %d: %w", e.Path, size, err)
		}
		if body[size] != '\n' {
			return nil, fmt.Errorf("read %s: missing blob terminator", e.Path)
		}
		bodies = append(bodies, string(body[:size]))
	}
	if _, err := r.ReadByte(); err == nil {
		return nil, errors.New("read blobs: unexpected trailing batch output")
	} else if err != io.EOF {
		return nil, fmt.Errorf("read blobs: read trailing batch output: %w", err)
	}
	return bodies, nil
}

// cappedGitStderr keeps bounded diagnostics while allowing the subprocess to finish writing.
type cappedGitStderr struct {
	data []byte
}

func (b *cappedGitStderr) Write(p []byte) (int, error) {
	n := min(len(p), maxBatchStderrBytes-len(b.data))
	b.data = append(b.data, p[:n]...)
	return len(p), nil
}

func (b *cappedGitStderr) String() string { return string(b.data) }

// GitRemote runs git with credentials for remote injected through the environment, never argv.
func (s *Store) GitRemote(ctx context.Context, r Remote, access Access, args ...string) (string, error) {
	env, err := s.authEnv(ctx, r, access)
	if err != nil {
		return "", err
	}
	return s.run(ctx, nil, env, nil, args...)
}

func (s *Store) authEnv(ctx context.Context, r Remote, access Access) ([]string, error) {
	if s.Tokens == nil || r.Repo == "" {
		return nil, nil
	}
	tok, err := s.Tokens.Token(ctx, r.Repo, access)
	if err != nil {
		return nil, fmt.Errorf("token for %s: %w", r.Repo, err)
	}
	hdr := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + r.URL + ".extraheader",
		"GIT_CONFIG_VALUE_0=" + hdr,
	}, nil
}

func (s *Store) run(ctx context.Context, _ any, env []string, stdin io.Reader, args ...string) (string, error) {
	out, err := s.runRaw(ctx, env, stdin, args...)
	return strings.TrimRight(out, "\n"), err
}

// gitCommand applies the same noninteractive environment to buffered and streaming Git commands.
func (s *Store) gitCommand(ctx context.Context, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", s.Dir}, args...)...)
	id := s.identity()
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_COMMITTER_NAME="+id.Name,
		"GIT_COMMITTER_EMAIL="+id.Email,
	)
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

// runRaw preserves stdout byte for byte for Git callers that need buffered output.
func (s *Store) runRaw(ctx context.Context, env []string, stdin io.Reader, args ...string) (string, error) {
	cmd := s.gitCommand(ctx, env, args...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), &GitError{Args: redact(args), Err: err, Stderr: errb.String(), Stdout: out.String()}
	}
	return out.String(), nil
}

// GitError carries stderr of a failed git invocation.
type GitError struct {
	Args           []string
	Err            error
	Stderr, Stdout string
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

// ExitCode returns the process exit code, or -1.
func (e *GitError) ExitCode() int {
	var ee *exec.ExitError
	if errors.As(e.Err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func redact(args []string) []string { return args }

// Init creates the bare store when missing.
func (s *Store) Init(ctx context.Context) error {
	if _, err := os.Stat(s.Dir + "/HEAD"); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "init", "-q", "--bare", s.Dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %v: %s", err, out)
	}
	for k, v := range map[string]string{"gc.auto": "0", "core.logAllRefUpdates": "false", "protocol.version": "2"} {
		if _, err := s.Git(ctx, "config", k, v); err != nil {
			return err
		}
	}
	return nil
}

// Fetch updates refs in dst namespaces from remote. Refspecs must be forced ("+src:dst").
func (s *Store) Fetch(ctx context.Context, r Remote, refspecs ...string) error {
	args := append([]string{"fetch", "-q", "--no-tags", "--prune", r.URL}, refspecs...)
	_, err := s.GitRemote(ctx, r, Read, args...)
	return err
}

// FetchNoPrune fetches without pruning (used for sparse, non-owned namespaces).
func (s *Store) FetchNoPrune(ctx context.Context, r Remote, refspecs ...string) error {
	args := append([]string{"fetch", "-q", "--no-tags", r.URL}, refspecs...)
	_, err := s.GitRemote(ctx, r, Read, args...)
	return err
}

// Push pushes refspecs with explicit leases. leases are "ref:expected" strings; atomic selects --atomic.
func (s *Store) Push(ctx context.Context, r Remote, access Access, atomic bool, leases []string, refspecs ...string) error {
	args := []string{"push", "-q"}
	if atomic {
		args = append(args, "--atomic")
	}
	for _, l := range leases {
		args = append(args, "--force-with-lease="+l)
	}
	args = append(args, r.URL)
	args = append(args, refspecs...)
	_, err := s.GitRemote(ctx, r, access, args...)
	return err
}

// RevParse resolves rev to an object id; ok is false when it does not exist.
func (s *Store) RevParse(ctx context.Context, rev string) (string, bool) {
	out, err := s.Git(ctx, "rev-parse", "--verify", "-q", rev)
	if err != nil {
		return "", false
	}
	return out, true
}

// Tree returns the tree id of a commit-ish.
func (s *Store) Tree(ctx context.Context, rev string) (string, error) {
	return s.Git(ctx, "rev-parse", "--verify", rev+"^{tree}")
}

// CommitMeta reads author identity, date, and the full message of a commit.
type CommitMeta struct {
	AuthorName, AuthorEmail, AuthorDate string
	Message                             string
}

// Meta returns metadata of commit.
func (s *Store) Meta(ctx context.Context, commit string) (CommitMeta, error) {
	out, err := s.Git(ctx, "log", "-1", "--format=%an%x00%ae%x00%aI%x00%B", commit)
	if err != nil {
		return CommitMeta{}, err
	}
	p := strings.SplitN(out, "\x00", 4)
	if len(p) != 4 {
		return CommitMeta{}, fmt.Errorf("unexpected log output for %s", commit)
	}
	return CommitMeta{AuthorName: p[0], AuthorEmail: p[1], AuthorDate: p[2], Message: strings.TrimRight(p[3], "\n") + "\n"}, nil
}

// CommitTree creates a commit. parents may be empty. When meta carries an author it is preserved.
func (s *Store) CommitTree(ctx context.Context, tree string, parents []string, msg string, author *CommitMeta) (string, error) {
	var args []string
	if s.Signing != nil {
		args = append(args, "-c", "gpg.format=ssh", "-c", "user.signingkey="+s.Signing.KeyFile)
	}
	args = append(args, "commit-tree", tree)
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	if s.Signing != nil {
		args = append(args, "-S")
	}
	var env []string
	if author != nil && author.AuthorName != "" {
		env = append(env, "GIT_AUTHOR_NAME="+author.AuthorName, "GIT_AUTHOR_EMAIL="+author.AuthorEmail, "GIT_AUTHOR_DATE="+author.AuthorDate)
	} else {
		id := s.identity()
		env = append(env, "GIT_AUTHOR_NAME="+id.Name, "GIT_AUTHOR_EMAIL="+id.Email)
	}
	return s.run(ctx, nil, env, strings.NewReader(msg), args...)
}

func itoa(n int) string { return strconv.Itoa(n) }
