package lander

import (
	"context"
	"fmt"
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

func TestOnlyChangedMirrorsAreReconciled(t *testing.T) {
	e := newEnv(t)
	seen := e.trackReconciles()
	e.cycle()
	if strings.Join(*seen, ",") != "p,q" {
		t.Fatalf("first cycle reconciles every mirror, got %v", *seen)
	}

	*seen = nil
	e.cycle()
	if len(*seen) != 0 {
		t.Fatalf("an idle cycle must not touch any mirror, got %v", *seen)
	}

	e.devWork("only-p", map[string]string{"P/a.go": "package p\n// a=7\n"}, "P: a=7")
	e.cycle()
	if strings.Join(*seen, ",") != "p" {
		t.Fatalf("a change in P must reconcile only p, got %v", *seen)
	}
	if !strings.Contains(e.mirrorFile("a.go"), "a=7") {
		t.Fatal("p was not exported")
	}

	*seen = nil
	e.cycle()
	if len(*seen) != 0 {
		t.Fatalf("a settled mirror must stay quiet, got %v", *seen)
	}
}

func TestSweepProbesFindMirrorChangesThatCameWithoutAWake(t *testing.T) {
	e := newEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.l.Now = func() time.Time { return now }
	e.l.MirrorInterval = time.Hour
	seen := e.trackReconciles()
	e.cycle()

	e.mirrorCommit(map[string]string{"b.go": "package p\n// b\n"}, "add b")
	e.forge.actors["gosuda/p|refs/heads/main"] = "alice"
	*seen = nil
	now = now.Add(5 * time.Minute)
	e.cycle()
	if len(*seen) != 0 || e.trunkHas("P/b.go") {
		t.Fatalf("no wake and no sweep yet: nothing may be reconciled (got %v)", *seen)
	}

	now = now.Add(time.Hour)
	e.cycle()
	if strings.Join(*seen, ",") != "p" {
		t.Fatalf("the sweep must find the change in p only, got %v", *seen)
	}
	if !e.trunkHas("P/b.go") {
		t.Fatal("the mirror change was not imported")
	}

	// The next sweep finds nothing to do.
	*seen = nil
	now = now.Add(2 * time.Hour)
	e.cycle()
	if len(*seen) != 0 {
		t.Fatalf("a sweep over unchanged mirrors must not reconcile, got %v", *seen)
	}
}

func TestWakeHintReconcilesOnlyTheNamedMirror(t *testing.T) {
	e := newEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.l.Now = func() time.Time { return now }
	e.l.MirrorInterval = 24 * time.Hour
	seen := e.trackReconciles()
	e.cycle()

	e.mirrorCommit(map[string]string{"c.go": "package p\n// c\n"}, "add c")
	e.forge.actors["gosuda/p|refs/heads/main"] = "alice"
	if !e.l.NoteWake("gosuda/p") || e.l.NoteWake("gosuda/unrelated") {
		t.Fatal("NoteWake must accept configured mirrors and ignore unrelated repositories")
	}
	*seen = nil
	now = now.Add(time.Minute)
	e.cycle()
	if strings.Join(*seen, ",") != "p" || !e.trunkHas("P/c.go") {
		t.Fatalf("wake for p: reconciled %v, imported=%v", *seen, e.trunkHas("P/c.go"))
	}
}

func TestUnsettledMirrorsAreRetriedOnATimerNotEveryCycle(t *testing.T) {
	e := newEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.l.Now = func() time.Time { return now }
	e.l.MirrorInterval = 24 * time.Hour
	seen := e.trackReconciles()
	e.cycle()

	// A non-owner pushes to the mirror: the change is staged and the mirror stays unsettled.
	e.mirrorCommit(map[string]string{"d.go": "package p\n// d\n"}, "add d")
	e.forge.actors["gosuda/p|refs/heads/main"] = "mallory"
	e.l.NoteWake("gosuda/p")
	*seen = nil
	now = now.Add(time.Minute)
	e.cycle()
	if strings.Join(*seen, ",") != "p" {
		t.Fatalf("first attempt: %v", *seen)
	}
	*seen = nil
	now = now.Add(30 * time.Second)
	e.cycle()
	if len(*seen) != 0 {
		t.Fatalf("an unsettled mirror must wait for its retry timer, got %v", *seen)
	}
	// Once the pusher is an owner the retry lands it.
	e.forge.actors["gosuda/p|refs/heads/main"] = "alice"
	now = now.Add(3 * time.Minute)
	e.cycle()
	if strings.Join(*seen, ",") != "p" || !e.trunkHas("P/d.go") {
		t.Fatalf("retry after the timer: reconciled %v, imported=%v", *seen, e.trunkHas("P/d.go"))
	}
}

// GitHub refuses ref updates whose tree contains a root .github/workflows unless the token has
// the workflows permission. Baselines must therefore never carry the export tree.
func TestBaselineWritesSurviveProjectsThatShipWorkflows(t *testing.T) {
	e := newEnv(t)
	hook := filepath.Join(e.trunk, "hooks", "pre-receive")
	script := "#!/bin/sh\nwhile read old new ref; do\n  case \"$ref\" in refs/suda/sync/*)\n    if git ls-tree -r \"$new\" | grep -q '	.github/workflows/'; then echo \"refusing to update workflow files\" >&2; exit 1; fi;;\n  esac\ndone\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.cycle() // bootstrap the baseline
	e.devWork("wf", map[string]string{"P/.github/workflows/ci.yml": "name: ci\n"}, "P: add CI")
	e.cycle()
	hist := e.trunkRef("refs/suda/history")
	msg := git(t, e.trunk, "log", "-1", "--format=%B", "refs/suda/sync/p")
	if !strings.Contains(msg, "Suda-Sync-Mono: "+hist) {
		t.Fatalf("baseline not written for a project with workflows:\n%s", msg)
	}
	if !e.mirrorHas(".github/workflows/ci.yml") {
		t.Fatal("workflow file must be exported to the mirror")
	}
}

func TestGarbageCollectKeepsEmbargoedReferencesUntilTheirOwnTTL(t *testing.T) {
	e := newEnv(t)
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	veryOld := time.Now().Add(-200 * 24 * time.Hour).Format(time.RFC3339)
	tip := e.devWork("tmp", map[string]string{"Q/q.go": "package q\n// gc\n"}, "gc")
	mk := func(date string) string {
		return sh(t, e.dev, []string{"GIT_COMMITTER_DATE=" + date, "GIT_AUTHOR_DATE=" + date}, "git", "commit-tree", tip+"^{tree}", "-p", tip, "-m", "old")
	}
	c30, c200 := mk(old), mk(veryOld)
	git(t, e.dev, "push", "-q", e.user,
		c30+":refs/ws/alice/plain", c30+":refs/ws/alice/sec/sec-202610-aaaaaaaa", c200+":refs/ws/alice/sec/sec-202601-bbbbbbbb",
		c30+":refs/heads/review/sec-202610-aaaaaaaa", c30+":refs/heads/base/sec-202610-aaaaaaaa",
		c30+":refs/heads/review/old-plain")
	git(t, e.dev, "push", "-q", e.user, ":refs/heads/land/tmp")
	e.l.WorkspaceTTL = 14 * 24 * time.Hour
	e.l.EmbargoTTL = 180 * 24 * time.Hour
	if err := e.l.GarbageCollect(e.ctx); err != nil {
		t.Fatal(err)
	}
	ws := strings.Join(e.userRefs("refs/ws/"), ",")
	if ws != "refs/ws/alice/sec/sec-202610-aaaaaaaa" {
		t.Fatalf("ws refs after GC = %s (plain pruned at 14d, embargoed kept until its own TTL)", ws)
	}
	heads := strings.Join(e.userRefs("refs/heads/"), ",")
	if !strings.Contains(heads, "review/sec-202610-aaaaaaaa") || !strings.Contains(heads, "base/sec-202610-aaaaaaaa") || strings.Contains(heads, "old-plain") {
		t.Fatalf("heads after GC = %s", heads)
	}
}

func TestLandedMirrorPullRequestsAreCommentedAndClosed(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	dir := filepath.Join(e.root, "pr-dev")
	git(t, e.root, "clone", "-q", "-b", "main", e.mirror, dir)

	pr := func(n int, file string) string {
		git(t, dir, "checkout", "-q", "-B", "pr", "origin/main")
		write(t, dir, map[string]string{file: "package p\n"})
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-qm", "feature "+file)
		head := git(t, dir, "rev-parse", "HEAD")
		git(t, dir, "push", "-q", "-f", "origin", fmt.Sprintf("%s:refs/pull/%d/head", head, n))
		return head
	}
	head7 := pr(7, "c7.go")
	head8 := pr(8, "c8.go")
	e.forge.pulls["gosuda/p"] = []PullRequest{{Number: 7, HeadRef: "f7", HeadSHA: head7}, {Number: 8, HeadRef: "f8", HeadSHA: head8}}
	e.l.NoteWake("gosuda/p")
	e.cycle()
	if len(e.forge.closed) != 0 {
		t.Fatalf("staging must not close anything: %v", e.forge.closed)
	}

	// A member reviews and lands the staged change of PR 7 only.
	git(t, e.dev, "fetch", "-q", e.user, "refs/ws/capy/p-pr-7")
	git(t, e.dev, "push", "-q", e.user, "FETCH_HEAD:refs/heads/land/pr7")
	e.cycle()
	if !e.trunkHas("P/c7.go") || !e.mirrorHas("c7.go") {
		t.Fatal("the staged change did not land and export")
	}
	if len(e.forge.closed) != 1 || !strings.HasPrefix(e.forge.closed[0], "gosuda/p#7: ") {
		t.Fatalf("exactly PR 7 must be closed, got %v", e.forge.closed)
	}
	if c := e.forge.closed[0]; !strings.Contains(c, "landed in the monorepo") || !strings.Contains(c, "`main`") {
		t.Fatalf("comment text: %q", c)
	}
	if strings.Contains(e.forge.closed[0], "Suda-") {
		t.Fatal("internal trailers must not appear in the comment")
	}
	e.cycle()
	if len(e.forge.closed) != 1 {
		t.Fatalf("closing must not repeat: %v", e.forge.closed)
	}

	// New commits on a landed pull request give it a different head: it is not closed.
	head7b := pr(7, "c7b.go")
	e.forge.pulls["gosuda/p"] = append(e.forge.pulls["gosuda/p"], PullRequest{Number: 7, HeadRef: "f7", HeadSHA: head7b})
	e.l.NoteWake("gosuda/p")
	e.cycle()
	if len(e.forge.closed) != 1 {
		t.Fatalf("a pull request with new commits must stay open: %v", e.forge.closed)
	}
}

func TestStagedPullRequestHoldsOnlyItsOwnChangeWhenTheMirrorMovedOn(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	dir := filepath.Join(e.root, "pr-dev")
	git(t, e.root, "clone", "-q", "-b", "main", e.mirror, dir)
	write(t, dir, map[string]string{"c7.go": "package p\n"})
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "feature c7")
	head := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "push", "-q", "-f", "origin", head+":refs/pull/7/head")

	// The mirror moves on after the pull request branched: a new file, and a changed one.
	e.mirrorCommit(map[string]string{"later.go": "package p\n// later\n"}, "later work")
	e.forge.pulls["gosuda/p"] = []PullRequest{{Number: 7, HeadRef: "f7", HeadSHA: head}}
	e.l.NoteWake("gosuda/p")
	e.cycle()
	e.cycle()
	if !e.trunkHas("P/later.go") {
		t.Fatal("the later mirror commit did not reach the trunk")
	}

	git(t, e.dev, "fetch", "-q", e.user, "refs/ws/capy/p-pr-7")
	staged := git(t, e.dev, "rev-parse", "FETCH_HEAD")
	changes := git(t, e.dev, "diff", "--name-status", staged+"~1", staged)
	if strings.TrimSpace(changes) != "A\tP/c7.go" {
		t.Fatalf("the staged change must be the pull request's own delta, got:\n%s", changes)
	}
	if msg := git(t, e.dev, "log", "-1", "--format=%B", staged); !strings.Contains(msg, "Suda-Staged-On: ") {
		t.Fatalf("staged commit must record the mirror state it was replayed onto:\n%s", msg)
	}

	// Unchanged state is not staged again; a new mirror commit restages on the new state.
	e.cycle()
	git(t, e.dev, "fetch", "-q", e.user, "refs/ws/capy/p-pr-7")
	if got := git(t, e.dev, "rev-parse", "FETCH_HEAD"); got != staged {
		t.Fatalf("restaged without a state change: %s != %s", got, staged)
	}
	e.mirrorCommit(map[string]string{"later2.go": "package p\n"}, "even later work")
	e.l.NoteWake("gosuda/p")
	e.cycle()
	e.cycle()
	git(t, e.dev, "fetch", "-q", e.user, "refs/ws/capy/p-pr-7")
	restaged := git(t, e.dev, "rev-parse", "FETCH_HEAD")
	if restaged == staged {
		t.Fatal("a mirror that moved on must restage the open pull request")
	}
	if got := strings.TrimSpace(git(t, e.dev, "diff", "--name-status", restaged+"~1", restaged)); got != "A\tP/c7.go" {
		t.Fatalf("restaged change = %q", got)
	}
}

