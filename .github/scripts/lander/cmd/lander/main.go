// Command lander is the single writer of gosuda/0.
//
//	lander run     cycle forever (default)
//	lander once    run one cycle and exit
//	lander check   validate configuration, credentials and tools, then exit
//
// Configuration comes from LANDER_* environment variables (see README.md).
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gosuda.org/0/scripts/lander"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool parses a boolean variable strictly: unset or empty is false, and anything that is not a
// valid boolean is a startup error (a lenient "non-empty means true" would turn LANDER_DRYRUN=0 into a dry run).
func envBool(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		fatalf("%s: %q is not a boolean (use 1/true or 0/false)", key, v)
	}
	return b
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fatalf("%s: %v", key, err)
		}
		return d
	}
	return def
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "lander: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd != "run" && cmd != "once" && cmd != "check" {
		fatalf("unknown command %q (run|once|check)", cmd)
	}

	level := slog.LevelInfo
	if os.Getenv("LANDER_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ghURL := strings.TrimRight(env("LANDER_GITHUB_URL", "https://github.com"), "/")
	apiURL := env("LANDER_API_URL", "https://api.github.com")
	trunkRepo := env("LANDER_TRUNK_REPO", "gosuda/0")
	userRepo := env("LANDER_USER_REPO", "gosuda/1")

	appIDStr := os.Getenv("LANDER_APP_ID")
	pem, err := secret("LANDER_APP_KEY_FILE", "LANDER_APP_KEY")
	if appIDStr == "" {
		fatalf("LANDER_APP_ID is required")
	}
	if err != nil {
		fatalf("app key (LANDER_APP_KEY_FILE or LANDER_APP_KEY): %v", err)
	}
	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		fatalf("LANDER_APP_ID: %v", err)
	}
	key, err := lander.ParseAppKey(pem)
	if err != nil {
		fatalf("parse app key: %v", err)
	}
	app := &lander.App{ID: appID, Key: key, BaseURL: apiURL, HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now}

	store := &lander.Store{Dir: env("LANDER_STORE", "/var/lib/lander/data/store.git"), Tokens: app, Log: log}
	if sk, err := secret("LANDER_SIGNING_KEY_FILE", "LANDER_SIGNING_KEY"); err == nil {
		signing, err := materializeSigning(sk, os.Getenv("LANDER_ALLOWED_SIGNERS"))
		if err != nil {
			fatalf("signing key: %v", err)
		}
		store.Signing = signing
	}

	cfg := lander.Config{
		Store: store,
		Forge: lander.NewGitHubForge(app, userRepo),
		Trunk: lander.Remote{URL: ghURL + "/" + trunkRepo + ".git", Repo: trunkRepo},
		User:  lander.Remote{URL: ghURL + "/" + userRepo + ".git", Repo: userRepo},
		Mirror: func(repo string) lander.Remote {
			return lander.Remote{URL: ghURL + "/" + repo + ".git", Repo: repo}
		},
		AckTip:         os.Getenv("LANDER_ACK_TIP"),
		MirrorInterval: envDur("LANDER_MIRROR_INTERVAL", 5*time.Minute),
		DryRun:         os.Getenv("LANDER_DRYRUN") != "",
		Log:            log,
	}
	if env("LANDER_PRESUBMIT", "docker") == "docker" {
		cfg.Presubmit = &lander.DockerPresubmit{
			Image:     env("LANDER_PRESUBMIT_IMAGE", "golang:1.27"),
			Docker:    env("LANDER_DOCKER", "docker"),
			Timeout:   envDur("LANDER_PRESUBMIT_TIMEOUT", 15*time.Minute),
			CPUs:      env("LANDER_PRESUBMIT_CPUS", "2"),
			Memory:    env("LANDER_PRESUBMIT_MEMORY", "4g"),
			PidsLimit: 1024,
			CacheDir:  env("LANDER_CACHE_DIR", "/var/lib/lander/data/cache"),
			Log:       log,
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Init(ctx); err != nil {
		fatalf("init store: %v", err)
	}
	l, err := lander.New(cfg)
	if err != nil {
		fatalf("%v", err)
	}

	switch cmd {
	case "check":
		if err := check(ctx, app, apiURL, trunkRepo, userRepo); err != nil {
			fatalf("check failed: %v", err)
		}
		fmt.Println("ok")
	case "once":
		if err := l.Cycle(ctx); err != nil {
			fatalf("%v", err)
		}
	case "run":
		wake := make(chan struct{}, 1)
		go serve(ctx, log, env("LANDER_LISTEN", ":8080"), os.Getenv("LANDER_WAKE_SECRET"), wake, l.NoteWake)
		if err := l.Run(ctx, envDur("LANDER_INTERVAL", time.Minute), wake); err != nil && ctx.Err() == nil {
			fatalf("%v", err)
		}
	}
}

// check proves the deployment can act: tools exist, every repository yields a write token, and
// the narrowed read tokens the identity lookups use are accepted by the GitHub APIs they call
// (activity for pushers, org teams for @org/team OWNERS entries).
func check(ctx context.Context, app *lander.App, apiURL string, repos ...string) error {
	for _, tool := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found", tool)
		}
	}
	for _, r := range repos {
		if _, err := app.Token(ctx, r, lander.Write); err != nil {
			return fmt.Errorf("token for %s: %w", r, err)
		}
	}
	userRepo := repos[len(repos)-1]
	org, _, _ := strings.Cut(userRepo, "/")
	probe := func(perms map[string]string, path string) error {
		tok, err := app.TokenWithPermissions(ctx, userRepo, perms)
		if err != nil {
			return fmt.Errorf("mint %v token: %w", perms, err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiURL, "/")+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			return fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		return nil
	}
	if err := probe(map[string]string{"contents": "read"}, "/repos/"+userRepo+"/activity?per_page=1"); err != nil {
		return err
	}
	if err := probe(map[string]string{"pull_requests": "read"}, "/repos/"+userRepo+"/pulls?state=all&per_page=1"); err != nil {
		return err
	}
	return probe(map[string]string{"members": "read"}, "/orgs/"+org+"/teams?per_page=1")
}

