package lander

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gosuda.org/0/scripts/capy"
)

// requireReviewOnP lands `set require-review` for P (judged by the OWNERS before it, so the
// maintainer may land it) and returns the mirror tip once the export settled.
func (e *env) requireReviewOnP() string {
	e.cycle() // bootstrap the baseline
	e.devWork("rr", map[string]string{"P/OWNERS": "set noparent\nset require-review\n@alice\n@maintainer\n"}, "P: require review")
	e.cycle()
	return e.trunkRefMirror()
}

// mirrorPull opens pull request number on the mirror from a branch on from, then merges it into main
// with a merge commit, as the GitHub web UI does. extra, when set, is committed into the merge
// commit itself (a merge that changes more than the reviewed head). It returns the head and the merge.
func (e *env) mirrorPull(number int, from string, files, extra map[string]string) (head, merge string) {
	t := e.t
	dir := filepath.Join(e.root, fmt.Sprintf("mirror-pr-%d", number))
	os.RemoveAll(dir)
	git(t, e.root, "clone", "-q", "-b", "main", e.mirror, dir)
	git(t, dir, "checkout", "-q", "-b", "feature", from)
	write(t, dir, files)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", fmt.Sprintf("feature %d", number))
	head = git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "push", "-q", "origin", head+":"+fmt.Sprintf("refs/pull/%d/head", number))
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "--no-edit", "-m", fmt.Sprintf("Merge pull request #%d", number), "feature")
	if extra != nil {
		write(t, dir, extra)
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "--amend", "--no-edit")
	}
	merge = git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "push", "-q", "origin", "HEAD:refs/heads/main")
	return head, merge
}

// Under `set require-review`, pull requests merged on the mirror land when another owner approved
// the merged head on GitHub, also when the branch moved between the branch point and the merge.
func TestApprovedMirrorPullRequestsLandUnderRequireReview(t *testing.T) {
	e := newEnv(t)
	base := e.requireReviewOnP()
	head1, merge1 := e.mirrorPull(11, base, map[string]string{"c.go": "package p\n// c\n"}, nil)
	head2, merge2 := e.mirrorPull(12, base, map[string]string{"d.go": "package p\n// d\n"}, nil) // branched before #11 merged
	e.forge.chains = map[string][]capy.RefPush{"gosuda/p|refs/heads/main": {
		{Push: capy.Push{Actor: "maintainer"}, Before: base, After: merge1, PRMerge: true},
		{Push: capy.Push{Actor: "maintainer"}, Before: merge1, After: merge2, PRMerge: true},
	}}
	e.forge.mergedFor["gosuda/p|"+merge1] = capy.MergedPull{Number: 11, Head: head1}
	e.forge.mergedFor["gosuda/p|"+merge2] = capy.MergedPull{Number: 12, Head: head2}
	e.forge.pullOK["gosuda/p#11"] = []capy.Approval{{Login: "alice", Commit: head1}}
	e.forge.pullOK["gosuda/p#12"] = []capy.Approval{{Login: "alice", Commit: head2}}

	e.cycle()

	if !e.trunkHas("P/c.go") || !e.trunkHas("P/d.go") {
		t.Fatalf("approved mirror pull requests did not land: %v", e.userRefs("refs/ws/capy/"))
	}
	if refs := e.userRefs("refs/ws/capy/"); len(refs) != 0 {
		t.Fatalf("nothing needs staging, got %v", refs)
	}
	log := git(t, e.trunk, "log", "-2", "--format=%B", "refs/suda/history")
	for _, want := range []string{"Suda-Mirror-Pull: gosuda/p#11", "Suda-Mirror-Pull: gosuda/p#12", "Suda-Approved-By: alice", "Suda-Requester: maintainer"} {
		if !strings.Contains(log, want) {
			t.Fatalf("landed history lacks %q:\n%s", want, log)
		}
	}
}

func TestMirrorPullRequestsWithoutAValidApprovalAreStagedUnderRequireReview(t *testing.T) {
	cases := []struct {
		name      string
		approvals func(head string) []capy.Approval
		extra     map[string]string // committed into the merge commit
		push      bool              // a plain push instead of a pull request merge
	}{
		{name: "no approval", approvals: func(string) []capy.Approval { return nil }},
		{name: "approval of an earlier head", approvals: func(string) []capy.Approval {
			return []capy.Approval{{Login: "alice", Commit: strings.Repeat("0", 40)}}
		}},
		{name: "approval by the merger only", approvals: func(head string) []capy.Approval {
			return []capy.Approval{{Login: "maintainer", Commit: head}}
		}},
		{name: "approval by a non-owner", approvals: func(head string) []capy.Approval {
			return []capy.Approval{{Login: "mallory", Commit: head}}
		}},
		{name: "merge adds content the reviewers did not see", extra: map[string]string{"x.go": "package p\n// x\n"},
			approvals: func(head string) []capy.Approval { return []capy.Approval{{Login: "alice", Commit: head}} }},
		{name: "plain push of an approved head", push: true,
			approvals: func(head string) []capy.Approval { return []capy.Approval{{Login: "alice", Commit: head}} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			base := e.requireReviewOnP()
			head, merge := e.mirrorPull(21, base, map[string]string{"c.go": "package p\n// c\n"}, c.extra)
			e.forge.chains = map[string][]capy.RefPush{"gosuda/p|refs/heads/main": {
				{Push: capy.Push{Actor: "maintainer"}, Before: base, After: merge, PRMerge: !c.push},
			}}
			e.forge.mergedFor["gosuda/p|"+merge] = capy.MergedPull{Number: 21, Head: head}
			e.forge.pullOK["gosuda/p#21"] = c.approvals(head)
			mirrorTip := e.trunkRefMirror()

			e.cycle()

			if e.trunkHas("P/c.go") {
				t.Fatal("the merge landed without a valid approval")
			}
			if refs := e.userRefs("refs/ws/capy/"); len(refs) != 1 || refs[0] != "refs/ws/capy/p-main" {
				t.Fatalf("staged refs = %v", refs)
			}
			if e.trunkRefMirror() != mirrorTip {
				t.Fatal("export must not overwrite the unreviewed mirror merge")
			}
		})
	}
}

// Without `set require-review`, an owner's merge needs no approval, and a merge by a non-owner lands
// on an owner's approval of the merged head.
func TestOwnerApprovalOfAMirrorPullRequestCarriesANonOwnerMerge(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	base := e.trunkRefMirror()
	head, merge := e.mirrorPull(31, base, map[string]string{"c.go": "package p\n// c\n"}, nil)
	e.forge.chains = map[string][]capy.RefPush{"gosuda/p|refs/heads/main": {
		{Push: capy.Push{Actor: "mallory"}, Before: base, After: merge, PRMerge: true},
	}}
	e.forge.mergedFor["gosuda/p|"+merge] = capy.MergedPull{Number: 31, Head: head}
	e.forge.pullOK["gosuda/p#31"] = []capy.Approval{{Login: "alice", Commit: head}}

	e.cycle()

	if !e.trunkHas("P/c.go") {
		t.Fatalf("an owner-approved merge by a non-owner did not land: %v", e.userRefs("refs/ws/capy/"))
	}
}
