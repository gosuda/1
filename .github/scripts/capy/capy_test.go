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

	writeRepoFiles(t, repo, map[string]string{
		"web/securl/main.go":        "package main\nimport \"gosuda.org/0/corp/lemonsso\"\n",
		"corp/lemonsso/sso.go":      "package lemonsso\nimport \"gosuda.org/0/experimental/alice/proto\"\n",
		"experimental/bob/exp.go":   "package exp\nimport \"gosuda.org/0/corp/lemonsso\"\nimport \"gosuda.org/0/net/ivnp\"\n",
		"third_party/zstd/zstd.go":  "package zstd\nimport \"gosuda.org/0/web/gosuda\"\n",
	})
	badTree := commitTree(t, repo)

	violations, err := capy.CheckVisibility(ctx, repo, badTree)
	if err != nil {
		t.Fatalf("CheckVisibility: %v", err)
	}
	if len(violations) != 4 {
		t.Fatalf("violations = %v, want 4 tier violations", violations)
	}

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
	if len(violations) != 0 {
		t.Fatalf("clean tree violations = %v, want empty", violations)
	}

	violations, err = capy.CheckVisibilityDiff(ctx, repo, cleanTree, badTree)
	if err != nil {
		t.Fatalf("CheckVisibilityDiff: %v", err)
	}
	if len(violations) != 4 {
		t.Fatalf("diff violations = %v, want 4 violations", violations)
	}
}

func TestCheckOwnersEnforcesHierarchicalRulesAndExperimentalSandbox(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	writeRepoFiles(t, repo, map[string]string{
		"OWNERS":                        "@rootadmin\n",
		"net/OWNERS":                    "@netlead\n",
		"net/ivnp/OWNERS":               "set noparent\n@alice\nper-file *.md=@docwriter\n",
		"net/ivnp/relay.go":             "package ivnp\n",
		"net/ivnp/README.md":            "# IVNP\n",
		"experimental/charlie/proto.go": "package proto\n",
	})
	baseTree := commitTree(t, repo)

	// 1. Unauthorized edit to net/ivnp/relay.go (set noparent blocks @netlead and @rootadmin)
	writeRepoFiles(t, repo, map[string]string{
		"net/ivnp/relay.go": "package ivnp\n// edit\n",
		"net/ivnp/OWNERS":   "set noparent\n@alice\n@mallory\n",
	})
	unauthTree := commitTree(t, repo)

	actors := capy.ExtractCommitActors("mallory <mallory@gosuda.org>", "net/ivnp: hack\n\nReviewed-by: @netlead\n")
	violations, err := capy.CheckOwners(ctx, repo, baseTree, unauthTree, actors)
	if err != nil {
		t.Fatalf("CheckOwners: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("violations = %v, want 2 violations (relay.go and OWNERS tamper attempt)", violations)
	}

	// 2. Approved via Reviewed-by: @alice
	approvedActors := capy.ExtractCommitActors("mallory <mallory@gosuda.org>", "net/ivnp: fix\n\nReviewed-by: @alice\n")
	violations, err = capy.CheckOwners(ctx, repo, baseTree, unauthTree, approvedActors)
	if err != nil {
		t.Fatalf("CheckOwners(approved): %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("approved violations = %v, want empty", violations)
	}

	// 3. Per-file rule allows @docwriter to edit README.md and charlie to edit experimental/charlie/*
	writeRepoFiles(t, repo, map[string]string{
		"net/ivnp/relay.go":             "package ivnp\n",
		"net/ivnp/OWNERS":               "set noparent\n@alice\nper-file *.md=@docwriter\n",
		"net/ivnp/README.md":            "# IVNP updated\n",
		"experimental/charlie/proto.go": "package proto\n// sandbox\n",
	})
	docAndExpTree := commitTree(t, repo)

	docActors := capy.ExtractCommitActors("docwriter <docwriter@gosuda.org>", "docs + exp")
	violations, err = capy.CheckOwners(ctx, repo, baseTree, docAndExpTree, docActors)
	if err != nil {
		t.Fatalf("CheckOwners(doc+exp): %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("doc+exp violations = %v, want empty", violations)
	}
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
