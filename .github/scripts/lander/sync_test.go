package lander

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (e *env) mirrorCommit(files map[string]string, msg string) string {
	t := e.t
	dir := filepath.Join(e.root, "mirror-dev")
	os.RemoveAll(dir)
	git(t, e.root, "clone", "-q", "-b", "main", e.mirror, dir)
	write(t, dir, files)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", msg)
	git(t, dir, "push", "-q", "origin", "HEAD:refs/heads/main")
	return git(t, dir, "rev-parse", "HEAD")
}

func (e *env) mirrorFile(path string) string {
	return git(e.t, e.mirror, "cat-file", "blob", "refs/heads/main:"+path)
}
func (e *env) mirrorHas(path string) bool {
	return gitOK(e.mirror, "cat-file", "-e", "refs/heads/main:"+path)
}

func TestReconcileBootstrapsBaselineWhenMirrorMatchesExport(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	msg := git(t, e.trunk, "log", "-1", "--format=%B", "refs/suda/sync/p")
	hist := e.trunkRef("refs/suda/history")
	if !strings.Contains(msg, "Suda-Sync-Mono: "+hist) || !strings.Contains(msg, "Suda-Sync-Mirror: "+e.trunkRefMirror()) {
		t.Fatalf("baseline message:\n%s", msg)
	}
}

func (e *env) trunkRefMirror() string { return git(e.t, e.mirror, "rev-parse", "refs/heads/main") }

func TestLandingExportsToMirrorWithoutExcludedFilesAndRecordsHistoryRev(t *testing.T) {
	e := newEnv(t)
	e.cycle() // baseline
	e.devWork("exp", map[string]string{"P/a.go": "package p\n// a=2\n", "P/dev.local.toml": "k=1\n", "P/internal_notes/more.md": "x\n"}, "P: bump a")
	e.cycle()

	if !strings.Contains(e.mirrorFile("a.go"), "a=2") {
		t.Fatalf("mirror a.go = %q", e.mirrorFile("a.go"))
	}
	if e.mirrorHas("dev.local.toml") || e.mirrorHas("internal_notes/n.md") || e.mirrorHas("internal_notes/more.md") {
		t.Fatal("excluded files leaked to the mirror")
	}
	hist := e.trunkRef("refs/suda/history")
	msg := git(t, e.mirror, "log", "-1", "--format=%B", "refs/heads/main")
	if !strings.Contains(msg, "GitOrigin-RevId: "+hist) {
		t.Fatalf("mirror commit must record the history commit %s:\n%s", hist, msg)
	}
	if strings.Contains(msg, "Suda-") {
		t.Fatalf("internal trailers leaked to the mirror:\n%s", msg)
	}
	base := git(t, e.trunk, "log", "-1", "--format=%B", "refs/suda/sync/p")
	if !strings.Contains(base, "Suda-Sync-Mono: "+hist) {
		t.Fatalf("baseline not advanced after export:\n%s", base)
	}
}

