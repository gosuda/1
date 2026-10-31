package capy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLandScriptMergesDivergedWorkspaceAndEnforcesVisibility(t *testing.T) {
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	landScript := filepath.Join(repoRoot, ".github", "scripts", "land.sh")
	capyDir := filepath.Join(repoRoot, ".github", "scripts", "capy")

	tmp := t.TempDir()
	bareOrigin := filepath.Join(tmp, "origin.git")
	runCmd(t, tmp, "git", "init", "-q", "--bare", bareOrigin)

	work := filepath.Join(tmp, "work")
	runCmd(t, tmp, "git", "init", "-q", "-b", "main", work)
	runCmd(t, work, "git", "config", "user.name", "Test")
	runCmd(t, work, "git", "config", "user.email", "test@example.invalid")
	runCmd(t, work, "git", "remote", "add", "origin", bareOrigin)

	if err := os.MkdirAll(filepath.Join(work, ".github", "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir .github/scripts: %v", err)
	}
	if err := os.Symlink(capyDir, filepath.Join(work, ".github", "scripts", "capy")); err != nil {
		t.Fatalf("symlink capy: %v", err)
	}

	writeRepoFiles(t, work, map[string]string{
		"README.md": "Tree at [Browse `//` on Magpie](https://magpie.gosuda.org/tree)\n",
	})
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-qm", "init control plane")
	runGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	runGit(t, work, "rm", "-q", "README.md")

	writeRepoFiles(t, work, map[string]string{
		"web/securl/main.go":    "package main\n// line 1\n// line 2\n// line 3\n// line 4\n// line 5\n// line 6\n// line 7\n// line 8\n// line 9\n// line 10\n",
		"net/portalite/conn.go": "package portalite\nconst Port = 443\n",
		"OWNERS":                "@maintainer\n",
	})
	// corp/lemonsso is a vanity-path module, so bad-vis below resolves its import through this go.mod.
	writeRepoFiles(t, work, map[string]string{"corp/lemonsso/go.mod": "module gosuda.org/lemonsso\n\ngo 1.27\n"})
	tree0 := commitTree(t, work)
	snap0 := runGit(t, work, "commit-tree", tree0, "-m", "initial trunk")
	runGit(t, work, "push", "-q", "origin", snap0+":refs/suda/main", snap0+":refs/suda/history")

	// Branch workspace from snap0 with two commits.
	runGit(t, work, "switch", "-q", "-c", "ws/agent/task-1", snap0)
	writeRepoFiles(t, work, map[string]string{
		"web/securl/main.go": "package main\n// WS EDIT\n// line 2\n// line 3\n// line 4\n// line 5\n// line 6\n// line 7\n// line 8\n// line 9\n// line 10\n",
	})
	runGit(t, work, "commit", "-qam", "ws commit 1")
	writeRepoFiles(t, work, map[string]string{
		"net/portalite/extra.go": "package portalite\n",
	})
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.name=Workspace Author", "-c", "user.email=author@gosuda.org", "commit", "-qm", "web/securl: update header and add portalite file")
	runGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/land/task-1")

	// Advance refs/suda/main concurrently on a non-overlapping line.
	runGit(t, work, "switch", "-q", "--detach", snap0)
	writeRepoFiles(t, work, map[string]string{
		"web/securl/main.go": "package main\n// line 1\n// line 2\n// line 3\n// line 4\n// line 5\n// line 6\n// line 7\n// line 8\n// line 9\n// MAIN EDIT\n",
	})
	tree1 := commitTree(t, work)
	snap1 := runGit(t, work, "commit-tree", tree1, "-m", "concurrent trunk update")
	hist1 := runGit(t, work, "commit-tree", tree1, "-p", snap0, "-m", "concurrent trunk update")
	runGit(t, work, "push", "-q", "--force", "origin", snap1+":refs/suda/main", hist1+":refs/suda/history")

	// Execute land.sh for refs/heads/land/task-1.
	runCmdEnv(t, work, []string{"SUDA_REQUESTER=maintainer"}, landScript, "refs/heads/land/task-1")

	mergedMain := runGit(t, bareOrigin, "rev-parse", "refs/suda/main")
	mergedHist := runGit(t, bareOrigin, "rev-parse", "refs/suda/history")
	if gotParents := runGit(t, bareOrigin, "rev-list", "--count", mergedMain); gotParents != "1" {
		t.Fatalf("refs/suda/main commit count = %s, want 1 (orphan snapshot)", gotParents)
	}
	if gotHistCount := runGit(t, bareOrigin, "rev-list", "--count", mergedHist); gotHistCount != "3" {
		t.Fatalf("refs/suda/history commit count = %s, want 3", gotHistCount)
	}
	if runGit(t, bareOrigin, "rev-parse", mergedMain+"^{tree}") != runGit(t, bareOrigin, "rev-parse", mergedHist+"^{tree}") {
		t.Fatal("refs/suda/main and refs/suda/history root trees differ")
	}
	if gotAuthor := runGit(t, bareOrigin, "log", "-1", "--format=%an <%ae>", mergedMain); gotAuthor != "Workspace Author <author@gosuda.org>" {
		t.Fatalf("merged author = %q, want %q", gotAuthor, "Workspace Author <author@gosuda.org>")
	}

	mergedContent := readTreeBlob(t, bareOrigin, mergedMain+"^{tree}", "web/securl/main.go")
	if !strings.Contains(mergedContent, "// WS EDIT") || !strings.Contains(mergedContent, "// MAIN EDIT") {
		t.Fatalf("merged web/securl/main.go missing expected edits:\n%s", mergedContent)
	}

	ctrlMain := runGit(t, bareOrigin, "rev-parse", "refs/heads/main")
	updatedReadme := readTreeBlob(t, bareOrigin, ctrlMain+"^{tree}", "README.md")
	if !strings.Contains(updatedReadme, "https://magpie.gosuda.org/tree") {
		t.Fatalf("README.md on refs/heads/main missing permanent Magpie link:\n%s", updatedReadme)
	}
	syncedReadme := readTreeBlob(t, bareOrigin, mergedMain+"^{tree}", "README.md")
	if syncedReadme != updatedReadme {
		t.Fatalf("README.md on refs/suda/main not synchronized from refs/heads/main: got %q, want %q", syncedReadme, updatedReadme)
	}

	landRefs := runGit(t, bareOrigin, "for-each-ref", "refs/heads/land/")
	if landRefs != "" {
		t.Fatalf("refs/heads/land/ not deleted after landing: %s", landRefs)
	}

	// Attempt to land a workspace that violates public -> corp visibility.
	runGit(t, work, "fetch", "-q", "origin", "+refs/suda/main:refs/remotes/origin/suda/main")
	runGit(t, work, "switch", "-q", "-c", "ws/agent/bad-vis", "refs/remotes/origin/suda/main")
	writeRepoFiles(t, work, map[string]string{
		"web/securl/leak.go": "package main\nimport \"gosuda.org/lemonsso\"\n",
	})
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "bad import")
	runGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/land/bad-vis")

	cmd := exec.Command(landScript, "refs/heads/land/bad-vis")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "SUDA_REQUESTER=maintainer")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("land.sh succeeded on visibility violation, want non-zero exit:\n%s", out)
	}
	const wantViolation = "web/securl/leak.go:2: prohibited import in public domain: gosuda.org/lemonsso -> corp"
	if !strings.Contains(string(out), wantViolation) {
		t.Fatalf("land.sh rejected the landing without the visibility violation %q:\n%s", wantViolation, out)
	}
	if afterMain := runGit(t, bareOrigin, "rev-parse", "refs/suda/main"); afterMain != mergedMain {
		t.Fatalf("refs/suda/main advanced on rejected landing: got %s, want %s", afterMain, mergedMain)
	}

	// Add a strict OWNERS file on trunk and verify land.sh rejects unauthorized edits unless Reviewed-by is present.
	runGit(t, work, "switch", "-q", "-c", "ws/alice/add-owners", "refs/remotes/origin/suda/main")
	writeRepoFiles(t, work, map[string]string{
		"net/portalite/OWNERS": "set noparent\n@alice\n",
	})
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.name=alice", "-c", "user.email=alice@gosuda.org", "commit", "-qm", "net/portalite: add OWNERS")
	runGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/land/add-owners")
	runCmdEnv(t, work, []string{"SUDA_REQUESTER=maintainer"}, landScript, "refs/heads/land/add-owners")
	ownersMain := runGit(t, bareOrigin, "rev-parse", "refs/suda/main")

	// Unauthorized attempt by mallory without Reviewed-by: @alice
	runGit(t, work, "fetch", "-q", "origin", "+refs/suda/main:refs/remotes/origin/suda/main")
	runGit(t, work, "switch", "-q", "-c", "ws/mallory/unauth", "refs/remotes/origin/suda/main")
	writeRepoFiles(t, work, map[string]string{
		"net/portalite/conn.go": "package portalite\nconst Port = 8443\n",
	})
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.name=mallory", "-c", "user.email=mallory@gosuda.org", "commit", "-qm", "net/portalite: unauthorized change")
	runGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/land/unauth")

	unauthCmd := exec.Command(landScript, "refs/heads/land/unauth")
	unauthCmd.Dir = work
	unauthCmd.Env = append(os.Environ(), "SUDA_REQUESTER=mallory")
	if out, err := unauthCmd.CombinedOutput(); err == nil {
		t.Fatalf("land.sh succeeded on unauthorized OWNERS edit, want non-zero exit:\n%s", out)
	}
	if afterOwners := runGit(t, bareOrigin, "rev-parse", "refs/suda/main"); afterOwners != ownersMain {
		t.Fatalf("refs/suda/main advanced on rejected OWNERS landing: got %s, want %s", afterOwners, ownersMain)
	}

	// A self-written trailer and a spoofed commit author must not approve anything.
	runGit(t, work, "-c", "user.name=alice", "-c", "user.email=alice@gosuda.org", "commit", "--amend", "-qm", "net/portalite: forged\n\nReviewed-by: @alice")
	runGit(t, work, "push", "-q", "--force", "origin", "HEAD:refs/heads/land/unauth")
	forgedCmd := exec.Command(landScript, "refs/heads/land/unauth")
	forgedCmd.Dir = work
	forgedCmd.Env = append(os.Environ(), "SUDA_REQUESTER=mallory")
	if out, err := forgedCmd.CombinedOutput(); err == nil {
		t.Fatalf("land.sh accepted a forged Reviewed-by trailer and author:\n%s", out)
	}

	// Approval bound to the authenticated approver (alice) lands the change.
	runCmdEnv(t, work, []string{"SUDA_REQUESTER=mallory", "SUDA_APPROVERS=alice"}, landScript, "refs/heads/land/unauth")
	if finalMain := runGit(t, bareOrigin, "rev-parse", "refs/suda/main"); finalMain == ownersMain {
		t.Fatal("refs/suda/main did not advance after Reviewed-by approval")
	}
}

func runCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	return runCmdEnv(t, dir, nil, name, args...)
}

func runCmdEnv(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v (%s)", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
