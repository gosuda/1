package tests

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/chromenet/client"
)

// =============================================================================
// 1. Adversarial Edge Cases: Zero-Byte Buffer Reads
// =============================================================================

func TestAdversarial_ZeroByteReads(t *testing.T) {
	// A. Standard bodyReader
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("adversarial-body-data"))
	}))
	defer srv.Close()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	resp, err := tr.RoundTrip(httptest.NewRequest("GET", srv.URL, nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	// 1. Read([]byte{}) on open body must return (0, nil) without consuming data or blocking
	emptyBuf := []byte{}
	n, err := resp.Body.Read(emptyBuf)
	if n != 0 || err != nil {
		t.Fatalf("zero-byte Read on open body: got (%d, %v); want (0, nil)", n, err)
	}

	// Verify data is still intact
	fullData, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(fullData) != "adversarial-body-data" {
		t.Fatalf("body mismatch after zero-byte read: got %q", string(fullData))
	}

	// Close body
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 2. Read([]byte{}) after close must return error containing "closed"
	_, err = resp.Body.Read(emptyBuf)
	if err == nil {
		t.Fatal("expected error on zero-byte read after close, got nil")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected error containing 'closed', got: %v", err)
	}

	// B. HQQ Body Reader
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hqqReq, err := http.NewRequestWithContext(ctx, "GET", "hqq://chrome-net-engine/zero-byte-test", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	hqqResp, err := tr.RoundTrip(hqqReq)
	if err != nil {
		t.Fatalf("HQQ RoundTrip: %v", err)
	}

	n, err = hqqResp.Body.Read(emptyBuf)
	if n != 0 || err != nil {
		t.Fatalf("zero-byte Read on HQQ body: got (%d, %v); want (0, nil)", n, err)
	}

	// Close HQQ body
	if err := hqqResp.Body.Close(); err != nil {
		t.Fatalf("HQQ Body.Close: %v", err)
	}

	// Read([]byte{}) on closed HQQ body must return error containing "closed"
	_, err = hqqResp.Body.Read(emptyBuf)
	if err == nil {
		t.Fatal("expected error on zero-byte read after HQQ close, got nil")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected error containing 'closed', got: %v", err)
	}

	// C. WebTransport Stream
	session, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}

	stream, err := session.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	// Zero-byte Read on open stream must NOT deadlock or block
	readDone := make(chan error, 1)
	go func() {
		n, err := stream.Read(emptyBuf)
		if n != 0 || err != nil {
			readDone <- fmt.Errorf("got (%d, %v); want (0, nil)", n, err)
			return
		}
		readDone <- nil
	}()

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("zero-byte stream Read: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("DEADLOCK: zero-byte stream Read blocked!")
	}

	// Zero-byte Write on open stream must NOT deadlock or block
	writeDone := make(chan error, 1)
	go func() {
		n, err := stream.Write(emptyBuf)
		if n != 0 || err != nil {
			writeDone <- fmt.Errorf("got (%d, %v); want (0, nil)", n, err)
			return
		}
		writeDone <- nil
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("zero-byte stream Write: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("DEADLOCK: zero-byte stream Write blocked!")
	}

	// Close stream
	if err := stream.Close(); err != nil {
		t.Fatalf("stream.Close: %v", err)
	}

	// Read on closed stream returns io.EOF
	n, err = stream.Read(emptyBuf)
	if err != io.EOF {
		t.Fatalf("read after stream close: got error %v; want io.EOF", err)
	}

	// Close session with error
	if err := session.CloseWithError(0x99, "session_test_end"); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}

	// Read on stream after session termination returns session code/reason
	_, err = stream.Read([]byte{1})
	if err == nil || !strings.Contains(err.Error(), "0x99") || !strings.Contains(err.Error(), "session_test_end") {
		t.Fatalf("expected session close error, got: %v", err)
	}
}

// =============================================================================
// 2. Adversarial Edge Cases: Immediate Close Without Reading
// =============================================================================

func TestAdversarial_ImmediateCloseWithoutReading(t *testing.T) {
	// A. Standard HTTP immediate close
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Write large response that client won't read
		_, _ = w.Write(bytes.Repeat([]byte("X"), 1024*1024))
	}))
	defer srv.Close()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	for i := 0; i < 20; i++ {
		resp, err := tr.RoundTrip(httptest.NewRequest("GET", srv.URL, nil))
		if err != nil {
			t.Fatalf("RoundTrip %d: %v", i, err)
		}
		// Immediate close without reading a single byte
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("resp.Body.Close %d: %v", i, err)
		}
	}

	// B. HQQ Engine immediate close without reading
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 20; i++ {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("hqq://chrome-net-engine/immediate-close-%d", i), nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext %d: %v", i, err)
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("HQQ RoundTrip %d: %v", i, err)
		}
		// Immediate close without reading: must release engine handles and return client to pool
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("HQQ Body.Close %d: %v", i, err)
		}
	}

	// C. WebTransport immediate session and stream close
	for i := 0; i < 20; i++ {
		sess, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
		if err != nil {
			t.Fatalf("DialWebTransport %d: %v", i, err)
		}
		st, err := sess.OpenStream(ctx)
		if err != nil {
			t.Fatalf("OpenStream %d: %v", i, err)
		}
		// Immediately close stream without I/O
		if err := st.Close(); err != nil {
			t.Fatalf("st.Close %d: %v", i, err)
		}
		// Immediately close session
		if err := sess.CloseWithError(0x00, "clean"); err != nil {
			t.Fatalf("sess.CloseWithError %d: %v", i, err)
		}
	}

	// D. WebSocket immediate close after dial
	wsURL, cleanup := startMockWSServer(t, nil)
	defer cleanup()

	for i := 0; i < 20; i++ {
		ws, resp, err := client.DialWebSocket(ctx, wsURL+fmt.Sprintf("/imm-%d", i), client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket %d: %v", i, err)
		}
		_ = resp.Body.Close()
		// Immediately close WebSocket without frames
		if err := ws.Close(); err != nil {
			t.Fatalf("ws.Close %d: %v", i, err)
		}
	}
}

