package capy_test

import (
	"context"
	"testing"

	"gosuda.org/0/scripts/capy"
)

func localReplacesOf(t *testing.T, repo, base, tree string) []string {
	t.Helper()
	got, err := capy.CheckLocalReplaces(context.Background(), repo, base, tree)
	if err != nil {
		t.Fatalf("replace check: %v", err)
	}
	return got
}

func TestLocalReplaceLeavingTheModuleDirIsRejected(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"ipc/hqq/HQQ/go.mod", "module gosuda.org/hqq\n",
	)
	base := commitTree(t, repo)
	writeTree(t, repo,
		"ipc/hqq/dep/go.mod", "module gosuda.org/dep\n\nrequire gosuda.org/hqq v0.7.0\n\nreplace gosuda.org/hqq => ../HQQ\n",
		"ipc/hqq/nested/mod/go.mod", "module gosuda.org/nested\n\nreplace (\n\tgosuda.org/hqq v0.7.0 => ../../HQQ\n\texample.org/abs => /opt/abs\n)\n",
		"net/inner/go.mod", "module gosuda.org/inner\n\nreplace example.org/vendored => ./third_party/vendored\n\nreplace example.org/up => ./sub/../other\n",
		"net/remote/go.mod", "module gosuda.org/remote\n\nreplace example.org/fork => github.com/me/fork v1.0.0\n",
	)
	tree := commitTree(t, repo)

	want := []string{
		"ipc/hqq/dep/go.mod:5: local replace leaves the module directory: gosuda.org/hqq => ../HQQ (remove it; the root go.work resolves sibling modules locally)",
		"ipc/hqq/nested/mod/go.mod:4: local replace leaves the module directory: gosuda.org/hqq => ../../HQQ (remove it; the root go.work resolves sibling modules locally)",
		"ipc/hqq/nested/mod/go.mod:5: local replace leaves the module directory: example.org/abs => /opt/abs (remove it; the root go.work resolves sibling modules locally)",
	}
	wantViolations(t, localReplacesOf(t, repo, base, tree), want...)
	wantViolations(t, localReplacesOf(t, repo, "", tree), want...)
}

func TestLocalReplaceThatAlreadyExistsInTheBaseIsGrandfathered(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"ipc/hqq/dep/go.mod", "module gosuda.org/dep\n\nreplace gosuda.org/hqq => ../HQQ\n",
	)
	base := commitTree(t, repo)

	// Touching the file for another reason keeps the old replace legal; adding a second one is not.
	writeTree(t, repo, "ipc/hqq/dep/go.mod", "module gosuda.org/dep\n\ngo 1.24\n\nreplace gosuda.org/hqq => ../HQQ\n\nreplace gosuda.org/other => ../other\n")
	tree := commitTree(t, repo)
	wantViolations(t, localReplacesOf(t, repo, base, tree),
		"ipc/hqq/dep/go.mod:7: local replace leaves the module directory: gosuda.org/other => ../other (remove it; the root go.work resolves sibling modules locally)",
	)

	// Re-pointing the same module to a different directory is a new replace.
	writeTree(t, repo, "ipc/hqq/dep/go.mod", "module gosuda.org/dep\n\nreplace gosuda.org/hqq => ../elsewhere\n")
	moved := commitTree(t, repo)
	wantViolations(t, localReplacesOf(t, repo, base, moved),
		"ipc/hqq/dep/go.mod:3: local replace leaves the module directory: gosuda.org/hqq => ../elsewhere (remove it; the root go.work resolves sibling modules locally)",
	)

	// Removing it is always fine.
	writeTree(t, repo, "ipc/hqq/dep/go.mod", "module gosuda.org/dep\n")
	clean := commitTree(t, repo)
	wantViolations(t, localReplacesOf(t, repo, base, clean))
}

func TestLocalReplaceInControlPlaneIsExempt(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		".github/scripts/lander/go.mod", "module gosuda.org/0/scripts/lander\n\nreplace gosuda.org/0/scripts/capy => ../capy\n",
	)
	tree := commitTree(t, repo)
	wantViolations(t, localReplacesOf(t, repo, "", tree))
}
