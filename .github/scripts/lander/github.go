package lander

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gosuda.org/0/scripts/capy"
)

const (
	defaultAPIBase   = "https://api.github.com"
	githubAPIVersion = "2022-11-28"
	userAgent        = "suda-lander"
	// maxResponseBytes bounds REST response bodies, matching capy.
	maxResponseBytes = 8 << 20
	// maxStatusDescription is the longest commit status description GitHub accepts.
	maxStatusDescription = 140
	// perPage is the page size for paginated listings.
	perPage = 100
)

// Permission sets per call. members:read is organization-level and is minted on the default repository.
var (
	permContents = map[string]string{"contents": "read"}
	permPulls    = map[string]string{"pull_requests": "read"}
	permStatuses = map[string]string{"statuses": "write"}
	permPullsW   = map[string]string{"pull_requests": "write"}
	permMembers  = map[string]string{"members": "read"}
)

// errCapyToken is returned by the embedded capy client's Token, which GitHubForge never uses.
var errCapyToken = errors.New("github forge: call a GitHubForge method; the embedded capy client has no token")

// tokenFunc mints a token for repo limited to perms.
type tokenFunc func(ctx context.Context, repo string, perms map[string]string) (string, error)

var _ Forge = (*GitHubForge)(nil)

// GitHubForge implements Forge against the GitHub REST API. Each call requests an installation
// token narrowed to the repository and permissions it needs.
type GitHubForge struct {
	*capy.GitHub // BaseURL and a GET-retrying HTTP client; Token is replaced per call

	plain       *http.Client // same client without retry; rest retries itself
	tokens      tokenFunc
	defaultRepo string
	retry       retryPolicy
}

// NewGitHubForge returns a Forge that authenticates through app. defaultRepo ("owner/name") is the
// repository whose installation mints organization-level tokens such as members:read.
func NewGitHubForge(app *App, defaultRepo string) *GitHubForge {
	return newGitHubForge(app.baseURL(), app.httpClient(), app.TokenWithPermissions, defaultRepo, app.retry)
}

// NewStaticForge returns a Forge that sends token on every request. It serves CLI and development
// use; the token is not narrowed per repository, so the service uses NewGitHubForge.
func NewStaticForge(token string) *GitHubForge {
	static := func(context.Context, string, map[string]string) (string, error) { return token, nil }
	return newGitHubForge(defaultAPIBase, http.DefaultClient, static, "", retryPolicy{})
}

func newGitHubForge(base string, hc *http.Client, tokens tokenFunc, defaultRepo string, pol retryPolicy) *GitHubForge {
	pol = pol.norm()
	return &GitHubForge{
		GitHub: &capy.GitHub{
			BaseURL: base,
			HTTP:    withRetry(hc, pol),
			Token:   func(context.Context) (string, error) { return "", errCapyToken },
		},
		plain:       hc,
		tokens:      tokens,
		defaultRepo: defaultRepo,
		retry:       pol,
	}
}

// client returns a capy.GitHub that mints a token for repo and perms on every request.
func (f *GitHubForge) client(repo string, perms map[string]string) *capy.GitHub {
	c := *f.GitHub
	c.Token = func(ctx context.Context) (string, error) { return f.tokens(ctx, repo, perms) }
	return &c
}

// PushForTip implements Forge through the activity API (contents:read).
func (f *GitHubForge) PushForTip(ctx context.Context, repo, ref, sha string) (capy.Push, error) {
	return f.client(repo, permContents).PushForTip(ctx, repo, ref, sha)
}

// PusherForTip returns only the login of PushForTip (contents:read).
func (f *GitHubForge) PusherForTip(ctx context.Context, repo, ref, sha string) (string, error) {
	return f.client(repo, permContents).PusherForTip(ctx, repo, ref, sha)
}

