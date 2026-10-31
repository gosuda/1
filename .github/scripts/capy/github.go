package capy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GitHub is a minimal REST client for the identity lookups that authorize landings.
// Token is called for every request so callers can rotate short-lived installation tokens.
type GitHub struct {
	BaseURL string
	HTTP    *http.Client
	Token   func(ctx context.Context) (string, error)
}

// NewGitHub returns a client for api.github.com using a static token.
func NewGitHub(token string) *GitHub {
	return &GitHub{
		BaseURL: "https://api.github.com",
		HTTP:    http.DefaultClient,
		Token:   func(context.Context) (string, error) { return token, nil },
	}
}

// APIError is a non-2xx GitHub response.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s: HTTP %d: %s", e.Path, e.Status, strings.TrimSpace(e.Body))
}

func (g *GitHub) getJSON(ctx context.Context, p string, out any) error {
	tok, err := g.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "suda-capy")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Path: p, Body: string(body)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// ErrActorNotVisible means the activity API has no entry for the requested tip yet.
var ErrActorNotVisible = errors.New("no activity entry for ref tip")

// Push identifies who set a ref tip and when, according to the activity API.
type Push struct {
	Actor string
	Time  time.Time
}

// PushForTip returns the authenticated actor and timestamp of the push that set ref to sha, using
// the repository activity API (unlike commit metadata, GitHub authenticates it). ref is a full
// name such as refs/heads/land/x.
func (g *GitHub) PushForTip(ctx context.Context, repo string, ref string, sha string) (Push, error) {
	q := url.Values{"ref": {ref}, "per_page": {"100"}}
	var items []struct {
		After     string    `json:"after"`
		Type      string    `json:"activity_type"`
		Timestamp time.Time `json:"timestamp"`
		Actor     *struct {
			Login string `json:"login"`
		} `json:"actor"`
	}
	if err := g.getJSON(ctx, "/repos/"+repo+"/activity?"+q.Encode(), &items); err != nil {
		return Push{}, err
	}
	for _, it := range items {
		if it.After != sha || it.Actor == nil || it.Actor.Login == "" {
			continue
		}
		switch it.Type {
		case "push", "force_push", "branch_creation":
			return Push{Actor: strings.ToLower(it.Actor.Login), Time: it.Timestamp}, nil
		}
	}
	return Push{}, fmt.Errorf("%w: %s@%s", ErrActorNotVisible, ref, sha)
}

// PusherForTip returns only the login of PushForTip.
func (g *GitHub) PusherForTip(ctx context.Context, repo string, ref string, sha string) (string, error) {
	p, err := g.PushForTip(ctx, repo, ref, sha)
	return p.Actor, err
}

// ApproversForTip returns reviewers whose latest decisive review on a pull request from
// branch head ("review/<id>") is APPROVED and bound to sha.
func (g *GitHub) ApproversForTip(ctx context.Context, repo string, head string, sha string) ([]string, error) {
	owner, _, _ := strings.Cut(repo, "/")
	q := url.Values{"head": {owner + ":" + head}, "state": {"all"}, "per_page": {"10"}}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := g.getJSON(ctx, "/repos/"+repo+"/pulls?"+q.Encode(), &prs); err != nil {
		return nil, err
	}
	type decision struct {
		state, commit string
	}
	latest := map[string]decision{}
	var order []string
	for _, pr := range prs {
		for page := 1; ; page++ {
			var reviews []struct {
				User *struct {
					Login string `json:"login"`
				} `json:"user"`
				State  string `json:"state"`
				Commit string `json:"commit_id"`
			}
			p := fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100&page=%d", repo, pr.Number, page)
			if err := g.getJSON(ctx, p, &reviews); err != nil {
				return nil, err
			}
			for _, r := range reviews {
				if r.User == nil || r.State == "COMMENTED" || r.State == "PENDING" {
					continue
				}
				login := strings.ToLower(r.User.Login)
				if _, seen := latest[login]; !seen {
					order = append(order, login)
				}
				latest[login] = decision{state: r.State, commit: r.Commit}
			}
			if len(reviews) < 100 {
				break
			}
		}
	}
	var out []string
	for _, login := range order {
		if d := latest[login]; d.state == "APPROVED" && d.commit == sha {
			out = append(out, login)
		}
	}
	return out, nil
}

// TeamResolver returns a resolver backed by the organization team membership API with
// per-resolver caching. Entries are "org/slug".
func (g *GitHub) TeamResolver(ctx context.Context) TeamResolver {
	var mu sync.Mutex
	cache := map[string]bool{}
	return func(team string, login string) (bool, error) {
		key := team + "\x00" + login
		mu.Lock()
		v, ok := cache[key]
		mu.Unlock()
		if ok {
			return v, nil
		}
		org, slug, found := strings.Cut(team, "/")
		if !found {
			return false, fmt.Errorf("invalid team %q", team)
		}
		var m struct {
			State string `json:"state"`
		}
		err := g.getJSON(ctx, fmt.Sprintf("/orgs/%s/teams/%s/memberships/%s", org, slug, login), &m)
		var member bool
		switch {
		case err == nil:
			member = m.State == "active"
		default:
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
				return false, err
			}
		}
		mu.Lock()
		cache[key] = member
		mu.Unlock()
		return member, nil
	}
}
