package lander

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/0/scripts/capy"
)

// testInstallation is the installation id the fake reports for every repository.
const testInstallation = 42

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

// sharedKey returns one RSA key for all tests; generating a key per test is slow.
func sharedKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

// testClock is a settable clock for App.Now.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeFailure is an error response for an App endpoint.
type fakeFailure struct {
	status int
	body   string
}

// mintRequest is the body of POST /app/installations/{id}/access_tokens.
type mintRequest struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

// grant is what the fake issued for one installation token.
type grant struct {
	repos []string
	perms map[string]string
}

// fakeRoute serves an installation-token request; g describes the token that authorized it.
type fakeRoute func(w http.ResponseWriter, r *http.Request, g grant)

// fakeGitHub serves the App endpoints and optional installation-token routes. App JWTs are verified
// against the public key; installation tokens are accepted only if the fake minted them.
type fakeGitHub struct {
	t     *testing.T
	key   *rsa.PrivateKey
	clock *testClock
	ttl   time.Duration

	installFail *fakeFailure         // set before serving
	mintFail    *fakeFailure         // set before serving
	routes      map[string]fakeRoute // "METHOD /path" -> handler

	mu       sync.Mutex
	installs int
	mints    []mintRequest
	grants   map[string]grant
	claims   map[string]any // claims of the most recent verified app JWT
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if strings.HasPrefix(bearer, "ghs_") {
		f.mu.Lock()
		g, granted := f.grants[bearer]
		f.mu.Unlock()
		route, ok := f.routes[r.Method+" "+r.URL.Path]
		switch {
		case !granted:
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		case !ok:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		default:
			route(w, r, g)
		}
		return
	}
	claims, err := verifyAppJWT(bearer, &f.key.PublicKey)
	if err != nil {
		f.t.Errorf("app JWT rejected: %v", err)
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.claims = claims
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		f.mu.Lock()
		f.installs++
		f.mu.Unlock()
		if f.installFail != nil {
			writeFailure(w, f.installFail)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%d}`, testInstallation)
	case r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", testInstallation):
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var in mintRequest
		if err := dec.Decode(&in); err != nil {
			http.Error(w, `{"message":"bad body"}`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.mints = append(f.mints, in)
		n := len(f.mints)
		f.mu.Unlock()
		if f.mintFail != nil {
			writeFailure(w, f.mintFail)
			return
		}
		tok := fmt.Sprintf("ghs_token%d", n)
		f.mu.Lock()
		f.grants[tok] = grant{repos: in.Repositories, perms: in.Permissions}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      tok,
			"expires_at": f.clock.Now().Add(f.ttl).UTC().Format(time.RFC3339),
		})
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

func (f *fakeGitHub) installCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installs
}

func (f *fakeGitHub) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mints)
}

func (f *fakeGitHub) lastMint() mintRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mints) == 0 {
		return mintRequest{}
	}
	return f.mints[len(f.mints)-1]
}

func (f *fakeGitHub) lastClaims() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

func writeFailure(w http.ResponseWriter, fl *fakeFailure) {
	w.WriteHeader(fl.status)
	_, _ = w.Write([]byte(fl.body))
}

// checkPerms rejects the request unless its token was minted with exactly want.
func checkPerms(w http.ResponseWriter, g grant, want map[string]string) bool {
	if maps.Equal(g.perms, want) {
		return true
	}
	http.Error(w, fmt.Sprintf("token perms %v, want %v", g.perms, want), http.StatusForbidden)
	return false
}

// verifyAppJWT checks an RS256 JWT against pub and returns its claims.
func verifyAppJWT(tok string, pub *rsa.PublicKey) (map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("want three JWT segments")
	}
	var hdr map[string]string
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return nil, err
	}
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" {
		return nil, fmt.Errorf("unexpected header %v", hdr)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// appEnv is an App pointed at a fakeGitHub that shares its clock.
type appEnv struct {
	app   *App
	fake  *fakeGitHub
	clock *testClock
}

// newAppEnv starts a fake configured by setup (may be nil) and an App pointed at it. Retries sleep
// for no time unless a test replaces App.retry.
func newAppEnv(t *testing.T, setup func(*fakeGitHub)) *appEnv {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	fake := &fakeGitHub{t: t, key: sharedKey(t), clock: clock, ttl: time.Hour, grants: map[string]grant{}}
	if setup != nil {
		setup(fake)
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	app := &App{
		ID:      123,
		Key:     fake.key,
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
		Now:     clock.Now,
		retry:   retryPolicy{sleep: noSleep},
	}
	return &appEnv{app: app, fake: fake, clock: clock}
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestAppParseKeyAcceptsPKCS1AndPKCS8(t *testing.T) {
	want := sharedKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(want)})
	der, err := x509.MarshalPKCS8PrivateKey(want)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, data := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		got, err := ParseAppKey(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.N.Cmp(want.N) != 0 {
			t.Errorf("%s: parsed a different key", name)
		}
	}
	if _, err := ParseAppKey([]byte("not pem")); err == nil {
		t.Error("accepted input without a PEM block")
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAppKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})); err == nil {
		t.Error("accepted a non-RSA PKCS#8 key")
	}
}

func TestAppTokenSendsRS256JWTWithClaims(t *testing.T) {
	env := newAppEnv(t, nil)
	if _, err := env.app.Token(context.Background(), "gosuda/0", Read); err != nil {
		t.Fatal(err)
	}
	// The fake verifies the signature with the public key before recording claims.
	claims := env.fake.lastClaims()
	if claims == nil {
		t.Fatal("fake recorded no verified JWT")
	}
	if claims["iss"] != "123" {
		t.Errorf("iss = %v; want 123", claims["iss"])
	}
	now := env.clock.Now()
	wantIat := now.Add(-60 * time.Second).Unix()
	wantExp := now.Add(9 * time.Minute).Unix()
	if got, _ := claims["iat"].(float64); int64(got) != wantIat {
		t.Errorf("iat = %v; want %d", claims["iat"], wantIat)
	}
	if got, _ := claims["exp"].(float64); int64(got) != wantExp {
		t.Errorf("exp = %v; want %d", claims["exp"], wantExp)
	}
}

func TestAppTokenBodyNarrowsRepoAndPermissions(t *testing.T) {
	cases := []struct {
		name   string
		repo   string
		access Access
		perms  map[string]string
	}{
		{"read", "gosuda/0", Read, map[string]string{"contents": "read"}},
		{"write", "gosuda/1", Write, map[string]string{"contents": "write"}},
		{"write-workflows", "mirror/proj", WriteWorkflows, map[string]string{"contents": "write", "workflows": "write"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newAppEnv(t, nil)
			tok, err := env.app.Token(context.Background(), tc.repo, tc.access)
			if err != nil {
				t.Fatal(err)
			}
			_, name, _ := strings.Cut(tc.repo, "/")
			last := env.fake.lastMint()
			if len(last.Repositories) != 1 || last.Repositories[0] != name {
				t.Errorf("repositories = %v; want [%s]", last.Repositories, name)
			}
			if !maps.Equal(last.Permissions, tc.perms) {
				t.Errorf("permissions = %v; want %v", last.Permissions, tc.perms)
			}
			env.fake.mu.Lock()
			g := env.fake.grants[tok]
			env.fake.mu.Unlock()
			if !maps.Equal(g.perms, tc.perms) {
				t.Errorf("issued token perms = %v; want %v", g.perms, tc.perms)
			}
		})
	}
}

func TestAppTokenCachesInstallationAndRefreshesNearExpiry(t *testing.T) {
	env := newAppEnv(t, nil)
	ctx := context.Background()
	get := func(repo string, access Access) string {
		t.Helper()
		tok, err := env.app.Token(ctx, repo, access)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	first := get("gosuda/0", Read)
	if got := get("gosuda/0", Read); got != first {
		t.Fatal("token not reused within its lifetime")
	}
	write := get("gosuda/0", Write)
	if write == first {
		t.Fatal("different permission sets shared a token")
	}
	// Tokens last one hour and are refreshed five minutes early: at t0+55m.
	env.clock.Advance(55*time.Minute - time.Second)
	if got := get("gosuda/0", Read); got != first {
		t.Fatal("token refreshed before the refresh point")
	}
	env.clock.Advance(time.Second)
	if got := get("gosuda/0", Read); got == first {
		t.Fatal("token not refreshed at expires_at minus five minutes")
	}
	if n := env.fake.installCount(); n != 1 {
		t.Errorf("installation lookups = %d; want 1 (cached per repo)", n)
	}
	if n := env.fake.mintCount(); n != 3 {
		t.Errorf("mints = %d; want 3 (read, write, read refreshed)", n)
	}
}

func TestAppTokenFailuresCarryStatusNotToken(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*fakeGitHub)
		status   int
		wantBody string
	}{
		{
			name:     "installation 404",
			setup:    func(f *fakeGitHub) { f.installFail = &fakeFailure{http.StatusNotFound, `{"message":"Not Found"}`} },
			status:   http.StatusNotFound,
			wantBody: "Not Found",
		},
		{
			name: "token 422 echoing token-shaped text",
			setup: func(f *fakeGitHub) {
				f.mintFail = &fakeFailure{http.StatusUnprocessableEntity, `{"message":"Validation Failed","detail":"ghs_abcdef123456"}`}
			},
			status:   http.StatusUnprocessableEntity,
			wantBody: "Validation Failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newAppEnv(t, tc.setup)
			_, err := env.app.Token(context.Background(), "gosuda/0", Read)
			var ae *capy.APIError
			if !errors.As(err, &ae) || ae.Status != tc.status {
				t.Fatalf("err = %v; want APIError with status %d", err, tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantBody) {
				t.Errorf("error %q lacks GitHub's message %q", err, tc.wantBody)
			}
			if strings.Contains(err.Error(), "ghs_") || strings.Contains(err.Error(), "abcdef") {
				t.Errorf("error leaks token material: %v", err)
			}
		})
	}
}

func TestAppTokenRejectsInvalidInput(t *testing.T) {
	env := newAppEnv(t, nil)
	ctx := context.Background()
	if _, err := env.app.Token(ctx, "gosuda", Read); err == nil {
		t.Error("accepted a repository without an owner")
	}
	if _, err := env.app.TokenWithPermissions(ctx, "gosuda/0", nil); err == nil {
		t.Error("accepted an empty permission set")
	}
	if _, err := env.app.Token(ctx, "gosuda/0", Access(9)); err == nil {
		t.Error("accepted an unknown access level")
	}
	if n := env.fake.installCount(); n != 0 {
		t.Errorf("invalid input reached GitHub %d times", n)
	}
}
