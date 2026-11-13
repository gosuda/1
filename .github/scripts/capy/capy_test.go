package capy_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gosuda.org/0/scripts/capy"
)

func TestLoadConfigRegistersPilotWorkflowsAndRejectsEmptyPrefix(t *testing.T) {
	cfg, err := capy.LoadConfig("../../../capy.star")
	if err != nil {
		t.Fatalf("LoadConfig(../../../capy.star): %v", err)
	}
	if got := len(cfg.Workflows); got < 9 {
		t.Fatalf("len(Workflows) = %d, want >= 9", got)
	}

	wf, ok := cfg.FindWorkflow("hqq-sqlite3")
	if !ok {
		t.Fatal("FindWorkflow(hqq-sqlite3) = false, want true")
	}
	if wf.OriginPrefix != "ipc/hqq/hqq-sqlite3" {
		t.Fatalf("OriginPrefix = %q, want ipc/hqq/hqq-sqlite3", wf.OriginPrefix)
	}
	if wf.DestinationRepo != "gosuda/hqq-sqlite3" {
		t.Fatalf("DestinationRepo = %q, want gosuda/hqq-sqlite3", wf.DestinationRepo)
	}

	badStar := filepath.Join(t.TempDir(), "bad.star")
	if err := os.WriteFile(badStar, []byte(`
load("//defs.star", "capy")
capy.workflow(
    name = "bad",
    origin = capy.monorepo_path("/"),
    destination = capy.github_repo("gosuda/bad"),
)
`), 0o600); err != nil {
		t.Fatalf("write bad.star: %v", err)
	}
	if _, err := capy.LoadConfig(badStar); err == nil {
		t.Fatal("LoadConfig(bad.star) = nil, want empty prefix error")
	}
}

func TestTransformSubtreeExcludesPatternsAndRewritesManifests(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	writeRepoFiles(t, repo, map[string]string{
		"ipc/hqq/sub/go.mod":                    "module gosuda.org/sub\n\ngo 1.24\n\nrequire gosuda.org/hqq v0.1.0\n\nreplace gosuda.org/hqq => ../HQQ\n",
		"ipc/hqq/sub/Cargo.toml":                "[package]\nname = \"sub\"\nversion = \"0.1.0\"\n",
		"ipc/hqq/sub/README.md":                 "Install from gosuda/0/ipc/hqq/sub\n",
		"ipc/hqq/sub/internal_notes/secret.txt": "do not export\n",
		"ipc/hqq/sub/dev.local.toml":            "local = true\n",
	})
	rootTree := commitTree(t, repo)

	starFile := filepath.Join(t.TempDir(), "capy.star")
	if err := os.WriteFile(starFile, []byte(`
load("//defs.star", "capy", "core", "gomod", "cargo")
capy.workflow(
    name = "sub",
    origin = capy.monorepo_path("ipc/hqq/sub"),
    destination = capy.github_repo("gosuda/sub"),
    transformations = [
        core.exclude(["internal_notes/**", "**.local.toml"]),
        gomod.strip_local_replaces(),
        cargo.standalone_workspace(),
        core.replace(before = "gosuda/0/ipc/hqq/sub", after = "gosuda/sub", paths = ["README.md"]),
    ],
)
`), 0o600); err != nil {
		t.Fatalf("write capy.star: %v", err)
	}

	cfg, err := capy.LoadConfig(starFile)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	wf, _ := cfg.FindWorkflow("sub")

	exportedTree, err := capy.TransformSubtree(ctx, repo, rootTree, wf)
	if err != nil {
		t.Fatalf("TransformSubtree: %v", err)
	}

	files := listTreePaths(t, repo, exportedTree)
	if got := strings.Join(files, ","); got != "Cargo.toml,README.md,go.mod" {
		t.Fatalf("exported files = %q, want Cargo.toml,README.md,go.mod", got)
	}

	gomodBody := readTreeBlob(t, repo, exportedTree, "go.mod")
	if strings.Contains(gomodBody, "replace ") {
		t.Fatalf("go.mod still contains local replace directive:\n%s", gomodBody)
	}

	cargoBody := readTreeBlob(t, repo, exportedTree, "Cargo.toml")
	if !strings.Contains(cargoBody, "[workspace]") {
		t.Fatalf("Cargo.toml missing [workspace] section:\n%s", cargoBody)
	}

	readmeBody := readTreeBlob(t, repo, exportedTree, "README.md")
	if got, want := readmeBody, "Install from gosuda/sub\n"; got != want {
		t.Fatalf("README.md = %q, want %q", got, want)
	}
}

