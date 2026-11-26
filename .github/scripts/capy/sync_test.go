package capy_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gosuda.org/0/scripts/capy"
)

func writeStar(t *testing.T, body string) capy.Workflow {
	t.Helper()
	p := filepath.Join(t.TempDir(), "capy.star")
	src := "load(\"//defs.star\", \"capy\", \"core\", \"gomod\", \"metadata\")\n" + body
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := capy.LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg.Workflows[0]
}

func treeFiles(t *testing.T, repo, tree string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range listTreePaths(t, repo, tree) {
		out[p] = readTreeBlob(t, repo, tree, p)
	}
	return out
}

func snapshotTree(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	runGit(t, repo, "rm", "-rfq", "--ignore-unmatch", ".")
	runGit(t, repo, "clean", "-fdxq")
	writeRepoFiles(t, repo, files)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "snap")
	return runGit(t, repo, "rev-parse", "HEAD^{tree}")
}

// Reproduces finding F4: the monorepo landed a change that never reached the mirror while the
// mirror gained an unrelated commit. A prefix swap reverts the monorepo change and drops excluded files.
func TestImportTreeKeepsMonorepoOnlyChangesThroughThreeWayMerge(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	wf := writeStar(t, `
capy.workflow(
    name = "p",
    origin = capy.monorepo_path("P"),
    destination = capy.github_repo("gosuda/p"),
    transformations = [core.exclude(["internal_notes/**", "**.local.toml"])],
)
`)
	s0 := snapshotTree(t, repo, map[string]string{"P/a.go": "a=1\n", "P/internal_notes/n.md": "secret\n", "P/x.local.toml": "k=1\n", "Q/q.go": "q=1\n"})
	s1 := snapshotTree(t, repo, map[string]string{"P/a.go": "a=2\n", "P/internal_notes/n.md": "secret\n", "P/x.local.toml": "k=1\n", "Q/q.go": "q=1\n"})

	mirror := snapshotTree(t, repo, map[string]string{"a.go": "a=1\n", "b.go": "b=1\n"})

	imp, err := capy.ImportTree(ctx, repo, s0, wf, mirror)
	if err != nil {
		t.Fatalf("ImportTree: %v", err)
	}
	// The import result alone still contains excluded files and the mirror addition.
	got := treeFiles(t, repo, imp)
	for _, want := range []string{"P/internal_notes/n.md", "P/x.local.toml", "P/b.go", "Q/q.go"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("import tree lacks %s: %v", want, got)
		}
	}

	merged := strings.Split(runGit(t, repo, "merge-tree", "--write-tree", "--merge-base="+s0, s1, imp), "\n")[0]
	files := treeFiles(t, repo, merged)
	if files["P/a.go"] != "a=2\n" || files["P/b.go"] != "b=1\n" || files["P/internal_notes/n.md"] != "secret\n" {
		t.Fatalf("merged tree wrong: %v", files)
	}
}

func TestImportTreeRefusesNonInvertibleWorkflows(t *testing.T) {
	repo := initTestRepo(t)
	wf := writeStar(t, `
capy.workflow(
    name = "p",
    origin = capy.monorepo_path("P"),
    destination = capy.github_repo("gosuda/p"),
    transformations = [gomod.strip_local_replaces()],
)
`)
	tree := snapshotTree(t, repo, map[string]string{"P/go.mod": "module x\n"})
	if _, err := capy.ImportTree(context.Background(), repo, tree, wf, tree); err == nil {
		t.Fatal("ImportTree must refuse workflows with content transforms")
	}
}

func TestNormalizeMirrorTreeIgnoresExcludedPathsForComparison(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	wf := writeStar(t, `
capy.workflow(
    name = "p",
    origin = capy.monorepo_path("P"),
    destination = capy.github_repo("gosuda/p"),
    transformations = [core.exclude(["**.local.toml"])],
)
`)
	mono := snapshotTree(t, repo, map[string]string{"P/a.go": "a\n", "P/dev.local.toml": "x\n"})
	mirror := snapshotTree(t, repo, map[string]string{"a.go": "a\n", "other.local.toml": "y\n"})
	exported, err := capy.TransformSubtree(ctx, repo, mono, wf)
	if err != nil {
		t.Fatal(err)
	}
	norm, err := capy.NormalizeMirrorTree(ctx, repo, mirror, wf)
	if err != nil {
		t.Fatal(err)
	}
	if exported != norm {
		t.Fatalf("export tree %s != normalized mirror %s", exported, norm)
	}
}

func TestBaselineMessageRoundTrip(t *testing.T) {
	in := capy.SyncBaseline{Workflow: "HQQ", Mono: strings.Repeat("a", 40), Mirror: strings.Repeat("b", 40)}
	out, err := capy.ParseBaselineMessage(capy.FormatBaselineMessage(in))
	if err != nil || out != in {
		t.Fatalf("round trip = %+v, %v; want %+v", out, err, in)
	}
	if _, err := capy.ParseBaselineMessage("nothing"); err == nil {
		t.Fatal("message without trailers must not parse")
	}
}
