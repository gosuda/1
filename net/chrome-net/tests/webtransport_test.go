package tests

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/chrome-net/client"
)

func TestWebTransport_DialSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}
	if session == nil {
		t.Fatal("expected non-nil WebTransportSession")
	}

	// Invalid scheme rejection
	_, err = client.DialWebTransport(ctx, "ftp://127.0.0.1:443/wt", client.Options{})
	if err == nil {
		t.Fatal("expected error on invalid scheme, got nil")
	}
}

func TestWebTransport_BidirectionalStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}

	stream, err := session.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close()

	msg := []byte("hello webtransport bidirectional stream")
	go func() {
		_, _ = stream.Write(msg)
	}()

	buf := make([]byte, len(msg))
	n, err := io.ReadFull(stream, buf)
	if err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("read %d bytes; want %d", n, len(msg))
	}
	if string(buf) != string(msg) {
		t.Fatalf("got %q; want %q", string(buf), string(msg))
	}
}

func TestWebTransport_DatagramTransmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}

	payload := []byte("datagram-packet-payload-001")
	if err := session.SendDatagram(payload); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}

	recvd, err := session.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("ReceiveDatagram: %v", err)
	}
	if string(recvd) != string(payload) {
		t.Fatalf("got datagram %q; want %q", string(recvd), string(payload))
	}
}

func TestWebTransport_DatagramMTULimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}

	// Valid 1000-byte datagram (within 1472 MTU)
	validPayload := make([]byte, 1000)
	if err := session.SendDatagram(validPayload); err != nil {
		t.Fatalf("SendDatagram(valid): %v", err)
	}

	// Oversized 1500-byte datagram (exceeds 1472 MTU)
	oversizedPayload := make([]byte, 1500)
	err = session.SendDatagram(oversizedPayload)
	if err == nil {
		t.Fatal("expected error for oversized datagram, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds MTU limit") {
		t.Fatalf("expected error containing 'exceeds MTU limit', got: %v", err)
	}
}

func TestWebTransport_CloseWithError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}

	stream, err := session.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	appCode := uint32(0x101)
	appReason := "session_quota_exceeded"

	if err := session.CloseWithError(appCode, appReason); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}

	// Stream Read must return error with code and reason
	buf := make([]byte, 16)
	_, err = stream.Read(buf)
	if err == nil {
		t.Fatal("expected error reading from closed session stream, got nil")
	}
	if !strings.Contains(err.Error(), "0x101") || !strings.Contains(err.Error(), appReason) {
		t.Fatalf("expected error containing '0x101' and %q, got: %v", appReason, err)
	}

	// Subsequent OpenStream must be rejected
	_, err = session.OpenStream(ctx)
	if err == nil {
		t.Fatal("expected error opening stream on closed session, got nil")
	}
	if !strings.Contains(err.Error(), "0x101") || !strings.Contains(err.Error(), appReason) {
		t.Fatalf("expected error containing '0x101' and %q, got: %v", appReason, err)
	}

	// Subsequent SendDatagram must be rejected
	err = session.SendDatagram([]byte("test"))
	if err == nil {
		t.Fatal("expected error sending datagram on closed session, got nil")
	}
}

func TestWebTransport_ConcurrentStreamLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}

	var openedStreams []io.ReadWriteCloser
	defer func() {
		for _, s := range openedStreams {
			_ = s.Close()
		}
	}()

	// Open 100 streams (max allowed)
	for i := 0; i < 100; i++ {
		st, err := session.OpenStream(ctx)
		if err != nil {
			t.Fatalf("OpenStream %d failed: %v", i, err)
		}
		openedStreams = append(openedStreams, st)
	}

	// 101st stream must be rejected
	_, err = session.OpenStream(ctx)
	if err == nil {
		t.Fatal("expected 101st stream to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "concurrent stream limit 100 reached") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Close one stream
	if err := openedStreams[0].Close(); err != nil {
		t.Fatalf("Close stream: %v", err)
	}
	openedStreams = openedStreams[1:]

	// Now opening a stream should succeed
	st, err := session.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream after close failed: %v", err)
	}
	openedStreams = append(openedStreams, st)
}

func TestWebTransport_ConcurrentDatagrams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}

	const concurrent = 30
	var wg sync.WaitGroup
	wg.Add(concurrent)
	errCh := make(chan error, concurrent)

	for i := 0; i < concurrent; i++ {
		go func(id int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf("datagram-worker-%04d", id))
			if err := session.SendDatagram(payload); err != nil {
				errCh <- err
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent datagram error: %v", err)
	}
}