// Findings F4/F5: a landing whose export never happened and an external commit on the mirror.
func TestStaleMirrorPlusExternalCommitKeepsBothSides(t *testing.T) {
	e := newEnv(t)
	e.cycle() // baseline

	e.devWork("mono", map[string]string{"P/a.go": "package p\n// a=2\n"}, "P: a=2")
	if err := e.l.fetchTrunk(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.l.DrainLandRequests(e.ctx); err != nil { // lands, but no export (as after a failed export)
		t.Fatal(err)
	}
	if strings.Contains(e.mirrorFile("a.go"), "a=2") {
		t.Fatal("precondition: mirror must be stale")
	}
	ext := e.mirrorCommit(map[string]string{"b.go": "package p\n// b\n"}, "add b")
	e.forge.actors["gosuda/p|refs/heads/main"] = "alice"

	e.cycle()

	if !strings.Contains(e.mirrorFile("a.go"), "a=2") || !e.mirrorHas("b.go") {
		t.Fatal("mirror lost the monorepo change or the external file")
	}
	if !strings.Contains(e.trunkFile("refs/suda/main", "P/a.go"), "a=2") || !e.trunkHas("P/b.go") {
		t.Fatal("trunk lost the monorepo change or the imported file")
	}
	if !e.trunkHas("P/internal_notes/n.md") {
		t.Fatal("inbound import deleted export-excluded files")
	}
	if git(t, e.mirror, "merge-base", "--is-ancestor", ext, "refs/heads/main") != "" {
		t.Fatal("external commit must stay in the mirror history")
	}
	if msg := git(t, e.trunk, "log", "-1", "--format=%B", "refs/suda/sync/p"); !strings.Contains(msg, "Suda-Sync-Mirror: "+e.trunkRefMirror()) {
		t.Fatalf("baseline must point at the mirror tip after convergence:\n%s", msg)
	}
	// A further cycle is a no-op.
	tip, hist := e.trunkRefMirror(), e.trunkRef("refs/suda/history")
	e.cycle()
	if e.trunkRefMirror() != tip || e.trunkRef("refs/suda/history") != hist {
		t.Fatal("converged state must be stable")
	}
}

func TestInboundFromNonOwnerIsStagedAndBlocksExport(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.mirrorCommit(map[string]string{"b.go": "package p\n// b\n"}, "add b")
	e.forge.actors["gosuda/p|refs/heads/main"] = "mallory"
	e.devWork("mono", map[string]string{"P/a.go": "package p\n// a=2\n"}, "P: a=2")
	mirrorTip := e.trunkRefMirror()
	e.cycle()

	if e.trunkHas("P/b.go") {
		t.Fatal("non-owner mirror push landed in the trunk")
	}
	if e.trunkRefMirror() != mirrorTip {
		t.Fatal("export must not overwrite unreviewed mirror commits")
	}
	if refs := e.userRefs("refs/ws/capy/"); len(refs) != 1 || refs[0] != "refs/ws/capy/p-main" {
		t.Fatalf("staged refs = %v", refs)
	}
	// Staging is stable across cycles.
	staged := git(t, e.user, "rev-parse", "refs/ws/capy/p-main")
	e.cycle()
	if git(t, e.user, "rev-parse", "refs/ws/capy/p-main") != staged {
		t.Fatal("staged ref was rewritten without a mirror change")
	}
	// A member lands the staged change: the mirror then catches up.
	git(t, e.dev, "fetch", "-q", e.user, "refs/ws/capy/p-main")
	git(t, e.dev, "push", "-q", e.user, "FETCH_HEAD:refs/heads/land/staged")
	e.forge.actors["gosuda/1|refs/heads/land/staged"] = "alice"
	e.cycle()
	if !e.trunkHas("P/b.go") || !e.mirrorHas("b.go") || !strings.Contains(e.mirrorFile("a.go"), "a=2") {
		t.Fatal("member landing of the staged change did not converge both sides")
	}
}

func TestMirrorPullRequestsAreStagedAndPrunedWhenClosed(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	dir := filepath.Join(e.root, "pr-dev")
	git(t, e.root, "clone", "-q", "-b", "main", e.mirror, dir)
	write(t, dir, map[string]string{"c.go": "package p\n// c\n"})
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "feature c")
	head := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "push", "-q", "origin", head+":refs/pull/7/head")
	e.forge.pulls["gosuda/p"] = []PullRequest{{Number: 7, HeadRef: "feature", HeadSHA: head}}

	e.cycle()
	if refs := e.userRefs("refs/ws/capy/"); len(refs) != 1 || refs[0] != "refs/ws/capy/p-pr-7" {
		t.Fatalf("staged = %v", refs)
	}
	if e.trunkHas("P/c.go") {
		t.Fatal("PR staging must not land anything")
	}
	staged := git(t, e.user, "rev-parse", "refs/ws/capy/p-pr-7")
	if git(t, e.user, "cat-file", "-e", staged+":P/c.go") != "" {
		t.Fatal("staged tree lacks the PR file under the monorepo prefix")
	}
	if !strings.Contains(git(t, e.user, "log", "-1", "--format=%B", staged), "Suda-Inbound-From: gosuda/p#7@"+head) {
		t.Fatal("staged commit lacks provenance")
	}

	// The PR closes: garbage collection removes the staged ref.
	e.forge.pulls["gosuda/p"] = nil
	if err := e.l.GarbageCollect(e.ctx); err != nil {
		t.Fatal(err)
	}
	if refs := e.userRefs("refs/ws/capy/"); len(refs) != 0 {
		t.Fatalf("closed-PR staged ref survived GC: %v", refs)
	}
}

