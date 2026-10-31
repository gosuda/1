package lander

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DockerPresubmit validates a candidate by running the tests of every affected project in its own
// network-less container. Repository code never runs in the lander process: the lander exports the
// affected closure from the object store, warms dependency caches with commands that do not execute
// package code, and hands the sources to the sandbox read-only.
type DockerPresubmit struct {
	Image     string        // sandbox image; must already exist locally (never pulled at run time)
	Docker    string        // docker CLI; default "docker"
	Timeout   time.Duration // per container and per prepare command; default 15m
	CPUs      string        // --cpus; default "2"
	Memory    string        // --memory; default "4g"
	PidsLimit int           // --pids-limit; default 512
	CacheDir  string        // host dependency cache root (gomod/, cargo/, bun/); required
	Log       *slog.Logger  // default slog.Default()

	// TestCommands, when set, replaces the default commands of a project. Each element is one shell
	// command; they run in order and the first failure fails the project.
	TestCommands func(p Project) []string
}

var _ Presubmit = (*DockerPresubmit)(nil)

const (
	presubmitTimeout = 15 * time.Minute
	presubmitCPUs    = "2"
	presubmitMemory  = "4g"
	presubmitPids    = 512
	sandboxUser      = "65534:65534"
	sandboxSrc       = "/src"
	sandboxCache     = "/cache"
	sandboxWork      = "/work"
	logTailBytes     = 4 << 10
	summaryRunes     = 140
)

// sandboxEnv is the whole environment of a sandbox container; the host environment never enters.
var sandboxEnv = []string{
	"HOME=/tmp",
	"GOMODCACHE=" + sandboxCache + "/gomod",
	"GOFLAGS=-mod=readonly",
	"GOPROXY=off",
	"GOCACHE=/tmp/gocache",
	"GOWORK=off",
	"GOTOOLCHAIN=local",
	"CARGO_HOME=/tmp/cargo",
	"CARGO_NET_OFFLINE=true",
}

// prepareEnvNames are the host variables that package managers in the prepare step inherit. Credentials
// are never among them.
var prepareEnvNames = []string{
	"PATH", "TMPDIR", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR", "RUSTUP_HOME",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy",
}

// dockerEnvNames are the host variables the docker CLI needs to reach the daemon.
var dockerEnvNames = []string{
	"PATH", "HOME", "TMPDIR", "XDG_RUNTIME_DIR", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG",
	"DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH",
}

// Run implements Presubmit.
func (p *DockerPresubmit) Run(ctx context.Context, in PresubmitInput) (PresubmitResult, error) {
	cfg, err := p.config()
	if err != nil {
		return PresubmitResult{}, err
	}
	if in.Store == nil {
		return PresubmitResult{}, errors.New("presubmit: Store is required")
	}
	g, err := loadProjects(ctx, in.Store, in.NewTree)
	if err != nil {
		if ctx.Err() != nil {
			return PresubmitResult{}, ctx.Err()
		}
		return failedVerdict("FAILED: scan", err.Error()), nil
	}
	affected := g.affected(in.Changed)
	if len(affected) == 0 {
		return PresubmitResult{Passed: true, Summary: "no affected projects"}, nil
	}
	if _, err := exec.LookPath(cfg.Docker); err != nil {
		return PresubmitResult{}, fmt.Errorf("presubmit: %w", err)
	}
	for _, sub := range []string{"gomod", "cargo", "bun"} {
		if err := os.MkdirAll(filepath.Join(cfg.CacheDir, sub), 0o755); err != nil {
			return PresubmitResult{}, fmt.Errorf("presubmit: cache: %w", err)
		}
	}

	work, err := os.MkdirTemp("", "lander-presubmit-*")
	if err != nil {
		return PresubmitResult{}, fmt.Errorf("presubmit: %w", err)
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")
	home := filepath.Join(work, "home")
	for _, dir := range []string{src, home} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return PresubmitResult{}, fmt.Errorf("presubmit: %w", err)
		}
	}
	if strings.Contains(src, ":") {
		return PresubmitResult{}, fmt.Errorf("presubmit: work directory %q contains ':'", src)
	}

	closure := g.closure(affected)
	if err := exportProjects(ctx, in.Store, in.NewTree, g, closure, src); err != nil {
		if ctx.Err() != nil {
			return PresubmitResult{}, ctx.Err()
		}
		return failedVerdict("FAILED: archive", err.Error()), nil
	}

	r := &presubmitReport{}
	prepFailed := map[string]bool{}
	for _, n := range closure {
		if err := cfg.prepare(ctx, g, n, src, home, r); err != nil {
			return PresubmitResult{}, err
		}
		if r.failedIDs[n.id()] {
			prepFailed[n.id()] = true
		}
	}
	for _, n := range affected {
		if prepFailed[n.id()] {
			continue
		}
		cmds := cfg.testCommands(n)
		if len(cmds) == 0 {
			r.noTests++
			r.note("no tests declared: " + n.Dir + " [" + n.Kind + "]")
			continue
		}
		reason, tail, err := cfg.runSandbox(ctx, src, n.Project, cmds)
		if err != nil {
			return PresubmitResult{}, err
		}
		if reason != "" {
			cfg.Log.Warn("presubmit project failed", "dir", n.Dir, "kind", n.Kind, "reason", reason)
			r.fail(n.Project, reason, tail)
			continue
		}
		cfg.Log.Info("presubmit project passed", "dir", n.Dir, "kind", n.Kind)
		r.passed++
	}
	return r.result(), nil
}

