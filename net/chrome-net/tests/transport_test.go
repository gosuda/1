package tests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/chrome-net/client"
)

func TestTransport_ImplementsRoundTripper(t *testing.T) {
	tr := client.NewTransport(client.Options{})
	defer tr.Close()

	var _ http.RoundTripper = tr
	if tr == nil {
		t.Fatal("expected non-nil Transport")
	}
}

func TestTransport_HttpClientIntegration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestTransport_StreamingResponseBody(t *testing.T) {
	expectedPayload := []byte("STREAMING_RESPONSE_PAYLOAD_CHUNKED_0123456789_END")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		chunkSize := 10
		for i := 0; i < len(expectedPayload); i += chunkSize {
			end := i + chunkSize
			if end > len(expectedPayload) {
				end = len(expectedPayload)
			}
			_, _ = w.Write(expectedPayload[i:end])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	c := &http.Client{Transport: tr}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 10)
	var accumulated []byte
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			accumulated = append(accumulated, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("resp.Body.Read: %v", err)
		}
	}

	if !bytes.Equal(accumulated, expectedPayload) {
		t.Fatalf("body mismatch: got %q, want %q", string(accumulated), string(expectedPayload))
	}
}

func TestTransport_ContextCancellation(t *testing.T) {
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(blockCh)
		srv.Close()
	}()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	ctx, cancel := context.WithCancel(context.Background())

	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := tr.RoundTrip(req)
		errCh <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error on canceled context, got nil")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("expected context.Canceled error, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RoundTrip did not return after context cancellation")
	}
}

func TestTransport_ContextTimeoutEnforcement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	// Pre-expired context
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-10*time.Millisecond))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = tr.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error with pre-expired context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
}

func TestTransport_HeaderPreservation(t *testing.T) {
	var capturedHeader http.Header
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedHeader = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	customUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Custom/1.0"
	tr := client.NewTransport(client.Options{
		UserAgent:          customUA,
		InsecureSkipVerify: true,
	})
	defer tr.Close()

	req, err := http.NewRequest("GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Token", "token-xyz-123")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()

	if got := capturedHeader.Get("User-Agent"); got != customUA {
		t.Errorf("User-Agent = %q; want %q", got, customUA)
	}
	if got := capturedHeader.Get("X-Test-Token"); got != "token-xyz-123" {
		t.Errorf("X-Test-Token = %q; want token-xyz-123", got)
	}
	if got := capturedHeader.Get("Sec-Ch-Ua"); got != client.DefaultSecChUa {
		t.Errorf("Sec-Ch-Ua = %q; want %q", got, client.DefaultSecChUa)
	}
	if got := capturedHeader.Get("Accept-Encoding"); got != client.DefaultAcceptEncoding {
		t.Errorf("Accept-Encoding = %q; want %q", got, client.DefaultAcceptEncoding)
	}
}

func TestTransport_ClosedTransportRejection(t *testing.T) {
	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req, err := http.NewRequest("GET", "http://127.0.0.1:12345/test", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = tr.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error on closed transport, got nil")
	}
	if !strings.Contains(err.Error(), "transport closed") {
		t.Fatalf("expected error containing 'transport closed', got: %v", err)
	}
}

func TestTransport_ConcurrentRoundTrips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("concurrent-ok"))
	}))
	defer srv.Close()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	c := &http.Client{Transport: tr}

	const concurrent = 20
	var wg sync.WaitGroup
	wg.Add(concurrent)
	errCh := make(chan error, concurrent)

	for i := 0; i < concurrent; i++ {
		go func(id int) {
			defer wg.Done()
			req, err := http.NewRequest("GET", fmt.Sprintf("%s/test?id=%d", srv.URL, id), nil)
			if err != nil {
				errCh <- err
				return
			}
			resp, err := c.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errCh <- err
				return
			}
			if string(body) != "concurrent-ok" {
				errCh <- fmt.Errorf("unexpected body: %q", string(body))
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent RoundTrip error: %v", err)
	}
}