func TestExportFailureHealsOnNextCycle(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	good := e.l.Mirror
	e.l.Mirror = func(repo string) Remote { return Remote{URL: filepath.Join(e.root, "missing.git"), Repo: repo} }
	e.devWork("x", map[string]string{"P/a.go": "package p\n// a=3\n"}, "P: a=3")
	if err := e.l.Cycle(e.ctx); err == nil {
		t.Fatal("expected a reconcile error while the mirror is unreachable")
	}
	if !strings.Contains(e.trunkFile("refs/suda/main", "P/a.go"), "a=3") {
		t.Fatal("trunk landing must not depend on mirror availability")
	}
	e.l.Mirror = good
	e.cycle()
	if !strings.Contains(e.mirrorFile("a.go"), "a=3") {
		t.Fatal("mirror did not converge after the outage")
	}
}

func TestLostBaselineSelfHeals(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.devWork("x", map[string]string{"P/a.go": "package p\n// a=4\n"}, "P: a=4")
	e.cycle()
	hist := e.trunkRef("refs/suda/history")
	// Lose the baseline update after a successful export.
	git(t, e.trunk, "update-ref", "-d", "refs/suda/sync/p")
	e.cycle()
	if msg := git(t, e.trunk, "log", "-1", "--format=%B", "refs/suda/sync/p"); !strings.Contains(msg, "Suda-Sync-Mono: "+hist) {
		t.Fatalf("baseline not rebuilt:\n%s", msg)
	}
}

func TestGarbageCollectPrunesOnlyStaleWorkspacesAndClosedReviewPairs(t *testing.T) {
	e := newEnv(t)
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	tip := e.devWork("tmp", map[string]string{"Q/q.go": "package q\n// gc\n"}, "gc")
	oldCommit := sh(t, e.dev, []string{"GIT_COMMITTER_DATE=" + old, "GIT_AUTHOR_DATE=" + old}, "git", "commit-tree", tip+"^{tree}", "-p", tip, "-m", "old")
	git(t, e.dev, "push", "-q", e.user, oldCommit+":refs/ws/alice/old", tip+":refs/ws/alice/fresh",
		oldCommit+":refs/heads/review/closed", oldCommit+":refs/heads/base/closed", oldCommit+":refs/heads/review/open")
	git(t, e.dev, "push", "-q", e.user, ":refs/heads/land/tmp")
	e.forge.pulls["gosuda/1"] = []PullRequest{{Number: 1, HeadRef: "review/open"}}
	e.l.WorkspaceTTL = 14 * 24 * time.Hour

	if err := e.l.GarbageCollect(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.userRefs("refs/ws/"), ","); got != "refs/ws/alice/fresh" {
		t.Fatalf("ws refs = %s", got)
	}
	got := strings.Join(e.userRefs("refs/heads/"), ",")
	if !strings.Contains(got, "review/open") || strings.Contains(got, "review/closed") || strings.Contains(got, "base/closed") {
		t.Fatalf("heads after GC = %s", got)
	}
}

var _ = context.Background
