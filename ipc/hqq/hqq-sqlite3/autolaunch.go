package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"gosuda.org/sqlite3/internal/embedded"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// ServerBinary materializes the embedded prebuilt hqq-sqlite-server binary for
// the current OS and architecture, verifies its SHA-256 digest against
// ExpectedHash(runtime.GOOS, runtime.GOARCH), and returns its filesystem path
// and verified digest.
//
// If no embedded binary is available for the current platform, ServerBinary
// falls back to the HQQ_SQLITE_SERVER or HQQ_SQLITE3GEN_SERVER environment
// variable when set.
func ServerBinary() (string, secureexec.Hash, error) {
	if expected, ok := ExpectedHash(runtime.GOOS, runtime.GOARCH); ok {
		path, err := embedded.Materialize(expected)
		if err == nil {
			return path, expected, nil
		}
	}
	for _, env := range []string{"HQQ_SQLITE_SERVER", "HQQ_SQLITE3GEN_SERVER"} {
		if p := os.Getenv(env); p != "" {
			h, _, err := secureexec.HashFile(p)
			if err != nil {
				return "", secureexec.Hash{}, fmt.Errorf("sqlite: hash %s (%s): %w", env, p, err)
			}
			return p, h, nil
		}
	}
	return "", secureexec.Hash{}, fmt.Errorf("sqlite: no embedded hqq-sqlite-server binary for %s/%s", runtime.GOOS, runtime.GOARCH)
}

type autoServerEntry struct {
	srv  *launch.Server
	refs int
}

var (
	autoServersMu sync.Mutex
	autoServers   = make(map[string]*autoServerEntry)
)

func canonicalAllowDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

func acquireAutoServer(ctx context.Context, allowDir string) (string, func() error, error) {
	canonDir, err := canonicalAllowDir(allowDir)
	if err != nil {
		return "", nil, fmt.Errorf("sqlite: resolve allow directory: %w", err)
	}

	autoServersMu.Lock()
	if e, ok := autoServers[canonDir]; ok {
		select {
		case <-e.srv.Done():
			delete(autoServers, canonDir)
		default:
			e.refs++
			listener := e.srv.Listener()
			autoServersMu.Unlock()
			return listener, makeReleaseFunc(canonDir, e), nil
		}
	}
	autoServersMu.Unlock()

	binPath, hash, err := ServerBinary()
	if err != nil {
		return "", nil, err
	}
	listener, err := launch.NewListenerName("hqqsql")
	if err != nil {
		return "", nil, err
	}
	srv, err := launch.Start(ctx, launch.Config{
		Binary:   binPath,
		SHA256:   hash,
		Listener: listener,
		Args:     []string{listener, "--allow-dir", canonDir},
	})
	if err != nil {
		return "", nil, fmt.Errorf("sqlite: start embedded server: %w", err)
	}

	autoServersMu.Lock()
	if existing, ok := autoServers[canonDir]; ok {
		select {
		case <-existing.srv.Done():
			delete(autoServers, canonDir)
		default:
			existing.refs++
			l := existing.srv.Listener()
			autoServersMu.Unlock()
			_ = srv.Close()
			return l, makeReleaseFunc(canonDir, existing), nil
		}
	}
	entry := &autoServerEntry{srv: srv, refs: 1}
	autoServers[canonDir] = entry
	autoServersMu.Unlock()

	return srv.Listener(), makeReleaseFunc(canonDir, entry), nil
}

func makeReleaseFunc(canonDir string, entry *autoServerEntry) func() error {
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() {
			autoServersMu.Lock()
			entry.refs--
			shouldClose := entry.refs <= 0
			if shouldClose && autoServers[canonDir] == entry {
				delete(autoServers, canonDir)
			}
			autoServersMu.Unlock()
			if shouldClose {
				closeErr = entry.srv.Close()
			}
		})
		return closeErr
	}
}
