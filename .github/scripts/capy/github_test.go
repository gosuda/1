package capy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gosuda.org/0/scripts/capy"
)

func newFakeGitHub(t *testing.T, routes map[string]string) *capy.GitHub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, ok := routes[r.URL.RequestURI()]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	gh := capy.NewGitHub("tok")
	gh.BaseURL = srv.URL
	return gh
}

func TestPusherForTipUsesActivityEntryMatchingTip(t *testing.T) {
	gh := newFakeGitHub(t, map[string]string{
		"/repos/gosuda/1/activity?per_page=100&ref=refs%2Fheads%2Fland%2Fx": `[
			{"after":"bbb","activity_type":"force_push","actor":{"login":"Mallory"}},
			{"after":"aaa","activity_type":"branch_creation","actor":{"login":"Alice"}}]`,
	})
	got, err := gh.PusherForTip(context.Background(), "gosuda/1", "refs/heads/land/x", "aaa")
	if err != nil || got != "alice" {
		t.Fatalf("PusherForTip = %q, %v; want alice", got, err)
	}
	if _, err := gh.PusherForTip(context.Background(), "gosuda/1", "refs/heads/land/x", "ccc"); !errors.Is(err, capy.ErrActorNotVisible) {
		t.Fatalf("unknown tip err = %v, want ErrActorNotVisible", err)
	}
}

func TestPusherForTipAcceptsPullRequestMerges(t *testing.T) {
	gh := newFakeGitHub(t, map[string]string{
		"/repos/gosuda/p/activity?per_page=100&ref=refs%2Fheads%2Fmain": `[
			{"before":"aaa","after":"bbb","activity_type":"pr_merge","actor":{"login":"Alice"}},
			{"before":"zzz","after":"ccc","activity_type":"branch_deletion","actor":{"login":"Mallory"}}]`,
	})
	got, err := gh.PusherForTip(context.Background(), "gosuda/p", "refs/heads/main", "bbb")
	if err != nil || got != "alice" {
		t.Fatalf("PusherForTip(pr_merge) = %q, %v; want alice", got, err)
	}
	if _, err := gh.PusherForTip(context.Background(), "gosuda/p", "refs/heads/main", "ccc"); !errors.Is(err, capy.ErrActorNotVisible) {
		t.Fatalf("a ref deletion must not identify a pusher, err = %v", err)
	}
}