// =============================================================================
// 3. Adversarial Edge Cases: Operations After Close
// =============================================================================

func TestAdversarial_OperationsAfterClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A. Transport.RoundTrip after Transport.Close()
	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	if err := tr.Close(); err != nil {
		t.Fatalf("tr.Close: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:12345", nil)
	_, err := tr.RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected error containing 'closed', got: %v", err)
	}

	// B. WebSocket operations after Close()
	wsURL, cleanup := startMockWSServer(t, nil)
	defer cleanup()

	ws, resp, err := client.DialWebSocket(ctx, wsURL+"/ops-after-close", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	_ = resp.Body.Close()

	if err := ws.Close(); err != nil {
		t.Fatalf("ws.Close: %v", err)
	}

	// Write after close
	err = ws.WriteMessage(client.TextMessage, []byte("payload"))
	if err == nil || !strings.Contains(err.Error(), "after close") {
		t.Fatalf("expected error containing 'after close', got: %v", err)
	}

	// Ping after close
	err = ws.Ping([]byte("ping"))
	if err == nil || !strings.Contains(err.Error(), "after close") {
		t.Fatalf("expected error containing 'after close', got: %v", err)
	}

	// Close idempotency
	if err := ws.Close(); err != nil {
		t.Fatalf("idempotent ws.Close failed: %v", err)
	}

	// Read after close returns io.EOF
	_, _, err = ws.ReadMessage()
	if err != io.EOF {
		t.Fatalf("expected io.EOF on ReadMessage after close, got: %v", err)
	}

	// C. WebTransport operations after CloseWithError()
	sess, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}
	st, err := sess.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	if err := sess.CloseWithError(0x42, "closed_for_test"); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}

	// SendDatagram after close
	err = sess.SendDatagram([]byte("data"))
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected error containing 'closed', got: %v", err)
	}

	// OpenStream after close
	_, err = sess.OpenStream(ctx)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected error containing 'closed', got: %v", err)
	}

	// Stream write after session close
	_, err = st.Write([]byte("data"))
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected error containing 'closed', got: %v", err)
	}

	// Stream write after stream close
	_ = st.Close()
	_, err = st.Write([]byte("data"))
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected error containing 'closed', got: %v", err)
	}

	// Idempotent stream close and session close
	if err := st.Close(); err != nil {
		t.Fatalf("idempotent st.Close failed: %v", err)
	}
	if err := sess.CloseWithError(0x42, "closed_for_test"); err != nil {
		t.Fatalf("idempotent sess.CloseWithError failed: %v", err)
	}
}

// =============================================================================
// 4. Adversarial Edge Cases: RFC 6455 Ping Boundary
// =============================================================================