func TestTransformSubtreeReturnsOriginalOIDWhenUnmodified(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	writeRepoFiles(t, repo, map[string]string{
		"net/portalite/relay.go": "package portalite\n",
	})
	rootTree := commitTree(t, repo)
	origSubtree := runGit(t, repo, "rev-parse", rootTree+":net/portalite")

	cfg, err := capy.LoadConfig("../../../capy.star")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	wf, ok := cfg.FindWorkflow("portalite")
	if !ok {
		t.Fatal("missing portalite workflow")
	}

	gotTree, err := capy.TransformSubtree(ctx, repo, rootTree, wf)
	if err != nil {
		t.Fatalf("TransformSubtree: %v", err)
	}
	if gotTree != origSubtree {
		t.Fatalf("TransformSubtree = %s, want identical subtree OID %s", gotTree, origSubtree)
	}
}

func TestSpliceSubtreeReplacesTargetPrefixWithoutMutatingSiblings(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	writeRepoFiles(t, repo, map[string]string{
		"web/gosuda/main.go":  "package main\n",
		"net/portalite/v1.go": "package portalite\n",
	})
	baseTree := commitTree(t, repo)
	origWebTree := runGit(t, repo, "rev-parse", baseTree+":web/gosuda")

	extRepo := initTestRepo(t)
	writeRepoFiles(t, extRepo, map[string]string{
		"v2.go": "package portalite\nconst Version = 2\n",
	})
	extTree := commitTree(t, extRepo)
	runGit(t, repo, "fetch", extRepo, "HEAD")

	newRoot, err := capy.SpliceSubtree(ctx, repo, baseTree, "net/portalite", extTree)
	if err != nil {
		t.Fatalf("SpliceSubtree: %v", err)
	}

	if gotWeb := runGit(t, repo, "rev-parse", newRoot+":web/gosuda"); gotWeb != origWebTree {
		t.Fatalf("web/gosuda subtree changed: got %s, want %s", gotWeb, origWebTree)
	}
	if gotPortal := runGit(t, repo, "rev-parse", newRoot+":net/portalite"); gotPortal != extTree {
		t.Fatalf("net/portalite subtree = %s, want %s", gotPortal, extTree)
	}
}