// config validates p and returns a copy with defaults applied.
func (p *DockerPresubmit) config() (DockerPresubmit, error) {
	c := *p
	if c.Image == "" {
		return c, errors.New("presubmit: Image is required")
	}
	if c.CacheDir == "" {
		return c, errors.New("presubmit: CacheDir is required")
	}
	abs, err := filepath.Abs(c.CacheDir)
	if err != nil {
		return c, fmt.Errorf("presubmit: CacheDir: %w", err)
	}
	if strings.Contains(abs, ":") {
		return c, fmt.Errorf("presubmit: CacheDir %q contains ':'", abs)
	}
	c.CacheDir = abs
	if c.Docker == "" {
		c.Docker = "docker"
	}
	if c.Timeout <= 0 {
		c.Timeout = presubmitTimeout
	}
	if c.CPUs == "" {
		c.CPUs = presubmitCPUs
	}
	if c.Memory == "" {
		c.Memory = presubmitMemory
	}
	if c.PidsLimit <= 0 {
		c.PidsLimit = presubmitPids
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	return c, nil
}

// testCommands returns the commands that test n.
func (cfg DockerPresubmit) testCommands(n *projectNode) []string {
	if cfg.TestCommands != nil {
		return cfg.TestCommands(n.Project)
	}
	switch n.Kind {
	case kindGo:
		return []string{"go vet ./...", "go test ./..."}
	case kindCargo:
		if n.lock {
			return []string{"cargo test --offline --locked"}
		}
	case kindNode:
		if n.tests {
			return []string{"bun test"}
		}
	}
	return nil
}

// prepareSpec is a host command that warms a project's dependency cache.
type prepareSpec struct {
	argv []string
	env  []string
}

// prepareSpecFor returns the prepare command for n. A nil spec with an empty skip means nothing to do;
// a non-empty skip explains why a step was not run.
func (cfg DockerPresubmit) prepareSpecFor(g *projectGraph, n *projectNode) (*prepareSpec, string) {
	switch n.Kind {
	case kindGo:
		return &prepareSpec{
			argv: []string{"go", "mod", "download"},
			env: []string{
				"GOFLAGS=-mod=readonly",
				"GOPROXY=https://proxy.golang.org",
				"GOMODCACHE=" + filepath.Join(cfg.CacheDir, "gomod"),
				"GOWORK=off",
				"GOTOOLCHAIN=local",
			},
		}, ""
	case kindNode:
		if !n.lock {
			return nil, ""
		}
		return &prepareSpec{
			argv: []string{"bun", "install", "--ignore-scripts", "--frozen-lockfile"},
			env:  []string{"BUN_INSTALL_CACHE_DIR=" + filepath.Join(cfg.CacheDir, "bun")},
		}, ""
	case kindCargo:
		if !n.lock {
			return nil, ""
		}
		if hasCargoConfig(g, n.Dir) {
			return nil, "cargo fetch skipped for " + n.Dir + ": repository cargo config present"
		}
		return &prepareSpec{
			argv: []string{"cargo", "fetch", "--locked"},
			env:  []string{"CARGO_HOME=" + filepath.Join(cfg.CacheDir, "cargo")},
		}, ""
	}
	return nil, ""
}

// prepare runs n's dependency warm-up on the host. It never executes package code. A missing tool is
// skipped and recorded; a failing step fails n.
func (cfg DockerPresubmit) prepare(ctx context.Context, g *projectGraph, n *projectNode, src, home string, r *presubmitReport) error {
	spec, skip := cfg.prepareSpecFor(g, n)
	if skip != "" {
		r.note(skip)
	}
	if spec == nil {
		return nil
	}
	label := strings.Join(spec.argv, " ") + " in " + n.Dir
	if _, err := exec.LookPath(spec.argv[0]); err != nil {
		r.note("skipped " + label + ": " + spec.argv[0] + " not found")
		return nil
	}
	env := inheritedEnv(prepareEnvNames, append(spec.env, "HOME="+home)...)
	reason, tail, err := runCommand(ctx, cfg.Timeout, filepath.Join(src, n.Dir), spec.argv, env)
	if err != nil {
		return err
	}
	if reason != "" {
		r.fail(n.Project, "prepare "+label+": "+reason, tail)
	}
	return nil
}

// runSandbox runs cmds for p in a fresh container. It returns a failure reason ("" on success) and the
// tail of the output. The error is non-nil only when the lander's own context ended.
func (cfg DockerPresubmit) runSandbox(ctx context.Context, src string, p Project, cmds []string) (string, []byte, error) {
	name, err := sandboxName()
	if err != nil {
		return "", nil, err
	}
	args := sandboxArgs(cfg, name, src, cfg.CacheDir, p, containerScript(p, cmds))
	reason, tail, err := runCommand(ctx, cfg.Timeout, "", append([]string{cfg.Docker}, args...), inheritedEnv(dockerEnvNames))
	if reason != "" || err != nil {
		// The docker client may have been killed while the container kept running.
		cfg.removeContainer(name)
	}
	return reason, tail, err
}

// removeContainer force-removes name, ignoring errors: the container may already be gone.
func (cfg DockerPresubmit) removeContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.Docker, "rm", "-f", name)
	cmd.Env = inheritedEnv(dockerEnvNames)
	_ = cmd.Run()
}

