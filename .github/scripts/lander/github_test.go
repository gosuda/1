package lander

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"gosuda.org/0/scripts/capy"
)

// activityBody is one push of aaa by Alice, as the activity API returns it.
const activityBody = `[{"after":"aaa","activity_type":"push","actor":{"login":"Alice"},"timestamp":"2026-10-30T10:00:00Z"}]`

// scripted is one response in a route's sequence; the last entry repeats.
type scripted struct {
	status int
	header map[string]string
	body   string
}

// sleepLog records retry delays instead of waiting.
type sleepLog struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleepLog) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func (s *sleepLog) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

func TestGitHubForgePushForTipReturnsActivityTimestamp(t *testing.T) {
	env := newAppEnv(t, func(f *fakeGitHub) {
		f.routes = map[string]fakeRoute{
			"GET /repos/gosuda/0/activity": func(w http.ResponseWriter, r *http.Request, g grant) {
				if !checkPerms(w, g, map[string]string{"contents": "read"}) {
					return
				}
				if r.URL.Query().Get("ref") != "refs/heads/land/x" {
					http.Error(w, "wrong ref", http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(`[
					{"after":"bbb","activity_type":"push","actor":{"login":"Mallory"},"timestamp":"2026-10-30T11:00:00Z"},
					{"after":"aaa","activity_type":"push","actor":{"login":"Alice"},"timestamp":"2026-10-30T10:15:30Z"}]`))
			},
		}
	})
	f := NewGitHubForge(env.app, "gosuda/0")
	push, err := f.PushForTip(context.Background(), "gosuda/0", "refs/heads/land/x", "aaa")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 30, 10, 15, 30, 0, time.UTC)
	if push.Actor != "alice" || !push.Time.Equal(want) {
		t.Errorf("push = %+v; want alice at %v", push, want)
	}
}

func TestGitHubForgeOpenPullsPaginates(t *testing.T) {
	var mu sync.Mutex
	var pages []string
	env := newAppEnv(t, func(f *fakeGitHub) {
		f.routes = map[string]fakeRoute{
			"GET /repos/gosuda/0/pulls": func(w http.ResponseWriter, r *http.Request, g grant) {
				if !checkPerms(w, g, map[string]string{"pull_requests": "read"}) {
					return
				}
				q := r.URL.Query()
				if q.Get("state") != "open" || q.Get("per_page") != "100" {
					http.Error(w, "bad query", http.StatusBadRequest)
					return
				}
				mu.Lock()
				pages = append(pages, q.Get("page"))
				mu.Unlock()
				page, _ := strconv.Atoi(q.Get("page"))
				n := map[int]int{1: 100, 2: 3}[page]
				first := (page-1)*100 + 1
				items := make([]map[string]any, 0, n)
				for i := first; i < first+n; i++ {
					items = append(items, map[string]any{
						"number": i,
						"draft":  i%2 == 0,
						"head":   map[string]string{"ref": fmt.Sprintf("feature-%d", i), "sha": fmt.Sprintf("sha%d", i)},
					})
				}
				_ = json.NewEncoder(w).Encode(items)
			},
		}
	})
	f := NewGitHubForge(env.app, "gosuda/0")
	got, err := f.OpenPulls(context.Background(), "gosuda/0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 103 {
		t.Fatalf("got %d pulls; want 103", len(got))
	}
	if got[1] != (PullRequest{Number: 2, HeadRef: "feature-2", HeadSHA: "sha2", Draft: true}) {
		t.Errorf("pull 2 = %+v", got[1])
	}
	if got[102] != (PullRequest{Number: 103, HeadRef: "feature-103", HeadSHA: "sha103", Draft: false}) {
		t.Errorf("pull 103 = %+v", got[102])
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(pages, []string{"1", "2"}) {
		t.Errorf("pages requested = %v; want [1 2]", pages)
	}
}

func TestGitHubForgeSetStatusTruncatesDescription(t *testing.T) {
	var mu sync.Mutex
	var got map[string]string
	env := newAppEnv(t, func(f *fakeGitHub) {
		f.routes = map[string]fakeRoute{
			"POST /repos/gosuda/0/statuses/abc123": func(w http.ResponseWriter, r *http.Request, g grant) {
				if !checkPerms(w, g, map[string]string{"statuses": "write"}) {
					return
				}
				var in map[string]string
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					http.Error(w, "bad body", http.StatusBadRequest)
					return
				}
				mu.Lock()
				got = in
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{}`))
			},
		}
	})
	f := NewGitHubForge(env.app, "gosuda/0")
	// Multi-byte runes check that truncation counts characters, not bytes.
	long := strings.Repeat("é", 300)
	if err := f.SetStatus(context.Background(), "gosuda/0", "abc123", Status{State: "failure", Context: "suda/land", Description: long}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["state"] != "failure" || got["context"] != "suda/land" {
		t.Errorf("state/context = %q/%q", got["state"], got["context"])
	}
	if got["description"] != strings.Repeat("é", 140) || !utf8.ValidString(got["description"]) {
		t.Errorf("description has %d runes; want 140", utf8.RuneCountInString(got["description"]))
	}
}

func TestGitHubForgeSetStatusRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	env := newAppEnv(t, func(f *fakeGitHub) {
		f.routes = map[string]fakeRoute{
			"POST /repos/gosuda/0/statuses/abc123": func(w http.ResponseWriter, r *http.Request, g grant) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{}`))
			},
		}
	})
	var sleeps sleepLog
	env.app.retry = retryPolicy{sleep: sleeps.sleep}
	f := NewGitHubForge(env.app, "gosuda/0")
	if err := f.SetStatus(context.Background(), "gosuda/0", "abc123", Status{State: "success", Context: "suda/land"}); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("status POSTs = %d; want 2", n)
	}
	if d := sleeps.durations(); !slices.Equal(d, []time.Duration{time.Second}) {
		t.Errorf("sleeps = %v; want [1s] from Retry-After", d)
	}
}

