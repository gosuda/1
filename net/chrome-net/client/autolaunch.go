package client

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"gosuda.org/chrome-net/internal/embedded"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// ExpectedHash returns the pinned SHA-256 digest of chrome_net_server for goos/goarch.
func ExpectedHash(goos, goarch string) (secureexec.Hash, bool) {
	return embedded.ExpectedHash(goos, goarch)
}

// ServerBinary materializes the embedded prebuilt chrome_net_server binary for the
// current OS and architecture, verifies its SHA-256 digest against
// ExpectedHash(runtime.GOOS, runtime.GOARCH), and returns its filesystem path
// and verified digest.
//
// If HQQ_CHROME_NET_SERVER is set, it overrides the embedded binary and returns
// the path and computed SHA-256 digest of the specified executable.
func ServerBinary() (string, secureexec.Hash, error) {
	if p := os.Getenv("HQQ_CHROME_NET_SERVER"); p != "" {
		h, _, err := secureexec.HashFile(p)
		if err != nil {
			return "", secureexec.Hash{}, fmt.Errorf("chrome-net: hash HQQ_CHROME_NET_SERVER (%s): %w", p, err)
		}
		return p, h, nil
	}
	if expected, ok := ExpectedHash(runtime.GOOS, runtime.GOARCH); ok {
		path, err := embedded.Materialize(expected)
		if err == nil {
			return path, expected, nil
		}
		return "", secureexec.Hash{}, fmt.Errorf("chrome-net: materialize embedded binary (%s/%s): %w", runtime.GOOS, runtime.GOARCH, err)
	}
	return "", secureexec.Hash{}, fmt.Errorf("chrome-net: no embedded chrome_net_server binary for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// StartServer verifies the server binary with secureexec and starts an isolated
// worker process bound to the parent process lifetime via --parent-stdin.
func StartServer(ctx context.Context, args ...string) (*launch.Server, error) {
	binPath, hash, err := ServerBinary()
	if err != nil {
		return nil, err
	}
	listener, err := launch.NewListenerName("chromenet")
	if err != nil {
		return nil, err
	}
	serverArgs := append([]string{listener}, args...)
	return launch.Start(ctx, launch.Config{
		Binary:   binPath,
		SHA256:   hash,
		Listener: listener,
		Args:     serverArgs,
	})
}

var (
	sharedMu   sync.Mutex
	sharedSrv  *launch.Server
	sharedPool = rt.NewPool(16, func(ctx context.Context) (*Client, error) {
		return Open(ctx)
	}, func(c *Client) error {
		return c.Close()
	})
)

// EnsureRunning guarantees that a shared server process is active and ready
// to receive connections on its shared memory listener.
// The optional opts parameter is reserved for future server tuning.
func EnsureRunning(ctx context.Context, opts ...rt.Options) (string, error) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedSrv != nil {
		select {
		case <-sharedSrv.Done():
			sharedSrv = nil
		default:
			return sharedSrv.Listener(), nil
		}
	}
	srv, err := StartServer(ctx)
	if err != nil {
		return "", err
	}
	sharedSrv = srv
	return srv.Listener(), nil
}

// DefaultOptions returns the baseline link & buffer settings for chrome_net sessions.
func DefaultOptions() rt.Options {
	return rt.Options{
		BufferCount: 64,
		BufferSize:  64 * 1024, // 64 KiB
		MaxInFlight: 64,
		MaxMessage:  16 * 1024 * 1024,
	}
}

// Open connects a new *Client session to the automatically managed embedded
// chrome_net_server worker process using DefaultOptions().
func Open(ctx context.Context) (*Client, error) {
	return OpenWith(ctx, DefaultOptions())
}

// OpenWith connects a new *Client session to the automatically managed embedded
// chrome_net_server worker process using custom rt.Options.
func OpenWith(ctx context.Context, opts rt.Options) (*Client, error) {
	listener, err := EnsureRunning(ctx, opts)
	if err != nil {
		return nil, err
	}
	cl, err := Dial(ctx, listener, opts)
	if err != nil {
		sharedMu.Lock()
		if sharedSrv != nil && sharedSrv.Listener() == listener {
			select {
			case <-sharedSrv.Done():
				sharedSrv = nil
			default:
			}
		}
		sharedMu.Unlock()
		return nil, err
	}
	return cl, nil
}

// DefaultPool returns the process-wide client connection pool.
func DefaultPool() *rt.Pool[*Client] {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	return sharedPool
}

// NewClientPool returns a dedicated client connection pool with at most max connections.
func NewClientPool(max int, opts rt.Options) *rt.Pool[*Client] {
	return rt.NewPool(max, func(ctx context.Context) (*Client, error) {
		return OpenWith(ctx, opts)
	}, func(c *Client) error {
		return c.Close()
	})
}

// Shutdown terminates the shared background server and closes pooled client sessions.
func Shutdown() error {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	_ = sharedPool.Close()
	sharedPool = rt.NewPool(16, func(ctx context.Context) (*Client, error) {
		return Open(ctx)
	}, func(c *Client) error {
		return c.Close()
	})
	if sharedSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := sharedSrv.Stop(ctx)
		sharedSrv = nil
		return err
	}
	return nil
}

// ResetSharedServerForTesting stops and clears the shared server instance and client pool.
func ResetSharedServerForTesting() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	_ = sharedPool.Close()
	sharedPool = rt.NewPool(16, func(ctx context.Context) (*Client, error) {
		return Open(ctx)
	}, func(c *Client) error {
		return c.Close()
	})
	if sharedSrv != nil {
		_ = sharedSrv.Close()
		sharedSrv = nil
	}
}