// sandboxArgs returns the docker run arguments (without the docker binary) that run script in one
// container for project p. The container has no network, a read-only root, no capabilities, and a
// fixed environment. Its only mounts are the read-only source archive and the read-only cache.
func sandboxArgs(cfg DockerPresubmit, name, srcDir, cacheDir string, p Project, script string) []string {
	args := []string{
		"run", "--rm", "--pull=never",
		"--name", name,
		"--label", "lander.kind=" + p.Kind,
		"--label", "lander.dir=" + p.Dir,
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(cfg.PidsLimit),
		"--memory", cfg.Memory,
		"--memory-swap", cfg.Memory,
		"--cpus", cfg.CPUs,
		"--user", sandboxUser,
		"--tmpfs", "/tmp:rw,exec,size=1g",
		"--tmpfs", sandboxWork + ":rw,exec,size=2g",
		"-v", srcDir + ":" + sandboxSrc + ":ro",
		"-v", cacheDir + ":" + sandboxCache + ":ro",
	}
	for _, kv := range sandboxEnv {
		args = append(args, "-e", kv)
	}
	return append(args, "--entrypoint", "sh", cfg.Image, "-c", script)
}

// containerScript is the shell program run in the sandbox: copy the read-only sources into the
// writable work tree, enter the project, and run its commands.
func containerScript(p Project, cmds []string) string {
	// cp -R, not -a: attributes cannot be preserved onto the root-owned tmpfs as uid 65534, and -a
	// prints warnings for every run. Symlinks are copied as links.
	steps := []string{"cp -R " + sandboxSrc + "/. " + sandboxWork}
	if p.Kind == kindCargo {
		steps = append(steps,
			"mkdir -p /tmp/cargo",
			"if [ -d "+sandboxCache+"/cargo ]; then cp -R "+sandboxCache+"/cargo/. /tmp/cargo/; fi")
	}
	steps = append(steps, "cd "+shellQuote(sandboxWork+"/"+p.Dir))
	steps = append(steps, cmds...)
	return strings.Join(steps, " && ")
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sandboxName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "lander-presubmit-" + hex.EncodeToString(b[:]), nil
}

