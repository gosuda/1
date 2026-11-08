package client_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/chromenet/client"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

func TestServerBinary(t *testing.T) {
	path, hash, err := client.ServerBinary()
	if err != nil {
		t.Fatalf("ServerBinary() failed: %v", err)
	}
	if path == "" {
		t.Fatal("ServerBinary() returned empty path")
	}

	expected, ok := client.ExpectedHash(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Fatalf("ExpectedHash(%s, %s) returned false", runtime.GOOS, runtime.GOARCH)
	}
	if hash != expected {
		t.Fatalf("hash mismatch: got %s, want %s", hash, expected)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat server binary: %v", err)
	}
	if fi.IsDir() {
		t.Fatalf("server binary %s is a directory", path)
	}
	if fi.Mode()&0111 == 0 {
		t.Fatalf("server binary %s is not executable (mode %v)", path, fi.Mode())
	}

	gotHash, _, err := secureexec.HashFile(path)
	if err != nil {
		t.Fatalf("HashFile(%s): %v", path, err)
	}
	if gotHash != expected {
		t.Fatalf("materialized file hash mismatch: got %s, want %s", gotHash, expected)
	}
}

func TestEnvVarOverride(t *testing.T) {
	origPath, _, err := client.ServerBinary()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(origPath)
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	overridePath := filepath.Join(tmpDir, filepath.Base(origPath))
	if err := os.WriteFile(overridePath, data, 0700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HQQ_CHROME_NET_SERVER", overridePath)

	path, hash, err := client.ServerBinary()
	if err != nil {
		t.Fatalf("ServerBinary() with override failed: %v", err)
	}
	if path != overridePath {
		t.Fatalf("ServerBinary() = %s; want %s", path, overridePath)
	}

	expectedHash := secureexec.Hash(sha256.Sum256(data))
	if hash != expectedHash {
		t.Fatalf("hash = %s; want %s", hash, expectedHash)
	}

	// Verify server starts and accepts calls via the overridden binary
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv, err := client.StartServer(ctx)
	if err != nil {
		t.Fatalf("StartServer via override: %v", err)
	}
	defer srv.Close()

	cl, err := client.Dial(ctx, srv.Listener(), client.DefaultOptions())
	if err != nil {
		t.Fatalf("Dial override server: %v", err)
	}
	defer cl.Close()

	eng, err := cl.EngineCreate(nil)
	if err != nil {
		t.Fatalf("EngineCreate via override server: %v", err)
	}
	if eng.IsNil() {
		t.Fatal("expected non-nil Engine handle")
	}
	if err := cl.EngineDestroy(eng); err != nil {
		t.Fatalf("EngineDestroy: %v", err)
	}
}

func TestTamperedBinaryRejected(t *testing.T) {
	origPath, origHash, err := client.ServerBinary()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(origPath)
	if err != nil {
		t.Fatal(err)
	}

	// Flip a byte in the executable
	if len(data) > 100 {
		data[100] ^= 0xFF
	} else {
		data = append(data, 0xFF)
	}

	tamperedPath := filepath.Join(t.TempDir(), filepath.Base(origPath))
	if err := os.WriteFile(tamperedPath, data, 0700); err != nil {
		t.Fatal(err)
	}

	// secureexec.Verify should reject with HashMismatchError
	_, err = secureexec.Verify(tamperedPath, origHash)
	if err == nil {
		t.Fatal("expected HashMismatchError for tampered binary, got nil")
	}
	var mismatch *secureexec.HashMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected HashMismatchError, got: %T %v", err, err)
	}

	// launch.Start should reject execution
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = launch.Start(ctx, launch.Config{
		Binary:   tamperedPath,
		SHA256:   origHash,
		Listener: "tampered_listener",
	})
	if err == nil {
		t.Fatal("expected error starting tampered binary, got nil")
	}
}

func TestAutolaunchLifecycle(t *testing.T) {
	client.ResetSharedServerForTesting()
	t.Cleanup(func() {
		_ = client.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cl, err := client.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cl.Close()

	// Perform RPC round-trip: EngineCreate and EngineDestroy
	eng, err := cl.EngineCreate(nil)
	if err != nil {
		t.Fatalf("EngineCreate: %v", err)
	}
	if eng.IsNil() {
		t.Fatal("expected non-nil Engine handle")
	}
	if err := cl.EngineDestroy(eng); err != nil {
		t.Fatalf("EngineDestroy: %v", err)
	}

	if err := client.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestParentLifeline(t *testing.T) {
	binPath, _, err := client.ServerBinary()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := launch.NewListenerName("lifeline")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binPath, listener, "--parent-stdin")
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}

	// Read ready line from stdout
	r := bufio.NewReader(stdoutPipe)
	line, err := r.ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("read ready line: %v", err)
	}
	if !strings.HasPrefix(line, "ready") {
		_ = cmd.Process.Kill()
		t.Fatalf("expected ready line, got: %q", line)
	}

	// Close stdin to trigger graceful shutdown on EOF
	_ = stdinPipe.Close()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		t.Logf("child exited cleanly upon stdin EOF: %v", err)
	case <-time.After(4 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("server process did not exit within 4 seconds after stdin closed")
	}
}

func TestClientPool(t *testing.T) {
	client.ResetSharedServerForTesting()
	t.Cleanup(func() {
		_ = client.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	pool := client.NewClientPool(4, client.DefaultOptions())
	defer pool.Close()

	const concurrent = 8
	var wg sync.WaitGroup
	wg.Add(concurrent)
	errCh := make(chan error, concurrent)

	for i := 0; i < concurrent; i++ {
		go func() {
			defer wg.Done()
			c, err := pool.Get(ctx)
			if err != nil {
				errCh <- err
				return
			}
			defer pool.Put(c)

			eng, err := c.EngineCreate(nil)
			if err != nil {
				errCh <- err
				return
			}
			if eng.IsNil() {
				errCh <- errors.New("invalid engine handle")
				return
			}
			if err := c.EngineDestroy(eng); err != nil {
				errCh <- err
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent pool operation error: %v", err)
	}
}
