package lander

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/0/scripts/capy"
)

// env is a self-contained world: bare trunk, user and mirror repositories on disk, a developer
// clone, a fake forge and a Lander wired to them.
type env struct {
	t       *testing.T
	root    string
	trunk   string // bare
	user    string // bare
	mirror  string // bare, gosuda/p (branch main)
	mirrorQ string // bare, gosuda/q (branch main)
	dev     string // non-bare working clone used to author commits
	forge   *fakeForge
	l       *Lander
	ctx     context.Context
}

func sh(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.invalid",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.invalid")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func git(t *testing.T, dir string, args ...string) string { return sh(t, dir, nil, "git", args...) }

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, p)
		if c == "\x00delete" {
			_ = os.Remove(full)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const testCapyStar = `load("//.github/scripts/capy/defs.star", "capy", "core", "metadata")
capy.workflow(
    name = "p",
    origin = capy.monorepo_path("P"),
    destination = capy.github_repo("gosuda/p", branch = "main"),
    transformations = [
        core.exclude(["internal_notes/**", "**.local.toml"]),
        metadata.expose_label("GitOrigin-RevId"),
    ],
)
capy.workflow(
    name = "q",
    origin = capy.monorepo_path("Q"),
    destination = capy.github_repo("gosuda/q", branch = "main"),
    transformations = [metadata.expose_label("GitOrigin-RevId")],
)
`

type fakeForge struct {
	mu        sync.Mutex
	clock     int64
	actors    map[string]string           // repo|ref -> login
	chains    map[string][]capy.RefPush   // repo|ref -> every update of the ref, oldest first
	def       string                      // default actor
	reviews   map[string]capy.ReviewState // repo|head -> pull request and approvals
	mergedFor map[string]capy.MergedPull  // repo|merge sha -> merged pull request
	pullOK    map[string][]capy.Approval  // repo#number -> approvals of that pull request
	teams     map[string]map[string]bool
	admins    map[string]bool // organization admins
	statuses  []string
	pulls     map[string][]PullRequest
	merged    map[string][]MergedPull
	closed    []string // "repo#number: comment"
	created   []fakeCreated
	updated   []fakeCreated
	nextPull  int
}

func (f *fakeForge) PushForTip(_ context.Context, repo, ref, sha string) (capy.Push, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.actors[repo+"|"+ref]
	if !ok {
		a = f.def
	}
	if a == "" || a == "-" {
		return capy.Push{}, capy.ErrActorNotVisible
	}
	f.clock++
	return capy.Push{Actor: a, Time: time.Unix(1_700_000_000+f.clock, 0)}, nil
}
func (f *fakeForge) PushChain(_ context.Context, repo, ref, stop, tip string) ([]capy.RefPush, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.chains[repo+"|"+ref]
	var chain []capy.RefPush
	for cur := tip; cur != stop; {
		i := -1
		for j, p := range all {
			if p.After == cur {
				i = j
			}
		}
		if i < 0 || len(chain) >= len(all) {
			return nil, capy.ErrActorNotVisible
		}
		chain = append([]capy.RefPush{all[i]}, chain...)
		cur = all[i].Before
	}
	return chain, nil
}
func (f *fakeForge) ReviewApprovals(_ context.Context, repo, head string) (capy.ReviewState, error) {
	return f.reviews[repo+"|"+head], nil
}
func (f *fakeForge) MergedPullForCommit(_ context.Context, repo, mergeSHA string) (capy.MergedPull, bool, error) {
	pr, ok := f.mergedFor[repo+"|"+mergeSHA]
	return pr, ok, nil
}
func (f *fakeForge) PullApprovals(_ context.Context, repo string, number int) ([]capy.Approval, error) {
	return f.pullOK[fmt.Sprintf("%s#%d", repo, number)], nil
}
func (f *fakeForge) TeamResolver(context.Context) capy.TeamResolver {
	return func(team, login string) (bool, error) { return f.teams[team][login], nil }
}
func (f *fakeForge) OrgAdmin(_ context.Context, login string) (bool, error) {
	return f.admins[login], nil
}
func (f *fakeForge) SetStatus(_ context.Context, repo, sha string, st Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, fmt.Sprintf("%s %s %s %s", repo, st.Context, st.State, st.Description))
	return nil
}
func (f *fakeForge) OpenPulls(_ context.Context, repo string) ([]PullRequest, error) {
	return f.pulls[repo], nil
}