func TestCheckVisibilityDetectsProhibitedCorpAndExperimentalImports(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	// The root go.mod makes gosuda.org/0/... resolve to the tree; without it these imports are external.
	writeRepoFiles(t, repo, map[string]string{"go.mod": "module gosuda.org/0\n"})
	writeRepoFiles(t, repo, map[string]string{
		"web/securl/main.go":       "package main\nimport \"gosuda.org/0/corp/lemonsso\"\n",
		"corp/lemonsso/sso.go":     "package lemonsso\nimport \"gosuda.org/0/experimental/alice/proto\"\n",
		"experimental/bob/exp.go":  "package exp\nimport \"gosuda.org/0/corp/lemonsso\"\nimport \"gosuda.org/0/net/ivnp\"\n",
		"third_party/zstd/zstd.go": "package zstd\nimport \"gosuda.org/0/web/gosuda\"\n",
	})
	badTree := commitTree(t, repo)
	wantBad := []string{
		"corp/lemonsso/sso.go:2: prohibited import in corp domain: gosuda.org/0/experimental/alice/proto -> experimental",
		"experimental/bob/exp.go:2: prohibited import in experimental domain: gosuda.org/0/corp/lemonsso -> corp",
		"third_party/zstd/zstd.go:2: prohibited import in third_party domain: gosuda.org/0/web/gosuda -> public",
		"web/securl/main.go:2: prohibited import in public domain: gosuda.org/0/corp/lemonsso -> corp",
	}

	violations, err := capy.CheckVisibility(ctx, repo, badTree)
	if err != nil {
		t.Fatalf("CheckVisibility: %v", err)
	}
	wantViolations(t, violations, wantBad...)

	writeRepoFiles(t, repo, map[string]string{
		"web/securl/main.go":       "package main\n",
		"corp/lemonsso/sso.go":     "package lemonsso\nimport \"gosuda.org/0/web/gosuda\"\n",
		"experimental/bob/exp.go":  "package exp\nimport \"gosuda.org/0/net/ivnp\"\n",
		"third_party/zstd/zstd.go": "package zstd\n",
	})
	cleanTree := commitTree(t, repo)
	violations, err = capy.CheckVisibility(ctx, repo, cleanTree)
	if err != nil {
		t.Fatalf("CheckVisibility(clean): %v", err)
	}
	wantViolations(t, violations)

	violations, err = capy.CheckVisibilityDiff(ctx, repo, cleanTree, badTree)
	if err != nil {
		t.Fatalf("CheckVisibilityDiff: %v", err)
	}
	wantViolations(t, violations, wantBad...)
}

func checkOwners(t *testing.T, repo, base, tree string, requester string, approvers ...string) []string {
	t.Helper()
	v, err := capy.CheckOwners(context.Background(), repo, base, tree, capy.Identity{Requester: requester, Approvers: approvers},
		func(team, login string) (bool, error) { return team == "gosuda/core" && login == "carol", nil })
	if err != nil {
		t.Fatalf("CheckOwners: %v", err)
	}
	return v
}

func TestCheckOwnersEnforcesHierarchyAndApprovals(t *testing.T) {
	repo := initTestRepo(t)
	writeRepoFiles(t, repo, map[string]string{
		"OWNERS":                        "@rootadmin\n",
		"net/OWNERS":                    "@netlead\n",
		"net/ivnp/OWNERS":               "set noparent\n@alice\nper-file *.md=@docwriter\n",
		"net/ivnp/relay.go":             "package ivnp\n",
		"net/ivnp/README.md":            "# IVNP\n",
		"experimental/charlie/proto.go": "package proto\n",
	})
	base := commitTree(t, repo)

	// set noparent blocks @netlead and @rootadmin; editing OWNERS itself is also gated.
	writeRepoFiles(t, repo, map[string]string{
		"net/ivnp/relay.go": "package ivnp\n// edit\n",
		"net/ivnp/OWNERS":   "set noparent\n@alice\n@mallory\n",
	})
	edit := commitTree(t, repo)
	if v := checkOwners(t, repo, base, edit, "mallory", "netlead"); len(v) != 2 {
		t.Fatalf("violations = %v, want 2 (relay.go, OWNERS)", v)
	}
	if v := checkOwners(t, repo, base, edit, "mallory", "alice"); len(v) != 0 {
		t.Fatalf("approved violations = %v, want none", v)
	}

	// Per-file rule and the experimental sandbox of the requester.
	writeRepoFiles(t, repo, map[string]string{
		"net/ivnp/relay.go":             "package ivnp\n",
		"net/ivnp/OWNERS":               "set noparent\n@alice\nper-file *.md=@docwriter\n",
		"net/ivnp/README.md":            "# IVNP updated\n",
		"experimental/charlie/proto.go": "package proto\n// sandbox\n",
	})
	docExp := commitTree(t, repo)
	if v := checkOwners(t, repo, base, docExp, "docwriter"); len(v) != 1 || !strings.HasPrefix(v[0], "experimental/charlie/proto.go") {
		t.Fatalf("docwriter violations = %v, want only the experimental/charlie path", v)
	}
	if v := checkOwners(t, repo, base, docExp, "docwriter", "charlie"); len(v) != 1 {
		t.Fatalf("approver charlie must not own experimental/charlie unless requester: %v", v)
	}
}

