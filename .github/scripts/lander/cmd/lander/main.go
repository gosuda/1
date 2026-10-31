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
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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

	appIDStr, keyFile := os.Getenv("LANDER_APP_ID"), os.Getenv("LANDER_APP_KEY_FILE")
	if appIDStr == "" || keyFile == "" {
		fatalf("LANDER_APP_ID and LANDER_APP_KEY_FILE are required")
	}
	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		fatalf("LANDER_APP_ID: %v", err)
	}
	pem, err := os.ReadFile(keyFile)
	if err != nil {
		fatalf("read app key: %v", err)
	}
	key, err := lander.ParseAppKey(pem)
	if err != nil {
		fatalf("parse app key: %v", err)
	}
	app := &lander.App{ID: appID, Key: key, BaseURL: apiURL, HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now}

	store := &lander.Store{Dir: env("LANDER_STORE", "/var/lib/lander/store.git"), Tokens: app, Log: log}
	if k := os.Getenv("LANDER_SIGNING_KEY"); k != "" {
		signers := os.Getenv("LANDER_ALLOWED_SIGNERS")
		if signers == "" {
			fatalf("LANDER_SIGNING_KEY requires LANDER_ALLOWED_SIGNERS")
		}
		store.Signing = &lander.Signing{KeyFile: k, AllowedSigners: signers}
	}

	cfg := lander.Config{
		Store: store,
		Forge: lander.NewGitHubForge(app, userRepo),
		Trunk: lander.Remote{URL: ghURL + "/" + trunkRepo + ".git", Repo: trunkRepo},
		User:  lander.Remote{URL: ghURL + "/" + userRepo + ".git", Repo: userRepo},
		Mirror: func(repo string) lander.Remote {
			return lander.Remote{URL: ghURL + "/" + repo + ".git", Repo: repo}
		},
		AckTip: os.Getenv("LANDER_ACK_TIP"),
		DryRun: os.Getenv("LANDER_DRYRUN") != "",
		Log:    log,
	}
	if env("LANDER_PRESUBMIT", "docker") == "docker" {
		cfg.Presubmit = &lander.DockerPresubmit{
			Image:     env("LANDER_PRESUBMIT_IMAGE", "golang:1.27"),
			Docker:    env("LANDER_DOCKER", "docker"),
			Timeout:   envDur("LANDER_PRESUBMIT_TIMEOUT", 15*time.Minute),
			CPUs:      env("LANDER_PRESUBMIT_CPUS", "2"),
			Memory:    env("LANDER_PRESUBMIT_MEMORY", "4g"),
			PidsLimit: 1024,
			CacheDir:  env("LANDER_CACHE_DIR", "/var/lib/lander/cache"),
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
		if err := check(ctx, app, cfg, trunkRepo, userRepo); err != nil {
			fatalf("check failed: %v", err)
		}
		fmt.Println("ok")
	case "once":
		if err := l.Cycle(ctx); err != nil {
			fatalf("%v", err)
		}
	case "run":
		wake := make(chan struct{}, 1)
		go serve(ctx, log, env("LANDER_LISTEN", ":8080"), os.Getenv("LANDER_WAKE_SECRET"), wake)
		if err := l.Run(ctx, envDur("LANDER_INTERVAL", 15*time.Second), wake); err != nil && ctx.Err() == nil {
			fatalf("%v", err)
		}
	}
}

// check proves the deployment can act: tools exist and every repository yields a token.
func check(ctx context.Context, app *lander.App, cfg lander.Config, repos ...string) error {
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
	return nil
}

// serve exposes /healthz and an optional HMAC-verified /wake. A wake only shortens the wait for
// the next cycle; its payload is never read as input.
func serve(ctx context.Context, log *slog.Logger, addr, secret string, wake chan<- struct{}) {
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
		select {
		case wake <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	})
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