func (f *fakeForge) MergedPulls(_ context.Context, repo string) ([]MergedPull, error) {
	return f.merged[repo], nil
}

func (f *fakeForge) ClosePull(_ context.Context, repo string, number int, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, fmt.Sprintf("%s#%d: %s", repo, number, comment))
	var rest []PullRequest
	for _, p := range f.pulls[repo] {
		if p.Number != number {
			rest = append(rest, p)
		}
	}
	f.pulls[repo] = rest
	return nil
}

func (f *fakeForge) CreatePull(_ context.Context, repo, head, base, title, body string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPull++
	n := 1000 + f.nextPull
	f.pulls[repo] = append(f.pulls[repo], PullRequest{Number: n, HeadRef: head, Title: title})
	f.created = append(f.created, fakeCreated{Repo: repo, Number: n, Head: head, Base: base, Title: title, Body: body})
	return n, nil
}

func (f *fakeForge) UpdatePull(_ context.Context, repo string, number int, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, fakeCreated{Repo: repo, Number: number, Title: title, Body: body})
	for i, p := range f.pulls[repo] {
		if p.Number == number {
			f.pulls[repo][i].Title = title
		}
	}
	return nil
}

// fakeCreated records a pull request opened or updated through the fake forge.
type fakeCreated struct {
	Repo, Head, Base, Title, Body string
	Number                        int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root, ctx: context.Background(),
		trunk: filepath.Join(root, "trunk.git"), user: filepath.Join(root, "user.git"), mirror: filepath.Join(root, "p.git"), mirrorQ: filepath.Join(root, "q.git"),
		dev: filepath.Join(root, "dev")}
	for _, d := range []string{e.trunk, e.user, e.mirror, e.mirrorQ} {
		git(t, root, "init", "-q", "--bare", d)
	}
	git(t, e.user, "config", "uploadpack.allowFilter", "true")
	git(t, e.user, "config", "uploadpack.allowAnySHA1InWant", "true")
	git(t, root, "init", "-q", "-b", "main", e.dev)

	// Trunk: control plane overlay + sources, as one parentless snapshot.
	write(t, e.dev, map[string]string{
		"README.md":                         "control plane\n",
		"docs/user-repo/README.md":          "front page\n",
		"docs/user-repo/docs/onboarding.md": "onboarding\n",
		"capy.star":                         testCapyStar,
		".github/scripts/lander/Dockerfile": "FROM scratch\n",
		".github/scripts/lander/main.go":    "package main\n",
		".github/scripts/capy/capy.go":      "package capy\n",
	})
	git(t, e.dev, "add", "-A")
	git(t, e.dev, "commit", "-qm", "control plane")
	ctrl := git(t, e.dev, "rev-parse", "HEAD")
	write(t, e.dev, map[string]string{
		"OWNERS":                  "@maintainer\n@alice\n",
		"P/a.go":                  "package p\n// a=1\n",
		"P/internal_notes/n.md":   "secret\n",
		"P/OWNERS":                "set noparent\n@alice\n@maintainer\n",
		"Q/q.go":                  "package q\n",
		"R/r.go":                  "package r\n",
		"experimental/bob/exp.go": "package exp\n",
	})
	git(t, e.dev, "add", "-A")
	git(t, e.dev, "commit", "-qm", "initial trunk")
	tree := git(t, e.dev, "rev-parse", "HEAD^{tree}")
	snap := git(t, e.dev, "commit-tree", tree, "-m", "initial trunk")
	git(t, e.dev, "push", "-q", e.trunk, ctrl+":refs/heads/main", snap+":refs/suda/main", snap+":refs/suda/history")

	// Mirror main equals export(P).
	exp := git(t, e.dev, "rev-parse", snap+":P")
	_ = exp
	e.resetMirrorToExport(snap)
	e.seedMirrorQ(snap)

	e.forge = &fakeForge{def: "maintainer", actors: map[string]string{}, reviews: map[string]capy.ReviewState{},
		mergedFor: map[string]capy.MergedPull{}, pullOK: map[string][]capy.Approval{},
		teams: map[string]map[string]bool{}, pulls: map[string][]PullRequest{}}
	store := &Store{Dir: filepath.Join(root, "store.git")}
	cfg := Config{
		Store: store, Forge: e.forge,
		Trunk: Remote{URL: e.trunk, Repo: "gosuda/0"},
		User:  Remote{URL: e.user, Repo: "gosuda/1"},
		Mirror: func(repo string) Remote {
			if repo == "gosuda/q" {
				return Remote{URL: e.mirrorQ, Repo: repo}
			}
			return Remote{URL: e.mirror, Repo: repo}
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	if err := store.Init(e.ctx); err != nil {
		t.Fatal(err)
	}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.l = l
	return e
}

// resetMirrorToExport publishes P/ minus excluded files as the mirror's main (no baseline).
func (e *env) resetMirrorToExport(snap string) {
	t := e.t
	tmp := filepath.Join(e.root, "mirror-seed")
	os.RemoveAll(tmp)
	git(t, e.root, "init", "-q", "-b", "main", tmp)
	git(t, tmp, "fetch", "-q", e.trunk, snap)
	git(t, tmp, "read-tree", snap+":P")
	git(t, tmp, "rm", "-rq", "--cached", "--ignore-unmatch", "internal_notes")
	tree := git(t, tmp, "write-tree")
	c := git(t, tmp, "commit-tree", tree, "-m", "seed mirror")
	git(t, tmp, "push", "-q", "--force", e.mirror, c+":refs/heads/main")
}

// devWork creates a workspace commit on top of the current trunk snapshot and pushes it as a
// land ref in the user repository. It returns the tip.
func (e *env) devWork(id string, files map[string]string, msg string, extraEnv ...string) string {
	t := e.t
	git(t, e.dev, "fetch", "-q", e.trunk, "+refs/suda/main:refs/remotes/trunk/suda-main")
	git(t, e.dev, "checkout", "-q", "-B", "ws-"+id, "refs/remotes/trunk/suda-main")
	git(t, e.dev, "clean", "-fdxq")
	write(t, e.dev, files)
	git(t, e.dev, "add", "-A")
	sh(t, e.dev, extraEnv, "git", "commit", "-qm", msg)
	tip := git(t, e.dev, "rev-parse", "HEAD")
	git(t, e.dev, "push", "-q", "--force", e.user, tip+":refs/heads/land/"+id)
	return tip
}

func (e *env) trunkRef(ref string) string { return git(e.t, e.trunk, "rev-parse", "--verify", ref) }
func (e *env) trunkFile(ref, path string) string {
	return git(e.t, e.trunk, "cat-file", "blob", ref+":"+path)
}
func (e *env) trunkHas(path string) bool {
	return exec.Command("git", "-C", e.trunk, "cat-file", "-e", "refs/suda/main:"+path).Run() == nil
}
func (e *env) userRefs(prefix string) []string {
	out := git(e.t, e.user, "for-each-ref", "--format=%(refname)", prefix)
	if out == "" {
		return nil
	}
	l := strings.Split(out, "\n")
	sort.Strings(l)
	return l
}
func (e *env) cycle() {
	e.t.Helper()
	if err := e.l.Cycle(e.ctx); err != nil {
		e.t.Fatalf("Cycle: %v", err)
	}
}
func (e *env) hasStatus(sub string) bool {
	for _, s := range e.forge.statuses {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func gitOK(dir string, args ...string) bool {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Run() == nil
}

// seedMirrorQ publishes Q/ as the main branch of the second mirror.
func (e *env) seedMirrorQ(snap string) {
	t := e.t
	tmp := filepath.Join(e.root, "mirror-seed-q")
	os.RemoveAll(tmp)
	git(t, e.root, "init", "-q", "-b", "main", tmp)
	git(t, tmp, "fetch", "-q", e.trunk, snap)
	git(t, tmp, "read-tree", snap+":Q")
	tree := git(t, tmp, "write-tree")
	c := git(t, tmp, "commit-tree", tree, "-m", "seed mirror q")
	git(t, tmp, "push", "-q", "--force", e.mirrorQ, c+":refs/heads/main")
}

// trackReconciles records which workflows the lander reconciled.
func (e *env) trackReconciles() *[]string {
	var mu sync.Mutex
	var seen []string
	e.l.onReconcile = func(name string) {
		mu.Lock()
		seen = append(seen, name)
		mu.Unlock()
	}
	return &seen
}