// An organization admin passes noparent and require-review without being an owner or having an
// approver, but a path no OWNERS file covers stays rejected.
func TestCheckOwnersAdminBypassesApprovalsButNotUncoveredPaths(t *testing.T) {
	repo := initTestRepo(t)
	writeRepoFiles(t, repo, map[string]string{
		"net/ivnp/OWNERS":   "set noparent\nset require-review\n@alice\n",
		"net/ivnp/relay.go": "package ivnp\n",
		"orphan/o.go":       "package orphan\n",
	})
	base := commitTree(t, repo)
	writeRepoFiles(t, repo, map[string]string{
		"net/ivnp/relay.go": "package ivnp\n// edit\n",
		"orphan/o.go":       "package orphan\n// edit\n",
	})
	tree := commitTree(t, repo)
	check := func(id capy.Identity) []string {
		t.Helper()
		v, err := capy.CheckOwners(context.Background(), repo, base, tree, id, nil)
		if err != nil {
			t.Fatalf("CheckOwners: %v", err)
		}
		return v
	}

	if v := check(capy.Identity{Requester: "boss"}); len(v) != 2 {
		t.Fatalf("a non-admin non-owner must be rejected on both paths: %v", v)
	}
	if v := check(capy.Identity{Requester: "alice"}); len(v) != 2 {
		t.Fatalf("require-review must still bind a non-admin owner: %v", v)
	}
	wantViolations(t, check(capy.Identity{Requester: "boss", Admin: true}), "orphan/o.go: no OWNERS entry covers this path")
	wantViolations(t, check(capy.Identity{Admin: true}),
		"net/ivnp/relay.go: missing OWNERS approval (requester=\"\" approvers=[], required one of [alice])",
		"orphan/o.go: no OWNERS entry covers this path")
}

// With the authors of a change known, set require-review needs an owner approver outside the authors:
// a reviewer who merged counts, an author who approved does not, and authors own nothing.
func TestCheckOwnersRequireReviewExcludesAuthorsInsteadOfTheRequester(t *testing.T) {
	repo := initTestRepo(t)
	writeRepoFiles(t, repo, map[string]string{
		"net/ivnp/OWNERS":   "set noparent\nset require-review\n@alice\n@bob\n",
		"net/ivnp/relay.go": "package ivnp\n",
	})
	base := commitTree(t, repo)
	writeRepoFiles(t, repo, map[string]string{"net/ivnp/relay.go": "package ivnp\n// edit\n"})
	tree := commitTree(t, repo)
	check := func(id capy.Identity) []string {
		t.Helper()
		v, err := capy.CheckOwners(context.Background(), repo, base, tree, id, nil)
		if err != nil {
			t.Fatalf("CheckOwners: %v", err)
		}
		return v
	}

	// alice pushed, bob approved and merged.
	if v := check(capy.Identity{Requester: "bob", Approvers: []string{"bob"}, Authors: []string{"alice"}}); len(v) != 0 {
		t.Fatalf("a reviewer who merged someone else's change must count: %v", v)
	}
	if v := check(capy.Identity{Requester: "bob", Approvers: []string{"bob"}}); len(v) != 1 {
		t.Fatalf("without known authors the merger's own approval must not count: %v", v)
	}
	// bob pushed a fix-up onto alice's branch before approving it.
	wantViolations(t, check(capy.Identity{Requester: "bob", Approvers: []string{"bob"}, Authors: []string{"alice", "bob"}}),
		"net/ivnp/relay.go: requires review by an owner other than the authors [alice bob] (required one of [alice bob])")
	// A non-owner merger with an author who is an owner still needs an owner principal.
	if v := check(capy.Identity{Requester: "mallory", Authors: []string{"alice"}}); len(v) != 1 {
		t.Fatalf("authors must not own the change: %v", v)
	}
}