// presubmitReport accumulates the verdict and the Details of one run.
type presubmitReport struct {
	failed    []string        // failing project directories, in run order
	failedIDs map[string]bool // failing project ids, for dependents
	passed    int
	noTests   int
	details   strings.Builder
}

func (r *presubmitReport) note(msg string) {
	fmt.Fprintf(&r.details, "note: %s\n", msg)
}

// fail records that p failed for reason, keeping the tail of its output in Details.
func (r *presubmitReport) fail(p Project, reason string, tail []byte) {
	if r.failedIDs == nil {
		r.failedIDs = map[string]bool{}
	}
	r.failedIDs[p.Kind+":"+p.Dir] = true
	r.failed = append(r.failed, p.Dir)
	fmt.Fprintf(&r.details, "== %s [%s]: %s\n%s\n", p.Dir, p.Kind, reason, tailText(tail))
}

// result converts the report into a verdict.
func (r *presubmitReport) result() PresubmitResult {
	if len(r.failed) > 0 {
		dirs := slices.Clone(r.failed)
		slices.Sort(dirs)
		dirs = slices.Compact(dirs)
		return PresubmitResult{
			Passed:  false,
			Summary: truncateRunes("FAILED: "+strings.Join(dirs, ", "), summaryRunes),
			Details: r.details.String(),
		}
	}
	var summary string
	switch {
	case r.passed == 0 && r.noTests > 0:
		summary = "no tests declared"
	case r.noTests > 0:
		summary = fmt.Sprintf("%s passed, %d with no tests declared", projectWord(r.passed), r.noTests)
	default:
		summary = projectWord(r.passed) + " passed"
	}
	return PresubmitResult{Passed: true, Summary: truncateRunes(summary, summaryRunes), Details: r.details.String()}
}

func projectWord(n int) string {
	if n == 1 {
		return "1 project"
	}
	return fmt.Sprintf("%d projects", n)
}

// failedVerdict is a failing result whose Details carry the reason.
func failedVerdict(summary, details string) PresubmitResult {
	return PresubmitResult{Passed: false, Summary: summary, Details: details}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	if len(t.buf) > t.max {
		return t.buf[len(t.buf)-t.max:]
	}
	return t.buf
}

// tailText renders a tail, dropping a partial leading rune.
func tailText(b []byte) string {
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return string(b)
}

