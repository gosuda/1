package lander

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gosuda.org/0/scripts/capy"
)

var testChange = "I" + strings.Repeat("a", 40)

// devPatchset publishes a workspace tip with a Change-Id as git ws-pr does: review/<id> and
// base/<id>, plus (when publish is set) the immutable patchset ref refs/ws/cl/<change>/<tip>.
func (e *env) devPatchset(id, change string, files map[string]string, subject string, publish bool) string {
	tip := e.devReview(id, files, subject+"\n\nChange-Id: "+change)
	if publish {
		git(e.t, e.dev, "push", "-q", e.user, tip+":refs/ws/cl/"+change+"/"+tip)
	}
	return tip
}

// devRebase replays tip onto the current trunk snapshot, as git ws-sync does.
func (e *env) devRebase(tip string) string {
	t := e.t
	git(t, e.dev, "fetch", "-q", e.trunk, "+refs/suda/main:refs/remotes/trunk/suda-main")
	root := git(t, e.dev, "rev-list", "--max-parents=0", tip)
	git(t, e.dev, "checkout", "-q", "-B", "rebased", tip)
	git(t, e.dev, "rebase", "-q", "--onto", "refs/remotes/trunk/suda-main", root)
	return git(t, e.dev, "rev-parse", "HEAD")
}

// devAmend changes the working tree of the checked-out tip and amends it.
func (e *env) devAmend(files map[string]string) string {
	write(e.t, e.dev, files)
	git(e.t, e.dev, "add", "-A")
	git(e.t, e.dev, "commit", "-q", "--amend", "--no-edit")
	return git(e.t, e.dev, "rev-parse", "HEAD")
}

// pushRequest publishes tip as review/<id> and land/<id>, the state after git ws-pr and git ws-land
// of a rebased workspace.
func (e *env) pushRequest(id, tip string) {
	git(e.t, e.dev, "push", "-q", "--force", e.user, tip+":refs/heads/review/"+id, tip+":refs/heads/land/"+id)
}

// moveTrunk lands an unrelated change so that the snapshot a workspace was based on is outdated.
func (e *env) moveTrunk() {
	def := e.forge.def
	e.forge.def = "maintainer"
	e.devWork("unrelated", map[string]string{"P/b.go": "package p\n// b\n"}, "P: unrelated")
	e.cycle()
	e.forge.def = def
}

func (e *env) approvedBy(id string, pull int, login, commit string) {
	e.forge.reviews["gosuda/1|review/"+id] = capy.ReviewState{Pull: pull, Approvals: []capy.Approval{{Login: login, Commit: commit}}}
}

func TestApprovalCarriesToTheSameChangeRebasedOntoANewSnapshot(t *testing.T) {
	e := newEnv(t)
	e.forge.def = "mallory" // not an owner of P: only alice's approval can authorize the change
	ps1 := e.devPatchset("c1", testChange, map[string]string{"P/a.go": "package p\n// a=1\n// reviewed\n"}, "P: reviewed", true)
	e.approvedBy("c1", 12, "alice", ps1)

	e.moveTrunk()
	ps2 := e.devRebase(ps1)
	if ps2 == ps1 || git(t, e.dev, "rev-list", "--max-parents=0", ps2) == git(t, e.dev, "rev-list", "--max-parents=0", ps1) {
		t.Fatal("the rebase must produce a new tip on a new root snapshot")
	}
	e.pushRequest("c1", ps2) // review/c1 no longer holds the approved commit
	e.cycle()

	if got := e.trunkFile("refs/suda/main", "P/a.go"); !strings.Contains(got, "// reviewed") {
		t.Fatalf("rebased change did not land on its approval of the earlier patchset: %v", e.forge.statuses)
	}
	if !e.trunkHas("P/b.go") {
		t.Fatal("the unrelated landing must survive")
	}
	msg := git(t, e.trunk, "log", "-1", "--format=%B", "refs/suda/history")
	for _, want := range []string{"Change-Id: " + testChange, "Suda-Approved-By: alice", "Suda-Review: gosuda/1#12", "Suda-Requester: mallory"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("landed message lacks %q:\n%s", want, msg)
		}
	}
}