func TestGitHubRetryPolicyOnReads(t *testing.T) {
	ok := scripted{status: http.StatusOK, body: activityBody}
	cases := []struct {
		name       string
		script     []scripted
		wantCalls  int32
		wantSleeps int
		wantFirst  time.Duration // exact first delay when nonzero
		wantStatus int           // expected APIError status; 0 means success
	}{
		{"429 then 200", []scripted{{status: 429}, ok}, 2, 1, 0, 0},
		{"429 Retry-After honored", []scripted{{status: 429, header: map[string]string{"Retry-After": "2"}}, ok}, 2, 1, 2 * time.Second, 0},
		{"secondary limit 403 then 200", []scripted{{status: 403, header: map[string]string{"X-RateLimit-Remaining": "0"}}, ok}, 2, 1, 0, 0},
		{"503 exhausts five attempts", []scripted{{status: 503}}, 5, 4, 0, 503},
		{"403 without limit headers is final", []scripted{{status: 403, body: `{"message":"Resource not accessible"}`}}, 1, 0, 0, 403},
		{"Retry-After beyond cap is final", []scripted{{status: 429, header: map[string]string{"Retry-After": "60"}}}, 1, 0, 0, 429},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			env := newAppEnv(t, func(f *fakeGitHub) {
				f.routes = map[string]fakeRoute{
					"GET /repos/gosuda/0/activity": func(w http.ResponseWriter, r *http.Request, g grant) {
						i := int(calls.Add(1)) - 1
						s := tc.script[min(i, len(tc.script)-1)]
						for k, v := range s.header {
							w.Header().Set(k, v)
						}
						w.WriteHeader(s.status)
						_, _ = w.Write([]byte(s.body))
					},
				}
			})
			var sleeps sleepLog
			env.app.retry = retryPolicy{sleep: sleeps.sleep}
			f := NewGitHubForge(env.app, "gosuda/0")
			push, err := f.PushForTip(context.Background(), "gosuda/0", "refs/heads/land/x", "aaa")
			if tc.wantStatus == 0 {
				if err != nil || push.Actor != "alice" {
					t.Fatalf("push = %+v, err = %v; want alice", push, err)
				}
			} else {
				var ae *capy.APIError
				if !errors.As(err, &ae) || ae.Status != tc.wantStatus {
					t.Fatalf("err = %v; want APIError %d", err, tc.wantStatus)
				}
			}
			if n := calls.Load(); n != tc.wantCalls {
				t.Errorf("requests = %d; want %d", n, tc.wantCalls)
			}
			d := sleeps.durations()
			if len(d) != tc.wantSleeps {
				t.Fatalf("sleeps = %v; want %d", d, tc.wantSleeps)
			}
			if tc.wantFirst != 0 && d[0] != tc.wantFirst {
				t.Errorf("first sleep = %v; want %v", d[0], tc.wantFirst)
			}
		})
	}
}

func TestGitHubRetryStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	env := newAppEnv(t, func(f *fakeGitHub) {
		f.routes = map[string]fakeRoute{
			"GET /repos/gosuda/0/activity": func(w http.ResponseWriter, r *http.Request, g grant) {
				calls.Add(1)
				cancel()
				w.WriteHeader(http.StatusTooManyRequests)
			},
		}
	})
	env.app.retry = retryPolicy{sleep: sleepContext}
	f := NewGitHubForge(env.app, "gosuda/0")
	if _, err := f.PushForTip(ctx, "gosuda/0", "refs/heads/land/x", "aaa"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("requests = %d; want 1 (no retry after cancellation)", n)
	}
}

func TestGitHubRetryBackoffBounds(t *testing.T) {
	p := retryPolicy{}.norm()
	for n := 1; n <= 6; n++ {
		lo := p.base
		for i := 1; i < n && lo < p.limit; i++ {
			lo *= 2
		}
		lo = min(lo, p.limit)
		hi := min(lo+lo/4, p.limit)
		for range 200 {
			if d := p.backoff(n); d < lo || d > hi {
				t.Fatalf("backoff(%d) = %v; want within [%v, %v]", n, d, lo, hi)
			}
		}
	}
}

func TestGitHubForgeTeamResolverMintsMembersToken(t *testing.T) {
	env := newAppEnv(t, func(f *fakeGitHub) {
		f.routes = map[string]fakeRoute{
			"GET /orgs/gosuda/teams/core/memberships/alice": func(w http.ResponseWriter, r *http.Request, g grant) {
				if !checkPerms(w, g, map[string]string{"members": "read"}) {
					return
				}
				_, _ = w.Write([]byte(`{"state":"active"}`))
			},
		}
	})
	f := NewGitHubForge(env.app, "gosuda/0")
	member, err := f.TeamResolver(context.Background())("gosuda/core", "alice")
	if err != nil || !member {
		t.Fatalf("member = %v, err = %v; want active member", member, err)
	}
	last := env.fake.lastMint()
	if !slices.Equal(last.Repositories, []string{"0"}) || !maps.Equal(last.Permissions, map[string]string{"members": "read"}) {
		t.Errorf("mint = %+v; want repositories [0] with members:read", last)
	}
}

func TestGitHubStaticForgeSendsStaticToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer static-tok" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(activityBody))
	}))
	t.Cleanup(srv.Close)
	f := NewStaticForge("static-tok")
	f.BaseURL = srv.URL
	push, err := f.PushForTip(context.Background(), "gosuda/0", "refs/heads/land/x", "aaa")
	if err != nil || push.Actor != "alice" {
		t.Fatalf("push = %+v, err = %v; want alice", push, err)
	}
}

func TestGitHubForgeClosePullClosesThenCommentsWithWriteScopedToken(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var comment string
	rec := func(name string, want map[string]string, decode func(*http.Request)) fakeRoute {
		return func(w http.ResponseWriter, r *http.Request, g grant) {
			if !checkPerms(w, g, want) {
				return
			}
			mu.Lock()
			calls = append(calls, name)
			mu.Unlock()
			if decode != nil {
				decode(r)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}
	env := newAppEnv(t, func(f *fakeGitHub) {
		f.routes = map[string]fakeRoute{
			"PATCH /repos/gosuda/HQQ/pulls/12": rec("close", map[string]string{"pull_requests": "write"}, func(r *http.Request) {
				var in map[string]string
				_ = json.NewDecoder(r.Body).Decode(&in)
				if in["state"] != "closed" {
					t.Errorf("close body = %v", in)
				}
			}),
			"POST /repos/gosuda/HQQ/issues/12/comments": rec("comment", map[string]string{"pull_requests": "write"}, func(r *http.Request) {
				var in map[string]string
				_ = json.NewDecoder(r.Body).Decode(&in)
				mu.Lock()
				comment = in["body"]
				mu.Unlock()
			}),
		}
	})
	f := NewGitHubForge(env.app, "gosuda/1")
	if err := f.ClosePull(context.Background(), "gosuda/HQQ", 12, "landed, thanks"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(calls, ",") != "close,comment" || comment != "landed, thanks" {
		t.Fatalf("calls = %v, comment = %q", calls, comment)
	}
}