func TestTransport_HQQEngineLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cl, err := client.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cl.Close()

	eng, err := cl.EngineCreate(nil)
	if err != nil {
		t.Fatalf("EngineCreate: %v", err)
	}
	if eng.IsNil() {
		t.Fatal("expected non-nil Engine handle")
	}
	defer func() {
		if err := cl.EngineDestroy(eng); err != nil {
			t.Errorf("EngineDestroy: %v", err)
		}
	}()
}

func TestTransport_HQQStreamingResponseBody(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	targetURL := "hqq://chrome-net-engine/stream-chunk-test"
	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	// 1. Verify response status
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d; want %d", resp.StatusCode, http.StatusOK)
	}

	// 2. Verify parsed response headers
	if got := resp.Header.Get("Server"); got != "chrome-net-engine" {
		t.Errorf("Server header = %q; want %q", got, "chrome-net-engine")
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type header = %q; want %q", got, "text/plain; charset=utf-8")
	}
	if got := resp.Header.Get("Alt-Svc"); got == "" {
		t.Errorf("expected non-empty Alt-Svc header, got empty")
	}

	// 3. Read body in small chunks to test streaming chunk accumulation
	chunkBuf := make([]byte, 5)
	var accumulated []byte
	for {
		n, rErr := resp.Body.Read(chunkBuf)
		if n > 0 {
			accumulated = append(accumulated, chunkBuf[:n]...)
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			t.Fatalf("resp.Body.Read: %v", rErr)
		}
	}

	expectedBody := "OK: GET " + targetURL
	if string(accumulated) != expectedBody {
		t.Fatalf("body mismatch: got %q, want %q", string(accumulated), expectedBody)
	}

	// 4. Verify Close() cleans up handles and returns client to pool
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("resp.Body.Close: %v", err)
	}

	// 5. Verify Close() idempotency
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("second resp.Body.Close: %v", err)
	}

	// 6. Verify Read() on closed body returns error
	if _, err := resp.Body.Read(chunkBuf); err == nil {
		t.Fatal("expected error reading from closed body, got nil")
	}
}

func TestTransport_HQQStreamingResponseBody_PoolLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Dedicated pool with max = 1 to verify client lock and recycle lifecycle
	pool := client.NewClientPool(1, client.DefaultOptions())
	defer pool.Close()

	tr := client.NewTransport(client.Options{
		InsecureSkipVerify: true,
		Pool:               pool,
	})
	defer tr.Close()

	req1, err := http.NewRequestWithContext(ctx, "GET", "hqq://chrome-net-engine/pool-lifecycle-1", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext 1: %v", err)
	}

	resp1, err := tr.RoundTrip(req1)
	if err != nil {
		t.Fatalf("RoundTrip 1: %v", err)
	}

	// While resp1.Body is open, the pool's single client is checked out and owned by the body reader.
	// An attempt to Get with a short deadline must time out since pool limit is 1.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err = pool.Get(shortCtx)
	shortCancel()
	if err == nil {
		t.Fatal("expected pool.Get to fail with timeout while body reader owns client, but got client")
	}

	// Read body to EOF
	bodyBytes, err := io.ReadAll(resp1.Body)
	if err != nil {
		t.Fatalf("io.ReadAll resp1: %v", err)
	}
	if !strings.Contains(string(bodyBytes), "pool-lifecycle-1") {
		t.Fatalf("unexpected body: %q", string(bodyBytes))
	}

	// Close the response body — must destroy handles and return client to pool
	if err := resp1.Body.Close(); err != nil {
		t.Fatalf("resp1.Body.Close: %v", err)
	}

	// Immediately execute a second request; it must succeed using the recycled pool client
	req2, err := http.NewRequestWithContext(ctx, "GET", "hqq://chrome-net-engine/pool-lifecycle-2", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext 2: %v", err)
	}

	resp2, err := tr.RoundTrip(req2)
	if err != nil {
		t.Fatalf("RoundTrip 2 using recycled client failed: %v", err)
	}
	defer resp2.Body.Close()

	bodyBytes2, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("io.ReadAll resp2: %v", err)
	}
	if !strings.Contains(string(bodyBytes2), "pool-lifecycle-2") {
		t.Fatalf("unexpected body 2: %q", string(bodyBytes2))
	}
}

