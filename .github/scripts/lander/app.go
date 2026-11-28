package lander

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gosuda.org/0/scripts/capy"
)

// App authenticates as a GitHub App and mints installation tokens narrowed to one repository and
// one permission set. Installation ids and tokens are cached. Key and ID are required.
type App struct {
	ID      int64
	Key     *rsa.PrivateKey
	BaseURL string           // default https://api.github.com
	HTTP    *http.Client     // default http.DefaultClient
	Now     func() time.Time // default time.Now

	retry retryPolicy // REST retries; zero value means defaults

	mu       sync.Mutex
	installs map[string]int64       // "owner/name" -> installation id
	tokens   map[string]cachedToken // cacheKey(repo, perms) -> token
}

// cachedToken is reused while now is before refresh.
type cachedToken struct {
	token   string
	refresh time.Time
}

var _ TokenSource = (*App)(nil)

// tokenSkew is how long before GitHub's expires_at a token is no longer reused.
const tokenSkew = 5 * time.Minute

// jwtHeader is the fixed RS256 JOSE header.
const jwtHeader = `{"alg":"RS256","typ":"JWT"}`

// ParseAppKey parses a GitHub App private key in PKCS#1 or PKCS#8 PEM form.
func ParseAppKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("github app key: no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app key: neither PKCS#1 nor PKCS#8: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github app key: %T is not an RSA key", k)
	}
	return rk, nil
}

// Token implements TokenSource. Read maps to contents:read, Write to contents:write, and
// WriteWorkflows to contents:write plus workflows:write.
func (a *App) Token(ctx context.Context, repo string, access Access) (string, error) {
	perms, err := accessPermissions(access)
	if err != nil {
		return "", err
	}
	return a.TokenWithPermissions(ctx, repo, perms)
}

// TokenWithPermissions returns an installation token for repo ("owner/name") limited to perms
// (permission name -> "read" or "write"). Tokens are cached per repository and permission set and
// reused until five minutes before GitHub's expires_at.
func (a *App) TokenWithPermissions(ctx context.Context, repo string, perms map[string]string) (string, error) {
	if len(perms) == 0 {
		return "", errors.New("github app: no permissions requested")
	}
	_, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	key := cacheKey(repo, perms)
	a.mu.Lock()
	c, ok := a.tokens[key]
	a.mu.Unlock()
	if ok && a.now().Before(c.refresh) {
		return c.token, nil
	}
	inst, err := a.installation(ctx, repo)
	if err != nil {
		return "", err
	}
	tok, expires, err := a.mint(ctx, inst, name, perms)
	if err != nil {
		return "", fmt.Errorf("token for %s: %w", repo, err)
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[string]cachedToken{}
	}
	a.tokens[key] = cachedToken{token: tok, refresh: expires.Add(-tokenSkew)}
	a.mu.Unlock()
	return tok, nil
}

// installation returns the installation id of the app on repo, cached per repository.
func (a *App) installation(ctx context.Context, repo string) (int64, error) {
	a.mu.Lock()
	id, ok := a.installs[repo]
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	rp, err := repoPath(repo)
	if err != nil {
		return 0, err
	}
	jwt, err := a.jwt()
	if err != nil {
		return 0, err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := a.call(ctx, http.MethodGet, rp+"/installation", jwt, nil, &inst); err != nil {
		return 0, fmt.Errorf("installation for %s: %w", repo, err)
	}
	if inst.ID == 0 {
		return 0, fmt.Errorf("installation for %s: response has no id", repo)
	}
	a.mu.Lock()
	if a.installs == nil {
		a.installs = map[string]int64{}
	}
	a.installs[repo] = inst.ID
	a.mu.Unlock()
	return inst.ID, nil
}

// mint requests a token for installation inst narrowed to repository name and perms.
func (a *App) mint(ctx context.Context, inst int64, name string, perms map[string]string) (string, time.Time, error) {
	jwt, err := a.jwt()
	if err != nil {
		return "", time.Time{}, err
	}
	in := struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{Repositories: []string{name}, Permissions: perms}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	p := fmt.Sprintf("/app/installations/%d/access_tokens", inst)
	if err := a.call(ctx, http.MethodPost, p, jwt, in, &out); err != nil {
		return "", time.Time{}, err
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("access_tokens response has no token")
	}
	return out.Token, out.ExpiresAt, nil
}

// jwt returns an RS256 app JWT issued 60 seconds ago and valid for nine minutes.
func (a *App) jwt() (string, error) {
	if a.Key == nil {
		return "", errors.New("github app: no private key")
	}
	now := a.now()
	payload, err := json.Marshal(struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}{
		Iat: now.Add(-60 * time.Second).Unix(),
		Exp: now.Add(9 * time.Minute).Unix(),
		Iss: strconv.FormatInt(a.ID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := b64url([]byte(jwtHeader)) + "." + b64url(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64url(sig), nil
}

// call sends one authenticated App API request. Non-2xx responses are *capy.APIError whose body
// has been scrubbed of token-shaped strings.
func (a *App) call(ctx context.Context, method, p, bearer string, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	hdr := githubHeader(bearer)
	if body != nil {
		hdr.Set("Content-Type", "application/json")
	}
	status, resp, err := doWithRetry(ctx, a.retry.norm(), a.httpClient(), method, a.baseURL()+p, hdr, body)
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

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) baseURL() string {
	if a.BaseURL != "" {
		return strings.TrimSuffix(a.BaseURL, "/")
	}
	return defaultAPIBase
}

func (a *App) httpClient() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return http.DefaultClient
}

// accessPermissions maps a lander access level to the permissions it needs.
func accessPermissions(access Access) (map[string]string, error) {
	switch access {
	case Read:
		return map[string]string{"contents": "read"}, nil
	case Write:
		return map[string]string{"contents": "write"}, nil
	case WriteWorkflows:
		return map[string]string{"contents": "write", "workflows": "write"}, nil
	}
	return nil, fmt.Errorf("github app: unknown access %d", access)
}

// splitRepo splits "owner/name" and rejects anything else.
func splitRepo(repo string) (string, string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("invalid repository %q: want owner/name", repo)
	}
	return owner, name, nil
}

// repoPath returns the REST path prefix "/repos/owner/name" for repo ("owner/name").
func repoPath(repo string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// cacheKey identifies a token by repository and permission set.
func cacheKey(repo string, perms map[string]string) string {
	var b strings.Builder
	b.WriteString(repo)
	for _, k := range slices.Sorted(maps.Keys(perms)) {
		b.WriteString("\x00" + k + "=" + perms[k])
	}
	return b.String()
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
