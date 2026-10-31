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