func TestPushChainReturnsEachPushWithItsOwnActorOldestFirst(t *testing.T) {
	gh := newFakeGitHub(t, map[string]string{
		"/repos/gosuda/p/activity?per_page=100&ref=refs%2Fheads%2Fmain": `[
			{"before":"bbb","after":"ccc","activity_type":"push","actor":{"login":"github-actions[bot]"}},
			{"before":"aaa","after":"bbb","activity_type":"pr_merge","actor":{"login":"Alice"}},
			{"before":"000","after":"aaa","activity_type":"push","actor":{"login":"Carol"}}]`,
	})
	ctx := context.Background()
	chain, err := gh.PushChain(ctx, "gosuda/p", "refs/heads/main", "aaa", "ccc")
	if err != nil {
		t.Fatal(err)
	}
	type step struct{ actor, before, after string }
	var got []step
	for _, p := range chain {
		got = append(got, step{p.Actor, p.Before, p.After})
	}
	want := []step{{"alice", "aaa", "bbb"}, {"github-actions[bot]", "bbb", "ccc"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	if chain, err := gh.PushChain(ctx, "gosuda/p", "refs/heads/main", "ccc", "ccc"); err != nil || len(chain) != 0 {
		t.Fatalf("empty range = %v, %v; want no pushes", chain, err)
	}
	// The baseline is not an ancestor the API knows about: the range is not covered.
	if _, err := gh.PushChain(ctx, "gosuda/p", "refs/heads/main", "unknown", "ccc"); !errors.Is(err, capy.ErrActorNotVisible) {
		t.Fatalf("uncovered range err = %v, want ErrActorNotVisible", err)
	}
}

func TestApproversForTipIgnoresStaleCommentedAndDismissedReviews(t *testing.T) {
	gh := newFakeGitHub(t, map[string]string{
		"/repos/gosuda/1/pulls?head=gosuda%3Areview%2Fx&per_page=10&state=all": `[{"number":7}]`,
		"/repos/gosuda/1/pulls/7/reviews?per_page=100&page=1": `[
			{"user":{"login":"alice"},"state":"APPROVED","commit_id":"old"},
			{"user":{"login":"bob"},"state":"APPROVED","commit_id":"tip"},
			{"user":{"login":"bob"},"state":"COMMENTED","commit_id":"tip"},
			{"user":{"login":"carol"},"state":"APPROVED","commit_id":"tip"},
			{"user":{"login":"carol"},"state":"CHANGES_REQUESTED","commit_id":"tip"},
			{"user":{"login":"Dave"},"state":"APPROVED","commit_id":"tip"}]`,
	})
	got, err := gh.ApproversForTip(context.Background(), "gosuda/1", "review/x", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bob", "dave"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("approvers = %v, want %v", got, want)
	}
}

func TestReviewApprovalsKeepsTheCommitEachApprovalIsBoundTo(t *testing.T) {
	gh := newFakeGitHub(t, map[string]string{
		"/repos/gosuda/1/pulls?head=gosuda%3Areview%2Fx&per_page=10&state=all": `[{"number":9},{"number":7}]`,
		"/repos/gosuda/1/pulls/7/reviews?per_page=100&page=1": `[
			{"user":{"login":"alice"},"state":"APPROVED","commit_id":"ps1"},
			{"user":{"login":"carol"},"state":"APPROVED","commit_id":"ps1"}]`,
		"/repos/gosuda/1/pulls/9/reviews?per_page=100&page=1": `[
			{"user":{"login":"bob"},"state":"APPROVED","commit_id":"ps2"},
			{"user":{"login":"carol"},"state":"CHANGES_REQUESTED","commit_id":"ps2"},
			{"user":{"login":"Dave"},"state":"APPROVED","commit_id":"ps1"},
			{"user":{"login":"Dave"},"state":"COMMENTED","commit_id":"ps2"}]`,
	})
	rs, err := gh.ReviewApprovals(context.Background(), "gosuda/1", "review/x")
	if err != nil {
		t.Fatal(err)
	}
	want := capy.ReviewState{Pull: 9, Approvals: []capy.Approval{
		{Login: "alice", Commit: "ps1"},
		{Login: "bob", Commit: "ps2"},
		{Login: "dave", Commit: "ps1"},
	}}
	if !reflect.DeepEqual(rs, want) {
		t.Fatalf("review state = %+v, want %+v (the newest pull request's decision supersedes an older approval; the API lists newest first)", rs, want)
	}
}

func TestTeamResolverTreatsOnly404AsNonMember(t *testing.T) {
	gh := newFakeGitHub(t, map[string]string{
		"/orgs/gosuda/teams/core/memberships/carol":   `{"state":"active"}`,
		"/orgs/gosuda/teams/core/memberships/pending": `{"state":"pending"}`,
	})
	resolve := gh.TeamResolver(context.Background())
	for login, want := range map[string]bool{"carol": true, "pending": false, "nobody": false} {
		got, err := resolve("gosuda/core", login)
		if err != nil || got != want {
			t.Errorf("resolve(%s) = %v, %v; want %v", login, got, err, want)
		}
	}
	bad := capy.NewGitHub("wrong")
	bad.BaseURL = gh.BaseURL
	if _, err := bad.TeamResolver(context.Background())("gosuda/core", "carol"); err == nil {
		t.Error("401 must surface as an error, not as non-membership")
	}
}