// ApproversForTip implements Forge through the pull request and review APIs (pull_requests:read).
func (f *GitHubForge) ApproversForTip(ctx context.Context, repo, head, sha string) ([]string, error) {
	return f.client(repo, permPulls).ApproversForTip(ctx, repo, head, sha)
}

// TeamResolver implements Forge through organization membership (members:read). The token is minted
// on defaultRepo because organization permissions apply through any repository of the installation.
func (f *GitHubForge) TeamResolver(ctx context.Context) capy.TeamResolver {
	return f.client(f.defaultRepo, permMembers).TeamResolver(ctx)
}

// SetStatus implements Forge with POST /repos/{repo}/statuses/{sha} (statuses:write). The
// description is truncated to GitHub's limit.
func (f *GitHubForge) SetStatus(ctx context.Context, repo, sha string, st Status) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	in := map[string]string{
		"state":       st.State,
		"context":     st.Context,
		"description": truncateRunes(st.Description, maxStatusDescription),
	}
	return f.rest(ctx, repo, permStatuses, http.MethodPost, rp+"/statuses/"+url.PathEscape(sha), in, nil)
}

// OpenPulls implements Forge, paging through every open pull request (pull_requests:read).
func (f *GitHubForge) OpenPulls(ctx context.Context, repo string) ([]PullRequest, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var out []PullRequest
	for page := 1; ; page++ {
		q := url.Values{"state": {"open"}, "per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		var items []struct {
			Number int  `json:"number"`
			Draft  bool `json:"draft"`
			Head   struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if err := f.rest(ctx, repo, permPulls, http.MethodGet, rp+"/pulls?"+q.Encode(), nil, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			out = append(out, PullRequest{Number: it.Number, HeadRef: it.Head.Ref, HeadSHA: it.Head.SHA, Draft: it.Draft})
		}
		if len(items) < perPage {
			return out, nil
		}
	}
}

// ClosePull implements Forge: it closes the pull request, then comments. The order avoids a
// duplicate comment when a retry follows a failure between the two calls.
func (f *GitHubForge) ClosePull(ctx context.Context, repo string, number int, comment string) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	if err := f.rest(ctx, repo, permPullsW, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", rp, number), map[string]string{"state": "closed"}, nil); err != nil {
		return err
	}
	if comment == "" {
		return nil
	}
	return f.rest(ctx, repo, permPullsW, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", rp, number), map[string]string{"body": comment}, nil)
}

// rest performs one REST call for repo with a token limited to perms. Non-2xx responses are
// *capy.APIError. in is JSON-encoded when non-nil; out is decoded when non-nil.
func (f *GitHubForge) rest(ctx context.Context, repo string, perms map[string]string, method, p string, in, out any) error {
	tok, err := f.tokens(ctx, repo, perms)
	if err != nil {
		return fmt.Errorf("token for %s: %w", repo, err)
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	hdr := githubHeader(tok)
	if body != nil {
		hdr.Set("Content-Type", "application/json")
	}
	status, resp, err := doWithRetry(ctx, f.retry, f.plain, method, f.BaseURL+p, hdr, body)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, p, err)
	}
	if status/100 != 2 {
		return &capy.APIError{Status: status, Path: p, Body: errorBody(resp)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(resp, out)
}

// truncateRunes returns the first n runes of s.
func truncateRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

// githubHeader returns the headers every REST request carries. bearer is optional.
func githubHeader(bearer string) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/vnd.github+json")
	h.Set("X-GitHub-Api-Version", githubAPIVersion)
	h.Set("User-Agent", userAgent)
	if bearer != "" {
		h.Set("Authorization", "Bearer "+bearer)
	}
	return h
}

// tokenShaped matches GitHub token formats and JWTs; error bodies are scrubbed of them.
var tokenShaped = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+|eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*`)

// errorBody bounds a response body and removes token-shaped strings before it enters an error.
func errorBody(b []byte) string {
	const limit = 1024
	if len(b) > limit {
		b = b[:limit]
	}
	return tokenShaped.ReplaceAllString(string(b), "[redacted]")
}

// retryPolicy bounds retries of transient GitHub failures: 5xx, 429, and 403 that carries
// Retry-After or x-ratelimit-remaining: 0 (secondary rate limits). The zero value means defaults.
type retryPolicy struct {
	attempts int                                        // total attempts including the first; default 5
	base     time.Duration                              // first backoff; default 1s
	limit    time.Duration                              // cap on any single delay; default 30s
	sleep    func(context.Context, time.Duration) error // default: timer that honors ctx
}

// norm fills zero fields with defaults.
func (p retryPolicy) norm() retryPolicy {
	if p.attempts == 0 {
		p.attempts = 5
	}
	if p.base == 0 {
		p.base = time.Second
	}
	if p.limit == 0 {
		p.limit = 30 * time.Second
	}
	if p.sleep == nil {
		p.sleep = sleepContext
	}
	return p
}

// send runs attempt until its response is final or the policy gives up. attempt must build a fresh
// request on every call. A response that is not retried is returned unread.
func (p retryPolicy) send(ctx context.Context, attempt func() (*http.Response, error)) (*http.Response, error) {
	p = p.norm()
	for n := 1; ; n++ {
		resp, err := attempt()
		if err != nil {
			return nil, err
		}
		wait, again := p.delay(resp, n)
		if !again {
			return resp, nil
		}
		drainClose(resp)
		if err := p.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// delay reports whether resp, the response to attempt n, should be retried and how long to wait.
// A Retry-After longer than the cap ends retrying rather than waiting past the cap.
func (p retryPolicy) delay(resp *http.Response, n int) (time.Duration, bool) {
	if n >= p.attempts || !transient(resp) {
		return 0, false
	}
	if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
		if d > p.limit {
			return 0, false
		}
		return d, true
	}
	return p.backoff(n), true
}

// backoff returns the jittered delay after attempt n: base·2^(n-1) capped at limit, plus up to
// 25%, capped at limit again.
func (p retryPolicy) backoff(n int) time.Duration {
	d := p.base
	for i := 1; i < n && d < p.limit; i++ {
		d *= 2
	}
	d = min(d, p.limit)
	if j := d / 4; j > 0 {
		d += rand.N(j)
	}
	return min(d, p.limit)
}

// transient reports whether resp is a failure the policy retries.
func transient(resp *http.Response) bool {
	switch code := resp.StatusCode; {
	case code == http.StatusTooManyRequests, code >= 500 && code <= 599:
		return true
	case code == http.StatusForbidden:
		return resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-Ratelimit-Remaining") == "0"
	}
	return false
}

// parseRetryAfter reads a Retry-After value given as delta-seconds or an HTTP date.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

// sleepContext waits d or until ctx ends, whichever is first.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// drainClose discards a response body so its connection can be reused.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// doWithRetry sends one REST request under p and returns the final status and body. Non-2xx
// statuses are returned, not errored; callers decide. hdr is cloned per attempt.
func doWithRetry(ctx context.Context, p retryPolicy, hc *http.Client, method, u string, hdr http.Header, body []byte) (int, []byte, error) {
	resp, err := p.send(ctx, func() (*http.Response, error) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		req.Header = hdr.Clone()
		return hc.Do(req)
	})
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, out, nil
}

// withRetry returns a copy of hc whose transport retries GET requests under pol. Other methods pass
// through once.
func withRetry(hc *http.Client, pol retryPolicy) *http.Client {
	c := *hc
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = &retryTransport{base: base, policy: pol}
	return &c
}

// retryTransport applies the retry policy to GET requests, which the capy client issues.
type retryTransport struct {
	base   http.RoundTripper
	policy retryPolicy
}

// RoundTrip implements http.RoundTripper.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return t.base.RoundTrip(req)
	}
	return t.policy.send(req.Context(), func() (*http.Response, error) { return t.base.RoundTrip(req) })
}