func TestAdversarial_WebSocket_PingBoundaries(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, func(conn net.Conn, r *bufio.Reader) {
		for {
			op, payload, err := readClientFrame(r)
			if err != nil {
				return
			}
			if op == client.PingMessage {
				_ = writeServerFrame(conn, client.PongMessage, payload)
			}
		}
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := client.DialWebSocket(ctx, wsURL+"/ping-boundaries", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	_ = resp.Body.Close()
	defer ws.Close()

	// 1. 0-byte ping (valid)
	if err := ws.Ping([]byte{}); err != nil {
		t.Fatalf("Ping(0 bytes) failed: %v", err)
	}
	op, pongPayload, err := ws.ReadMessage()
	if err != nil || op != client.PongMessage || len(pongPayload) != 0 {
		t.Fatalf("expected 0-byte pong, got op=%d, payload=%d bytes, err=%v", op, len(pongPayload), err)
	}

	// 2. 1-byte ping (valid)
	if err := ws.Ping([]byte{0x7f}); err != nil {
		t.Fatalf("Ping(1 byte) failed: %v", err)
	}
	op, pongPayload, err = ws.ReadMessage()
	if err != nil || op != client.PongMessage || len(pongPayload) != 1 {
		t.Fatalf("expected 1-byte pong, got op=%d, payload=%d bytes, err=%v", op, len(pongPayload), err)
	}

	// 3. 125-byte ping (exact maximum RFC 6455 control frame boundary — valid)
	maxPing := bytes.Repeat([]byte("P"), 125)
	if err := ws.Ping(maxPing); err != nil {
		t.Fatalf("Ping(125 bytes) failed: %v", err)
	}
	op, pongPayload, err = ws.ReadMessage()
	if err != nil || op != client.PongMessage || len(pongPayload) != 125 {
		t.Fatalf("expected 125-byte pong, got op=%d, len=%d, err=%v", op, len(pongPayload), err)
	}

	// 4. 126-byte ping (1 byte above boundary — must fail)
	oversizedPing126 := bytes.Repeat([]byte("Q"), 126)
	err = ws.Ping(oversizedPing126)
	if err == nil {
		t.Fatal("expected 126-byte ping to fail, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds RFC 6455 maximum limit of 125 bytes") {
		t.Fatalf("expected RFC 6455 limit error, got: %v", err)
	}

	// 5. 1000-byte ping (well above boundary — must fail)
	oversizedPing1000 := bytes.Repeat([]byte("R"), 1000)
	err = ws.Ping(oversizedPing1000)
	if err == nil {
		t.Fatal("expected 1000-byte ping to fail, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds RFC 6455 maximum limit of 125 bytes") {
		t.Fatalf("expected RFC 6455 limit error, got: %v", err)
	}
}

// =============================================================================
// 5. Adversarial Edge Cases: WebTransport MTU Limit
// =============================================================================

func TestAdversarial_WebTransport_MTULimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}
	defer sess.CloseWithError(0, "done")

	// 1. 0-byte datagram -> succeeds
	if err := sess.SendDatagram([]byte{}); err != nil {
		t.Fatalf("SendDatagram(0 bytes) failed: %v", err)
	}
	d, err := sess.ReceiveDatagram(ctx)
	if err != nil || len(d) != 0 {
		t.Fatalf("ReceiveDatagram: got len=%d, err=%v; want 0 bytes", len(d), err)
	}

	// 2. 1472-byte datagram (exact GoldenChromeQUICTransportParams.MaxUDPPayloadSize) -> succeeds
	maxDatagram := bytes.Repeat([]byte("D"), 1472)
	if err := sess.SendDatagram(maxDatagram); err != nil {
		t.Fatalf("SendDatagram(1472 bytes) failed: %v", err)
	}
	d, err = sess.ReceiveDatagram(ctx)
	if err != nil || len(d) != 1472 {
		t.Fatalf("ReceiveDatagram: got len=%d, err=%v; want 1472 bytes", len(d), err)
	}

	// 3. 1473-byte datagram (1 byte above MTU limit) -> must return error containing "exceeds MTU limit"
	oversized1473 := bytes.Repeat([]byte("E"), 1473)
	err = sess.SendDatagram(oversized1473)
	if err == nil {
		t.Fatal("expected 1473-byte datagram to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds MTU limit") {
		t.Fatalf("expected error containing 'exceeds MTU limit', got: %v", err)
	}

	// 4. 2048-byte datagram -> must return error containing "exceeds MTU limit"
	oversized2048 := bytes.Repeat([]byte("F"), 2048)
	err = sess.SendDatagram(oversized2048)
	if err == nil {
		t.Fatal("expected 2048-byte datagram to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds MTU limit") {
		t.Fatalf("expected error containing 'exceeds MTU limit', got: %v", err)
	}
}

// =============================================================================
// 6. Adversarial Edge Cases: Context Cancellations & Deadlines
// =============================================================================

func TestAdversarial_ContextCancellationsAndDeadlines(t *testing.T) {
	// A. In-flight request aborted midway through body streaming
	blockStreamCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// First chunk
		_, _ = w.Write([]byte("chunk-1-data"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Block second chunk until client aborts
		<-blockStreamCh
		_, _ = w.Write([]byte("chunk-2-data"))
	}))
	defer func() {
		close(blockStreamCh)
		srv.Close()
	}()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	// Read first chunk
	buf := make([]byte, 12)
	n, err := resp.Body.Read(buf)
	if err != nil || n != 12 || string(buf[:n]) != "chunk-1-data" {
		t.Fatalf("read first chunk: n=%d, err=%v, data=%q", n, err, string(buf[:n]))
	}

	// Cancel context while waiting for second chunk
	cancel()

	// Read on body must immediately return context.Canceled error
	readErrCh := make(chan error, 1)
	go func() {
		_, rErr := resp.Body.Read(buf)
		readErrCh <- rErr
	}()

	select {
	case rErr := <-readErrCh:
		if rErr == nil {
			t.Fatal("expected error reading after context cancel, got nil")
		}
		if !errors.Is(rErr, context.Canceled) && !strings.Contains(rErr.Error(), "canceled") {
			t.Fatalf("expected context.Canceled, got: %v", rErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Body.Read did not return after context cancellation")
	}

	// B. Pre-expired deadline rejection on DialWebSocket
	expiredCtx, expiredCancel := context.WithDeadline(context.Background(), time.Now().Add(-100*time.Millisecond))
	defer expiredCancel()

	_, _, err = client.DialWebSocket(expiredCtx, "ws://127.0.0.1:443/test", client.Options{})
	if err == nil {
		t.Fatal("expected error dialing websocket with expired context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(expiredCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}

	// C. Pre-expired deadline rejection on DialWebTransport
	_, err = client.DialWebTransport(expiredCtx, "https://127.0.0.1:443/wt", client.Options{})
	if err == nil {
		t.Fatal("expected error dialing webtransport with expired context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(expiredCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
}

// =============================================================================
// 7. Connection Pool Lifecycle Under Contention
// =============================================================================

func TestAdversarial_ConnectionPoolContention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Small pool with capacity 2 to maximize contention
	pool := client.NewClientPool(2, client.DefaultOptions())
	defer pool.Close()

	tr := client.NewTransport(client.Options{
		InsecureSkipVerify: true,
		Pool:               pool,
	})
	defer tr.Close()

	const concurrentWorkers = 20
	var wg sync.WaitGroup
	wg.Add(concurrentWorkers)
	errCh := make(chan error, concurrentWorkers)

	for i := 0; i < concurrentWorkers; i++ {
		go func(workerID int) {
			defer wg.Done()
			reqURL := fmt.Sprintf("hqq://chrome-net-engine/contention-%d", workerID)
			req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
			if err != nil {
				errCh <- fmt.Errorf("worker %d: NewRequestWithContext: %w", workerID, err)
				return
			}

			resp, err := tr.RoundTrip(req)
			if err != nil {
				errCh <- fmt.Errorf("worker %d: RoundTrip: %w", workerID, err)
				return
			}

			// Read body in small pieces, introducing brief delay to force pool checkout contention
			var accumulated []byte
			buf := make([]byte, 4)
			for {
				n, rErr := resp.Body.Read(buf)
				if n > 0 {
					accumulated = append(accumulated, buf[:n]...)
				}
				if rErr == io.EOF {
					break
				}
				if rErr != nil {
					_ = resp.Body.Close()
					errCh <- fmt.Errorf("worker %d: Read: %w", workerID, rErr)
					return
				}
				// Short jitter
				time.Sleep(1 * time.Millisecond)
			}

			// Validate payload
			expected := fmt.Sprintf("OK: GET %s", reqURL)
			if string(accumulated) != expected {
				_ = resp.Body.Close()
				errCh <- fmt.Errorf("worker %d: body mismatch: got %q, want %q", workerID, string(accumulated), expected)
				return
			}

			// Close body, releasing client back to pool
			if err := resp.Body.Close(); err != nil {
				errCh <- fmt.Errorf("worker %d: Body.Close: %w", workerID, err)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Pool contention error: %v", err)
	}
}

// =============================================================================
// 8. Resource Leakage Verification
// =============================================================================

func countOpenFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

func TestAdversarial_ResourceLeakage(t *testing.T) {
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	initialGoroutines := runtime.NumGoroutine()
	initialFDs := countOpenFDs()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("resource-leak-test-ok"))
	}))
	defer srv.Close()

	tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const iterations = 50

	// 1. Repeated HTTP requests
	for i := 0; i < iterations; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("HTTP RoundTrip %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}

	// 2. Repeated HQQ requests
	for i := 0; i < iterations; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("hqq://chrome-net-engine/leak-%d", i), nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("HQQ RoundTrip %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}

	// 3. Repeated WebTransport sessions and streams
	for i := 0; i < iterations; i++ {
		sess, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
		if err != nil {
			t.Fatalf("DialWebTransport %d: %v", i, err)
		}
		st, err := sess.OpenStream(ctx)
		if err != nil {
			t.Fatalf("OpenStream %d: %v", i, err)
		}
		_ = st.Close()
		_ = sess.CloseWithError(0, "clean")
	}

	// Wait for connections and goroutines to settle
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	runtime.GC()

	finalGoroutines := runtime.NumGoroutine()
	finalFDs := countOpenFDs()

	// Ensure goroutines did not leak uncontrollably (allow small delta for test runner)
	goroutineDelta := finalGoroutines - initialGoroutines
	if goroutineDelta > 15 {
		t.Errorf("Potential goroutine leak: initial=%d, final=%d (delta=%d)", initialGoroutines, finalGoroutines, goroutineDelta)
	}

	// Ensure file descriptors did not leak uncontrollably
	if initialFDs > 0 && finalFDs > 0 {
		fdDelta := finalFDs - initialFDs
		if fdDelta > 20 {
			t.Errorf("Potential FD leak: initial=%d, final=%d (delta=%d)", initialFDs, finalFDs, fdDelta)
		}
	}
}

// =============================================================================
// 9. Adversarial Edge Cases: Unexpected EOF & Truncated Responses
// =============================================================================

func TestAdversarial_UnexpectedEOF(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		_, _ = http.ReadRequest(r)

		// Send partial body and close connection abruptly
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial"))
	}()

	tr := client.NewTransport(client.Options{})
	defer tr.Close()

	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://"+l.Addr().String(), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("expected error reading truncated body, got nil")
	}
}

func TestAdversarial_TruncatedWebSocketFrame(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}
		secKey := req.Header.Get("Sec-WebSocket-Key")
		acceptKey := computeAcceptKey(secKey)
		resp := fmt.Sprintf(
			"HTTP/1.1 101 Switching Protocols\r\n"+
				"Upgrade: websocket\r\n"+
				"Connection: Upgrade\r\n"+
				"Sec-WebSocket-Accept: %s\r\n\r\n",
			acceptKey,
		)
		_, _ = conn.Write([]byte(resp))

		// Send only 1 byte of frame header then close connection abruptly
		_, _ = conn.Write([]byte{0x81})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ws, resp, err := client.DialWebSocket(ctx, "ws://"+l.Addr().String()+"/truncated", client.Options{})
	if err != nil {
		return
	}
	_ = resp.Body.Close()
	defer ws.Close()

	_, _, err = ws.ReadMessage()
	if err == nil {
		t.Fatal("expected error reading truncated frame, got nil")
	}
}

// =============================================================================
// 10. Adversarial Edge Cases: Slow Peer Timeout Enforcement
// =============================================================================

func TestAdversarial_SlowPeerTimeout(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		_, _ = http.ReadRequest(r)

		// Send headers and stall before body chunks
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"))
		time.Sleep(200 * time.Millisecond)
		_, _ = conn.Write([]byte("4\r\ntest\r\n0\r\n\r\n"))
	}()

	tr := client.NewTransport(client.Options{})
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+l.Addr().String(), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	buf := make([]byte, 32)
	_, err = resp.Body.Read(buf)
	if err == nil {
		t.Fatal("expected timeout error reading slow peer, got nil")
	}
}

// =============================================================================
// 11. Adversarial Edge Cases: High Concurrency Stress
// =============================================================================

func TestAdversarial_HighConcurrencyStress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("concurrency_payload"))
	}))
	defer srv.Close()

	tr := client.NewTransport(client.Options{})
	defer tr.Close()

	var wg sync.WaitGroup
	const concurrentRequests = 50
	wg.Add(concurrentRequests)

	for i := 0; i < concurrentRequests; i++ {
		go func(id int) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
			if err != nil {
				t.Errorf("[%d] NewRequest error: %v", id, err)
				return
			}
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Errorf("[%d] RoundTrip error: %v", id, err)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "concurrency_payload" {
				t.Errorf("[%d] unexpected body: %s", id, body)
			}
		}(i)
	}
	wg.Wait()
}