// A bot login granted one generated file by a per-file rule in that file's own directory may change that
// file and nothing else, and only when it is the authenticated requester.
func TestCheckOwnersPerFileBotGrantCoversOnlyItsFile(t *testing.T) {
	repo := initTestRepo(t)
	writeRepoFiles(t, repo, map[string]string{
		"OWNERS":                          "@rootadmin\n",
		"web/gosuda/zdata/OWNERS":         "per-file data.json.zstd=@github-actions[bot]\n",
		"web/gosuda/zdata/data.json.zstd": "v1",
		"web/gosuda/zdata/other.json":     "v1",
		"web/gosuda/data.json.zstd":       "v1",
		"web/gosuda/main.go":              "package main\n",
	})
	base := commitTree(t, repo)

	writeRepoFiles(t, repo, map[string]string{"web/gosuda/zdata/data.json.zstd": "v2"})
	data := commitTree(t, repo)
	if v := checkOwners(t, repo, base, data, "github-actions[bot]"); len(v) != 0 {
		t.Fatalf("the bot must own its generated file: %v", v)
	}
	if v := checkOwners(t, repo, base, data, "mallory"); len(v) != 1 {
		t.Fatalf("others must not gain the grant: %v", v)
	}

	writeRepoFiles(t, repo, map[string]string{
		"web/gosuda/zdata/other.json": "v2",
		"web/gosuda/data.json.zstd":   "v2",
		"web/gosuda/main.go":          "package main\n// x\n",
	})
	other := commitTree(t, repo)
	if v := checkOwners(t, repo, base, other, "github-actions[bot]"); len(v) != 3 {
		t.Fatalf("the bot must not own anything else, not even a same-named file above the directory: %v", v)
	}
}

// Each case is a bypass that the commit-derived identity model accepted.
func TestCheckOwnersRejectsForgeryAndSelfApproval(t *testing.T) {
	repo := initTestRepo(t)
	writeRepoFiles(t, repo, map[string]string{
		"OWNERS":                "@rootadmin\n@gosuda/core\n",
		"net/ivnp/OWNERS":       "set noparent\n@alice\n",
		"net/ivnp/relay.go":     "package ivnp\n",
		"experimental/bob/x.go": "package x\n",
		"corp/OWNERS":           "set require-review\n@dave\n@erin\n",
		"corp/svc/a.go":         "package svc\n",
	})
	base := commitTree(t, repo)
	baseCommit := runGit(t, repo, "rev-parse", "HEAD")
	reset := func() {
		runGit(t, repo, "reset", "-q", "--hard", baseCommit)
		runGit(t, repo, "clean", "-fdq")
	}

	t.Run("new OWNERS cannot approve its own directory", func(t *testing.T) {
		reset()
		writeRepoFiles(t, repo, map[string]string{
			"net/ivnp/sub/OWNERS": "set noparent\n@mallory\n",
			"net/ivnp/sub/e.go":   "package sub\n",
		})
		tree := commitTree(t, repo)
		if v := checkOwners(t, repo, base, tree, "mallory"); len(v) != 2 {
			t.Fatalf("violations = %v, want 2", v)
		}
		if v := checkOwners(t, repo, base, tree, "alice"); len(v) != 0 {
			t.Fatalf("alice (nearest owner) violations = %v", v)
		}
	})
	t.Run("other users cannot edit someone else's sandbox", func(t *testing.T) {
		reset()
		writeRepoFiles(t, repo, map[string]string{"experimental/bob/x.go": "package x // evil\n"})
		tree := commitTree(t, repo)
		if v := checkOwners(t, repo, base, tree, "mallory"); len(v) != 1 {
			t.Fatalf("mallory violations = %v, want 1", v)
		}
		if v := checkOwners(t, repo, base, tree, "Bob"); len(v) != 0 {
			t.Fatalf("bob violations = %v, want none (case-insensitive login)", v)
		}
	})
	t.Run("wildcard and unresolved teams match nobody", func(t *testing.T) {
		reset()
		writeRepoFiles(t, repo, map[string]string{"web/new.go": "package web\n"})
		tree := commitTree(t, repo)
		// "*" style entries and non-members of @gosuda/core are rejected, members pass.
		if v := checkOwners(t, repo, base, tree, "mallory"); len(v) != 1 {
			t.Fatalf("non-member violations = %v, want 1", v)
		}
		if v := checkOwners(t, repo, base, tree, "carol"); len(v) != 0 {
			t.Fatalf("team member violations = %v, want none", v)
		}
	})
	t.Run("set require-review needs an owner other than the requester", func(t *testing.T) {
		reset()
		writeRepoFiles(t, repo, map[string]string{"corp/svc/a.go": "package svc // v2\n"})
		tree := commitTree(t, repo)
		if v := checkOwners(t, repo, base, tree, "dave"); len(v) != 1 {
			t.Fatalf("self-review violations = %v, want 1", v)
		}
		if v := checkOwners(t, repo, base, tree, "dave", "dave"); len(v) != 1 {
			t.Fatalf("self as approver violations = %v, want 1", v)
		}
		if v := checkOwners(t, repo, base, tree, "dave", "erin"); len(v) != 0 {
			t.Fatalf("reviewed violations = %v, want none", v)
		}
		if v := checkOwners(t, repo, base, tree, "mallory", "alice"); len(v) != 1 {
			t.Fatalf("non-owner approver violations = %v, want 1", v)
		}
	})
	t.Run("paths without any OWNERS entry fail closed", func(t *testing.T) {
		bare := initTestRepo(t)
		writeRepoFiles(t, bare, map[string]string{"a/b.go": "package a\n"})
		b := commitTree(t, bare)
		writeRepoFiles(t, bare, map[string]string{"a/b.go": "package a // x\n"})
		n := commitTree(t, bare)
		if v := checkOwners(t, bare, b, n, "anyone"); len(v) != 1 {
			t.Fatalf("violations = %v, want 1", v)
		}
	})
}