// serve exposes /healthz and an optional HMAC-verified /wake. A wake only shortens the wait for
// the next cycle; its payload is never read as input.
func wakeHandler(secret string, wake chan<- struct{}, note func(repo string) bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		if secret == "" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(want), []byte(r.Header.Get("X-Hub-Signature-256"))) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		// The body is only a hint about which repository changed (GitHub webhook or magpie).
		var hint struct {
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		_ = json.Unmarshal(body, &hint)
		if !note(hint.Repository.FullName) {
			w.WriteHeader(http.StatusAccepted) // signed, but not a repository the lander watches
			return
		}
		select {
		case wake <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}

func serve(ctx context.Context, log *slog.Logger, addr, secret string, wake chan<- struct{}, note func(repo string) bool) {
	mux := wakeHandler(secret, wake, note)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http server", "err", err)
	}
}

// secret reads a credential from a file (fileVar) or, for platforms that only offer environment
// variables, from the variable valueVar holding the content. Platforms mangle multi-line values in
// several ways, so the content is normalized (see normalizePEM).
func secret(fileVar, valueVar string) ([]byte, error) {
	if p := os.Getenv(fileVar); p != "" {
		return os.ReadFile(p)
	}
	if v := os.Getenv(valueVar); v != "" {
		return normalizePEM(v)
	}
	return nil, fmt.Errorf("neither %s nor %s is set", fileVar, valueVar)
}

var pemRe = regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]+)-----([\s\S]*?)-----END ([A-Z0-9 ]+)-----`)

// normalizePEM repairs the usual ways an env var mangles a PEM: surrounding quotes, literal "\n"
// sequences, newlines replaced by spaces, CRLF, lost line breaks, or the whole PEM base64-encoded.
// It returns an error describing the shape of the input (never its content) when no PEM is found.
func normalizePEM(raw string) ([]byte, error) {
	v := strings.TrimSpace(raw)
	v = strings.Trim(v, "\"'")
	v = strings.ReplaceAll(v, "\\r\\n", "\n")
	v = strings.ReplaceAll(v, "\\n", "\n")
	if !strings.Contains(v, "-----BEGIN") {
		if dec, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(v), "")); err == nil && strings.Contains(string(dec), "-----BEGIN") {
			v = string(dec)
		}
	}
	m := pemRe.FindStringSubmatch(v)
	if m == nil || m[1] != m[3] {
		return nil, fmt.Errorf("value has no PEM block (length %d, contains BEGIN marker: %t, contains END marker: %t)",
			len(raw), strings.Contains(raw, "-----BEGIN"), strings.Contains(raw, "-----END"))
	}
	body := strings.Join(strings.Fields(m[2]), "")
	width := 64
	if m[1] == "OPENSSH PRIVATE KEY" {
		width = 70
	}
	var b strings.Builder
	fmt.Fprintf(&b, "-----BEGIN %s-----\n", m[1])
	for len(body) > 0 {
		n := min(width, len(body))
		b.WriteString(body[:n] + "\n")
		body = body[n:]
	}
	fmt.Fprintf(&b, "-----END %s-----\n", m[1])
	return []byte(b.String()), nil
}

// materializeSigning writes the signing key to a private temp file (ssh-keygen and git need a
// path) and, unless an allowed-signers file is configured, derives one from the key's public half:
// the lander verifies only its own signatures.
func materializeSigning(privateKey []byte, allowedSigners string) (*lander.Signing, error) {
	dir, err := os.MkdirTemp("", "lander-signing-*")
	if err != nil {
		return nil, err
	}
	keyPath := dir + "/key"
	if err := os.WriteFile(keyPath, privateKey, 0o600); err != nil {
		return nil, err
	}
	if allowedSigners == "" {
		pub, err := exec.Command("ssh-keygen", "-y", "-f", keyPath).Output()
		if err != nil {
			return nil, fmt.Errorf("derive public key: %w", err)
		}
		allowedSigners = dir + "/allowed_signers"
		line := "lander@gosuda.org " + strings.TrimSpace(string(pub)) + "\n"
		if err := os.WriteFile(allowedSigners, []byte(line), 0o600); err != nil {
			return nil, err
		}
	}
	return &lander.Signing{KeyFile: keyPath, AllowedSigners: allowedSigners}, nil
}