func TestApprovalDoesNotCarryToADifferentPatch(t *testing.T) {
	const reviewed = "package p\n// a=1\n// reviewed\n"
	cases := []struct {
		name  string
		tweak func(e *env) string // amends the rebased tip
	}{
		{"edited content", func(e *env) string {
			return e.devAmend(map[string]string{"P/a.go": reviewed + "// sneaked in\n"})
		}},
		{"whitespace only", func(e *env) string {
			return e.devAmend(map[string]string{"P/a.go": "package p\n// a=1\n// reviewed \n"})
		}},
		{"mode change", func(e *env) string {
			if err := os.Chmod(filepath.Join(e.dev, "P/a.go"), 0o755); err != nil {
				t.Fatal(err)
			}
			return e.devAmend(nil)
		}},
		{"extra file", func(e *env) string {
			return e.devAmend(map[string]string{"P/extra.go": "package p\n"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.forge.def = "mallory"
			ps1 := e.devPatchset("c1", testChange, map[string]string{"P/a.go": reviewed}, "P: reviewed", true)
			e.approvedBy("c1", 12, "alice", ps1)
			e.moveTrunk()
			e.devRebase(ps1)
			ps2 := c.tweak(e)
			e.pushRequest("c1", ps2)
			before := e.trunkRef("refs/suda/main")
			e.cycle()

			if e.trunkRef("refs/suda/main") != before {
				t.Fatalf("a patch that differs from the approved one landed")
			}
			if !e.hasStatus("suda/land failure OWNERS") {
				t.Fatalf("expected an OWNERS failure, got %v", e.forge.statuses)
			}
		})
	}
}

// restartLander gives the lander an empty object store, as after a restart on a fresh machine: it
// no longer holds any commit it saw in an earlier cycle.
func (e *env) restartLander() {
	if err := os.RemoveAll(e.l.Store.Dir); err != nil {
		e.t.Fatal(err)
	}
	if err := e.l.Store.Init(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func TestApprovalNeedsTheApprovedPatchsetToBeReadable(t *testing.T) {
	for _, published := range []bool{true, false} {
		name := "published patchset"
		if !published {
			name = "unpublished patchset"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.forge.def = "mallory"
			ps1 := e.devPatchset("c1", testChange, map[string]string{"P/a.go": "package p\n// a=1\n// reviewed\n"}, "P: reviewed", published)
			e.approvedBy("c1", 12, "alice", ps1)
			e.moveTrunk()
			ps2 := e.devRebase(ps1)
			e.pushRequest("c1", ps2) // moves review/c1: ps1 survives only as refs/ws/cl/<change>/<ps1>
			e.restartLander()
			before := e.trunkRef("refs/suda/main")
			e.cycle()

			landed := e.trunkRef("refs/suda/main") != before
			if landed != published {
				t.Fatalf("landed = %v with published = %v: %v", landed, published, e.forge.statuses)
			}
			if !published && !e.hasStatus("suda/land failure OWNERS") {
				t.Fatalf("an approval the lander cannot read must fail the OWNERS check: %v", e.forge.statuses)
			}
		})
	}
}

func TestApprovalIsFoundThroughTheChangeIdWhenTheWorkspaceIsRenamed(t *testing.T) {
	e := newEnv(t)
	e.forge.def = "mallory"
	ps1 := e.devPatchset("old-name", testChange, map[string]string{"P/a.go": "package p\n// a=1\n// reviewed\n"}, "P: reviewed", true)
	e.approvedBy("old-name", 21, "alice", ps1)
	e.moveTrunk()
	ps2 := e.devRebase(ps1)
	git(t, e.dev, "push", "-q", "--force", e.user, ps2+":refs/heads/land/new-name") // no review/new-name at all
	e.cycle()

	if !strings.Contains(e.trunkFile("refs/suda/main", "P/a.go"), "// reviewed") {
		t.Fatalf("approval on review/old-name did not reach land/new-name: %v", e.forge.statuses)
	}
	if msg := git(t, e.trunk, "log", "-1", "--format=%B", "refs/suda/history"); !strings.Contains(msg, "Suda-Review: gosuda/1#21") {
		t.Fatalf("landed message lacks the review number:\n%s", msg)
	}
}

func TestApprovalIsNotSharedWithAnotherChangeOfTheSameName(t *testing.T) {
	e := newEnv(t)
	e.forge.def = "mallory"
	ps1 := e.devPatchset("old-name", testChange, map[string]string{"P/a.go": "package p\n// a=1\n// reviewed\n"}, "P: reviewed", true)
	e.approvedBy("old-name", 21, "alice", ps1)
	// Another change with another Change-Id and another diff: no lookup, no approval.
	e.devWork("new-name", map[string]string{"P/a.go": "package p\n// a=1\n// other\n"}, "P: other\n\nChange-Id: I"+strings.Repeat("b", 40))
	before := e.trunkRef("refs/suda/main")
	e.cycle()
	if e.trunkRef("refs/suda/main") != before || !e.hasStatus("suda/land failure OWNERS") {
		t.Fatalf("a change with another Change-Id borrowed an approval: %v", e.forge.statuses)
	}
}

func TestChangeIDIsTheLastTrailerOfTheFinalParagraph(t *testing.T) {
	id := func(c byte) string { return "I" + strings.Repeat(string(c), 40) }
	for _, c := range []struct{ name, msg, want string }{
		{"trailer", "P: x\n\nbody\n\nChange-Id: " + id('a') + "\n", id('a')},
		{"with other trailers", "P: x\n\nChange-Id: " + id('a') + "\nSigned-off-by: A <a@x>\n", id('a')},
		{"last of several", "P: x\n\nChange-Id: " + id('a') + "\nChange-Id: " + id('b') + "\n", id('b')},
		{"quoted in the body", "P: x\n\nChange-Id: " + id('a') + "\n\nmore text\n", ""},
		{"subject only", "Change-Id: " + id('a') + "\n", ""},
		{"short id", "P: x\n\nChange-Id: Iabc\n", ""},
		{"upper case hex", "P: x\n\nChange-Id: I" + strings.Repeat("A", 40) + "\n", ""},
		{"none", "P: x\n\nbody\n", ""},
	} {
		if got := ChangeID(c.msg); got != c.want {
			t.Errorf("%s: ChangeID = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGarbageCollectKeepsPatchsetsWhileTheChangeIsFresh(t *testing.T) {
	e := newEnv(t)
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	tip := e.devWork("tmp", map[string]string{"Q/q.go": "package q\n// gc\n"}, "gc")
	oldCommit := sh(t, e.dev, []string{"GIT_COMMITTER_DATE=" + old, "GIT_AUTHOR_DATE=" + old}, "git", "commit-tree", tip+"^{tree}", "-p", tip, "-m", "old")
	stale, live, sec := "I"+strings.Repeat("c", 40), "I"+strings.Repeat("d", 40), "I"+strings.Repeat("e", 40)
	git(t, e.dev, "push", "-q", e.user,
		oldCommit+":refs/ws/cl/"+stale+"/"+oldCommit,   // every patchset of this change is old
		oldCommit+":refs/ws/cl/"+live+"/"+oldCommit,    // an old patchset of a change with a fresh one
		oldCommit+":refs/ws/cl/sec/"+sec+"/"+oldCommit, // embargoed: kept for the embargo TTL
		tip+":refs/ws/cl/"+live+"/"+tip,
		":refs/heads/land/tmp")
	e.l.WorkspaceTTL = 14 * 24 * time.Hour

	if err := e.l.GarbageCollect(e.ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"refs/ws/cl/" + live + "/" + oldCommit, "refs/ws/cl/" + live + "/" + tip, "refs/ws/cl/sec/" + sec + "/" + oldCommit}
	slices.Sort(want)
	if got, want := strings.Join(e.userRefs("refs/ws/"), ","), strings.Join(want, ","); got != want {
		t.Fatalf("patchset refs after GC = %s, want %s", got, want)
	}
}