// runCommand runs argv in dir with exactly env, bounded by timeout. reason is "" on success; otherwise
// it says how the command failed and tail holds the last output. err is non-nil only when ctx ended.
func runCommand(ctx context.Context, timeout time.Duration, dir string, argv, env []string) (reason string, tail []byte, err error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &tailBuffer{max: logTailBytes}
	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = time.Second
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	switch {
	case runErr == nil:
		return "", nil, nil
	case errors.Is(rctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("timed out after %s", timeout), out.Bytes(), nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return fmt.Sprintf("exit %d", ee.ExitCode()), out.Bytes(), nil
	}
	return runErr.Error(), out.Bytes(), nil
}

// inheritedEnv returns the variables named in names that are set in the lander's environment, followed
// by extra. The result is never nil, so the child never inherits the whole environment.
func inheritedEnv(names []string, extra ...string) []string {
	env := []string{}
	for _, k := range names {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

// exportProjects writes the closure's directories, plus root go.work and go.mod when present, from
// tree into dst. The archive is then checked against the tree listing, so an export-ignore attribute
// cannot silently remove a file from the candidate.
func exportProjects(ctx context.Context, s *Store, tree string, g *projectGraph, closure []*projectNode, dst string) error {
	roots := map[string]bool{}
	for _, n := range closure {
		roots[n.Dir] = true
	}
	for _, f := range []string{"go.work", "go.mod"} {
		if g.paths[f] {
			roots[f] = true
		}
	}
	wholeTree := roots["."]

	want := map[string]bool{}
	for p := range g.paths {
		if wholeTree || covered(p, roots) {
			want[p] = true
		}
	}

	args := []string{"archive", "--format=tar", tree}
	if !wholeTree {
		args = append(args, "--")
		for _, d := range slices.Sorted(maps.Keys(roots)) {
			args = append(args, ":(literal)"+d)
		}
	}
	// git archive writes zero padding at the end of a tar stream, so the trimming done by Store.Git
	// never touches entry data.
	archive, err := s.Git(ctx, args...)
	if err != nil {
		return fmt.Errorf("archive %s: %w", tree, err)
	}
	got, err := extractTar(strings.NewReader(archive), dst)
	if err != nil {
		return err
	}
	var missing []string
	for p := range want {
		if !got[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("archive omitted %d tree file(s), first %s (export-ignore?)", len(missing), missing[0])
	}
	if len(got) != len(want) {
		return fmt.Errorf("archive holds %d files, tree selection has %d", len(got), len(want))
	}
	return nil
}

// covered reports whether p lies at or under one of roots.
func covered(p string, roots map[string]bool) bool {
	for r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// extractTar unpacks a tar stream into dst, which must be empty. It accepts only directories, regular
// files, and symlinks. It rejects names that are absolute or contain "..", symlinks whose target leaves
// dst, and any entry whose parent path passes through a symlink, so nothing is written outside dst.
// It returns the set of non-directory paths it created.
func extractTar(r io.Reader, dst string) (map[string]bool, error) {
	files := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		comps, err := tarComponents(hdr.Name)
		if err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if _, err := ensureDirs(dst, comps); err != nil {
				return nil, err
			}
			continue
		case tar.TypeReg, tar.TypeSymlink:
		default:
			return nil, fmt.Errorf("archive: %s: unsupported entry type %q", hdr.Name, hdr.Typeflag)
		}
		if len(comps) == 0 {
			return nil, fmt.Errorf("archive: %q has no file name", hdr.Name)
		}
		dir, err := ensureDirs(dst, comps[:len(comps)-1])
		if err != nil {
			return nil, err
		}
		final := filepath.Join(dir, comps[len(comps)-1])
		if hdr.Typeflag == tar.TypeSymlink {
			if err := checkSymlink(comps, hdr.Linkname); err != nil {
				return nil, err
			}
			if err := os.Symlink(hdr.Linkname, final); err != nil {
				return nil, err
			}
		} else if err := writeRegular(final, hdr, tr); err != nil {
			return nil, err
		}
		files[strings.Join(comps, "/")] = true
	}
}

// tarComponents splits an entry name into path components, rejecting unsafe names.
func tarComponents(name string) ([]string, error) {
	if name == "" || path.IsAbs(name) || strings.ContainsRune(name, 0) {
		return nil, fmt.Errorf("archive: unsafe entry name %q", name)
	}
	var comps []string
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "", ".":
		case "..":
			return nil, fmt.Errorf("archive: entry %q escapes the archive root", name)
		default:
			comps = append(comps, c)
		}
	}
	return comps, nil
}

// ensureDirs creates each of comps under dst and returns the final directory. It refuses to pass
// through a symlink or a file; Lstat is used so symlinks are never followed.
func ensureDirs(dst string, comps []string) (string, error) {
	cur := dst
	for _, c := range comps {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case !fi.IsDir():
			return "", fmt.Errorf("archive: %s is a symlink or file, not a directory", c)
		}
	}
	return cur, nil
}

// checkSymlink accepts a symlink only when its relative target stays inside the archive root, judged
// lexically from the link's directory.
func checkSymlink(comps []string, target string) error {
	if target == "" || path.IsAbs(target) || strings.ContainsRune(target, 0) {
		return fmt.Errorf("archive: symlink %s -> %q leaves the archive root", strings.Join(comps, "/"), target)
	}
	resolved := path.Join(path.Join(comps[:len(comps)-1]...), target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("archive: symlink %s -> %q leaves the archive root", strings.Join(comps, "/"), target)
	}
	return nil
}

// writeRegular creates final with the entry's content. O_EXCL refuses to follow or overwrite anything.
func writeRegular(final string, hdr *tar.Header, r io.Reader) error {
	mode := fs.FileMode(0o644)
	if hdr.Mode&0o111 != 0 {
		mode = 0o755
	}
	f, err := os.OpenFile(final, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