func TestFormatCommitMessageScrubsInternalLinesAndAppendsOriginTrailer(t *testing.T) {
	starFile := filepath.Join(t.TempDir(), "capy.star")
	if err := os.WriteFile(starFile, []byte(`
load("//defs.star", "capy", "metadata")
capy.workflow(
    name = "ivnp",
    origin = capy.monorepo_path("net/ivnp"),
    destination = capy.github_repo("gosuda/IVNP"),
    transformations = [
        metadata.scrubber("(?m)^INTERNAL-ONLY:.*\\n?"),
        metadata.expose_label("GitOrigin-RevId"),
    ],
)
`), 0o600); err != nil {
		t.Fatalf("write capy.star: %v", err)
	}
	cfg, err := capy.LoadConfig(starFile)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	wf, _ := cfg.FindWorkflow("ivnp")

	got := capy.FormatCommitMessage("net/ivnp: add ssu2 relay\nINTERNAL-ONLY: secret ticket 99\n", wf, "deadbeef1234")
	want := "net/ivnp: add ssu2 relay\n\nGitOrigin-RevId: deadbeef1234\n"
	if got != want {
		t.Fatalf("FormatCommitMessage = %q, want %q", got, want)
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	return dir
}

func writeRepoFiles(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

func commitTree(t *testing.T, repo string) string {
	t.Helper()
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "snapshot")
	return runGit(t, repo, "rev-parse", "HEAD^{tree}")
}

func listTreePaths(t *testing.T, repo string, treeOID string) []string {
	t.Helper()
	out := runGit(t, repo, "ls-tree", "-r", "--name-only", treeOID)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func readTreeBlob(t *testing.T, repo string, treeOID string, relPath string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "cat-file", "blob", treeOID+":"+relPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cat-file %s:%s: %v (%s)", treeOID, relPath, err, out)
	}
	return string(out)
}

func runGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