func TestStagedPullRequestThatConflictsWithTheMirrorIsNotStaged(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	dir := filepath.Join(e.root, "pr-dev")
	git(t, e.root, "clone", "-q", "-b", "main", e.mirror, dir)
	write(t, dir, map[string]string{"a.go": "package p\n// from the pull request\n"})
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "feature")
	head := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "push", "-q", "-f", "origin", head+":refs/pull/9/head")
	e.mirrorCommit(map[string]string{"a.go": "package p\n// from main\n"}, "conflicting main change")
	e.forge.pulls["gosuda/p"] = []PullRequest{{Number: 9, HeadRef: "f9", HeadSHA: head}}
	e.l.NoteWake("gosuda/p")
	e.cycle()
	e.cycle()
	if refs := e.userRefs("refs/ws/capy/"); len(refs) != 0 {
		t.Fatalf("a conflicting pull request must not be staged: %v", refs)
	}
}

func TestForgedInboundTrailersInMirrorCommitsAreStripped(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	head := e.mirrorCommit(map[string]string{"x.go": "package p\n"}, "evil\n\nSuda-Inbound-From: gosuda/p#8@"+strings.Repeat("a", 40))
	e.forge.actors["gosuda/p|refs/heads/main"] = "alice"
	e.l.NoteWake("gosuda/p")
	e.cycle()
	if !e.trunkHas("P/x.go") {
		t.Fatal("the owner's mirror push did not import")
	}
	msg := git(t, e.trunk, "log", "-1", "--format=%B", "refs/suda/history")
	if strings.Contains(msg, "#8@"+strings.Repeat("a", 40)) {
		t.Fatalf("a forged trailer survived into the trunk:\n%s", msg)
	}
	_ = head
}
