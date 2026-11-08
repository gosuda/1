package tests

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gosuda.org/chrome-net/client"
	"gosuda.org/hqq/bindgen/rt"
)

// =============================================================================
// Test Suite 1: Malicious & Malformed WebSocket Frames (RFC 6455 Violations)
// =============================================================================

func TestTier5_MaliciousWebSocketFrames_ProtocolViolations(t *testing.T) {
	// 1A. Frame with MSB set in 64-bit length (1 << 63 or negative cast)
	t.Run("64BitLength_MSBSet_Rejection", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			// FIN=1, opcode=1 (text), rawLen=127, MSB set in 64-bit length
			frame := []byte{
				0x81,
				127,
				0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
				'A',
			}
			_, _ = c.Write(frame)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/msb", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("SECURITY VULNERABILITY: ReadMessage panicked on MSB set: %v", r)
			}
		}()

		_, _, err = ws.ReadMessage()
		if err == nil {
			t.Fatal("expected error on 64-bit frame with MSB set, got nil")
		}
		t.Logf("64-bit MSB set rejected with expected error: %v", err)
	})

	// 1B. Declared frame size exceeding maxFrameSize (16 MiB)
	t.Run("DeclaredOversizedFrame_Exceeds16MiB", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			// Declared length = 16 MiB + 1 (16777217 = 0x01000001)
			frame := []byte{
				0x82, // FIN=1, binary
				127,
				0x00, 0x00, 0x00, 0x00,
				0x01, 0x00, 0x00, 0x01,
			}
			_, _ = c.Write(frame)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/oversized", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		_, _, err = ws.ReadMessage()
		if err == nil {
			t.Fatal("expected error on declared frame > 16 MiB, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds max frame size") {
			t.Fatalf("expected error containing 'exceeds max frame size', got: %v", err)
		}
	})

	// 1C. Non-minimal length encodings (RFC 6455 §5.2 violation)
	t.Run("NonMinimalLengthEncoding_126_Below126", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			// rawLen = 126 with 16-bit length = 5 (< 126)
			frame := []byte{
				0x81,
				126,
				0x00, 0x05,
				'h', 'e', 'l', 'l', 'o',
			}
			_, _ = c.Write(frame)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/nonminimal126", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		_, _, err = ws.ReadMessage()
		if err == nil {
			t.Fatal("expected error on non-minimal length encoding (rawLen 126 with length < 126), got nil")
		}
		if !strings.Contains(err.Error(), "non-minimal") {
			t.Fatalf("expected error mentioning non-minimal encoding, got: %v", err)
		}
	})

	t.Run("NonMinimalLengthEncoding_127_Below65536", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			// rawLen = 127 with 64-bit length = 256 (<= 65535)
			payload := make([]byte, 256)
			header := []byte{
				0x82,
				127,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00,
			}
			_, _ = c.Write(append(header, payload...))
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/nonminimal127", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		_, _, err = ws.ReadMessage()
		if err == nil {
			t.Fatal("expected error on non-minimal length encoding (rawLen 127 with length <= 65535), got nil")
		}
		if !strings.Contains(err.Error(), "non-minimal") {
			t.Fatalf("expected error mentioning non-minimal encoding, got: %v", err)
		}
	})

	// 1D. Reserved opcodes (0x3-0x7 non-control, 0xB-0xF control)
	t.Run("ReservedOpcode_NonControl_0x3", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			frame := []byte{0x83, 0x00} // FIN=1, opcode=0x3, len=0
			_, _ = c.Write(frame)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/opcode3", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		_, _, err = ws.ReadMessage()
		if err == nil {
			t.Fatal("expected error on reserved opcode 0x3, got nil")
		}
		if !strings.Contains(err.Error(), "reserved opcode") {
			t.Fatalf("expected error mentioning reserved opcode, got: %v", err)
		}
	})

	t.Run("ReservedOpcode_Control_0xB", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			frame := []byte{0x8b, 0x00} // FIN=1, opcode=0xB, len=0
			_, _ = c.Write(frame)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/opcodeB", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		_, _, err = ws.ReadMessage()
		if err == nil {
			t.Fatal("expected error on reserved opcode 0xB, got nil")
		}
		if !strings.Contains(err.Error(), "reserved opcode") {
			t.Fatalf("expected error mentioning reserved opcode, got: %v", err)
		}
	})

	// 1E. Masked server-to-client frames (RFC 6455 §5.1 requires unmasked frames from server)
	t.Run("MaskedServerFrame_SafeParsingOrRejection", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			maskKey := []byte{0x12, 0x34, 0x56, 0x78}
			data := []byte("masked-server-payload")
			masked := make([]byte, len(data))
			for i := range data {
				masked[i] = data[i] ^ maskKey[i%4]
			}
			header := []byte{0x81, byte(0x80 | len(data))}
			frame := append(header, maskKey...)
			frame = append(frame, masked...)
			_, _ = c.Write(frame)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/masked", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		// RFC 6455 §5.1 allows client to either cleanly reject or safely unmask
		op, payload, err := ws.ReadMessage()
		if err != nil {
			// Protocol rejection is compliant
			t.Logf("masked server frame cleanly rejected per RFC 6455 §5.1: %v", err)
		} else {
			// Safe unmasking without desync is also acceptable
			if op != client.TextMessage || string(payload) != "masked-server-payload" {
				t.Fatalf("unexpected unmasked result: op=%d payload=%q", op, string(payload))
			}
		}
	})

	// 1F. Fragmented control frames (FIN=0 for control frame violates RFC 6455 §5.5)
	t.Run("FragmentedControlFrame_FIN0_Rejection", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			// FIN=0, opcode=0x9 (Ping)
			frame := []byte{0x09, 0x04, 'p', 'i', 'n', 'g'}
			_, _ = c.Write(frame)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/fragctrl", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		_, _, err = ws.ReadMessage()
		if err == nil {
			t.Fatal("expected error on fragmented control frame (FIN=0), got nil")
		}
		if !strings.Contains(err.Error(), "fragmented control frame") {
			t.Fatalf("expected error mentioning fragmented control frame, got: %v", err)
		}
	})

	// 1G. Control frame with payload > 125 bytes (violates RFC 6455 §5.5)
	t.Run("ControlFrame_PayloadOver125Bytes_Rejection", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			// FIN=1, opcode=0x9 (Ping), len=126
			frame := make([]byte, 2+2+126)
			frame[0] = 0x89
			frame[1] = 126
			frame[2] = 0
			frame[3] = 126
			_, _ = c.Write(frame)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/ping126", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		_, _, err = ws.ReadMessage()
		if err == nil {
			t.Fatal("expected error on control frame payload > 125 bytes, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds RFC 6455 limit of 125 bytes") && !strings.Contains(err.Error(), "125") {
			t.Fatalf("expected error mentioning 125 bytes limit, got: %v", err)
		}
	})

	// 1H. Non-zero RSV bits (RSV1=1 without extension violates RFC 6455 §5.2)
	t.Run("NonZeroRSVBits_Rejection", func(t *testing.T) {
		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			// FIN=1, RSV1=1 (0x40), opcode=0x1 -> 0xC1
			frame := []byte{0xc1, 0x04, 't', 'e', 's', 't'}
			_, _ = c.Write(frame)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/rsv", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		_, _, err = ws.ReadMessage()
		if err == nil {
			t.Fatal("expected error on non-zero RSV bits, got nil")
		}
		if !strings.Contains(err.Error(), "non-zero RSV") {
			t.Fatalf("expected error mentioning non-zero RSV, got: %v", err)
		}
	})
}

// =============================================================================
// Test Suite 2: Malformed HTTP/2 Response Headers & Pseudo-Headers Boundary Fuzzing
// =============================================================================

func TestTier5_MalformedResponseHeaders_BoundaryFuzzing(t *testing.T) {
	testCases := []struct {
		name     string
		raw      string
		validate func(t *testing.T, hdr http.Header)
	}{
		{
			name: "EmptyInput",
			raw:  "",
			validate: func(t *testing.T, hdr http.Header) {
				if len(hdr) != 0 {
					t.Fatalf("expected 0 headers, got %d", len(hdr))
				}
			},
		},
		{
			name: "BareColonLines",
			raw:  ":\r\n::\r\n:::\r\n",
			validate: func(t *testing.T, hdr http.Header) {
				// Bare single colon should be ignored without panic
				// '::' produces key ':' with value ''
				// ':::' produces key ':' with value ':'
			},
		},
		{
			name: "PseudoHeaderWithMultipleColons",
			raw:  ":status:200\n:custom:extra:colon:value\n",
			validate: func(t *testing.T, hdr http.Header) {
				if hdr.Get(":status") != "200" {
					t.Fatalf(":status = %q; want 200", hdr.Get(":status"))
				}
				if hdr.Get(":custom") != "extra:colon:value" {
					t.Fatalf(":custom = %q; want extra:colon:value", hdr.Get(":custom"))
				}
			},
		},
		{
			name: "PseudoHeaderWithEmptyValue",
			raw:  ":path:\n:status:\n",
			validate: func(t *testing.T, hdr http.Header) {
				if _, ok := hdr[":path"]; !ok {
					t.Fatal("expected :path key present")
				}
				if hdr.Get(":path") != "" {
					t.Fatalf(":path = %q; want empty string", hdr.Get(":path"))
				}
			},
		},
		{
			name: "HeaderLineWithoutColon",
			raw:  "InvalidHeaderLineWithoutAnyColon\nContent-Type: text/plain\n",
			validate: func(t *testing.T, hdr http.Header) {
				if hdr.Get("Content-Type") != "text/plain" {
					t.Fatalf("Content-Type = %q; want text/plain", hdr.Get("Content-Type"))
				}
			},
		},
		{
			name: "LeadingTrailingWhitespaceAndCRLF",
			raw:  "   Server   :   ChromeEngine/131   \r\n   X-Custom  :  value  \r\n",
			validate: func(t *testing.T, hdr http.Header) {
				if hdr.Get("Server") != "ChromeEngine/131" {
					t.Fatalf("Server = %q; want ChromeEngine/131", hdr.Get("Server"))
				}
				if hdr.Get("X-Custom") != "value" {
					t.Fatalf("X-Custom = %q; want value", hdr.Get("X-Custom"))
				}
			},
		},
		{
			name: "OversizedHeaderLineApproaching64KiB",
			raw: func() string {
				var sb strings.Builder
				sb.WriteString("X-Giant-Header: ")
				sb.WriteString(strings.Repeat("A", 60000))
				sb.WriteString("\r\n")
				return sb.String()
			}(),
			validate: func(t *testing.T, hdr http.Header) {
				val := hdr.Get("X-Giant-Header")
				if len(val) != 60000 {
					t.Fatalf("giant header length = %d; want 60000", len(val))
				}
			},
		},
		{
			name: "HundredsOfHeaderLines",
			raw: func() string {
				var sb strings.Builder
				for i := 0; i < 500; i++ {
					sb.WriteString(fmt.Sprintf("X-Header-%03d: val-%03d\r\n", i, i))
				}
				return sb.String()
			}(),
			validate: func(t *testing.T, hdr http.Header) {
				for i := 0; i < 500; i++ {
					k := fmt.Sprintf("X-Header-%03d", i)
					want := fmt.Sprintf("val-%03d", i)
					if hdr.Get(k) != want {
						t.Fatalf("%s = %q; want %q", k, hdr.Get(k), want)
					}
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := client.ParseResponseHeaders([]byte(tc.raw))
			tc.validate(t, hdr)
		})
	}
}

// =============================================================================
// Test Suite 3: WebTransport Datagram Queue Saturation & MTU Enforcement
// =============================================================================

func TestTier5_WebTransport_DatagramBufferSaturation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt-saturation", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}
	defer sess.CloseWithError(0x00, "test_complete")

	// 3A. Sequential flooding: first 128 succeed, 129th fails with 'datagram buffer full'
	const bufCap = 128
	for i := 0; i < bufCap; i++ {
		payload := []byte(fmt.Sprintf("datagram-payload-%04d", i))
		if err := sess.SendDatagram(payload); err != nil {
			t.Fatalf("SendDatagram #%d failed unexpectedly: %v", i, err)
		}
	}

	// 129th must return 'datagram buffer full' immediately
	overflowErr := sess.SendDatagram([]byte("overflow-datagram"))
	if overflowErr == nil {
		t.Fatal("expected error on 129th datagram (buffer overflow), got nil")
	}
	if !strings.Contains(overflowErr.Error(), "datagram buffer full") {
		t.Fatalf("expected error containing 'datagram buffer full', got: %v", overflowErr)
	}

	// 3B. Non-blocking verification: SendDatagram must return immediately without blocking
	start := time.Now()
	_ = sess.SendDatagram([]byte("overflow-timing-check"))
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("SendDatagram blocked for %v on full buffer; must be non-blocking", elapsed)
	}

	// 3C. Drain and recover: receive 10 datagrams, verify FIFO ordering and payload integrity
	for i := 0; i < 10; i++ {
		d, err := sess.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatalf("ReceiveDatagram #%d failed: %v", i, err)
		}
		expected := fmt.Sprintf("datagram-payload-%04d", i)
		if string(d) != expected {
			t.Fatalf("ReceiveDatagram #%d got %q; want %q", i, string(d), expected)
		}
	}

	// 3D. Refill: exactly 10 new datagrams should now succeed
	for i := 0; i < 10; i++ {
		payload := []byte(fmt.Sprintf("datagram-refill-%04d", i))
		if err := sess.SendDatagram(payload); err != nil {
			t.Fatalf("SendDatagram refill #%d failed: %v", i, err)
		}
	}

	// 11th must fail again with buffer full
	if err := sess.SendDatagram([]byte("refill-overflow")); err == nil {
		t.Fatal("expected buffer full after refill, got nil")
	}

	// 3E. MTU limit enforcement (1472 bytes is max MTU)
	exactMTU := make([]byte, 1472)
	// Draining one slot to allow sending
	_, _ = sess.ReceiveDatagram(ctx)
	if err := sess.SendDatagram(exactMTU); err != nil {
		t.Fatalf("SendDatagram(1472) exact MTU failed: %v", err)
	}

	overMTU := make([]byte, 1473)
	if err := sess.SendDatagram(overMTU); err == nil {
		t.Fatal("expected error for 1473 bytes datagram, got nil")
	} else if !strings.Contains(err.Error(), "exceeds MTU limit") {
		t.Fatalf("expected MTU limit error, got: %v", err)
	}

	// 3F. Concurrent datagram saturation stress (20 senders racing against 5 receivers)
	var wg sync.WaitGroup
	var fullErrors int64
	var sendSuccesses int64
	var mu sync.Mutex

	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				data := []byte(fmt.Sprintf("concurrent-dgram-%02d-%02d", gid, i))
				err := sess.SendDatagram(data)
				mu.Lock()
				if err != nil && strings.Contains(err.Error(), "buffer full") {
					fullErrors++
				} else if err == nil {
					sendSuccesses++
				}
				mu.Unlock()
			}
		}(g)
	}

	// Receivers
	recvCtx, recvCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer recvCancel()
	for r := 0; r < 5; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, err := sess.ReceiveDatagram(recvCtx)
				if err != nil {
					return
				}
			}
		}()
	}

	wg.Wait()
	t.Logf("Concurrent Datagrams: %d successful sends, %d buffer full drops", sendSuccesses, fullErrors)
}

// =============================================================================
// Test Suite 4: High-Throughput / Boundary Payload Transfers (1 MiB WS, 1-byte vs 64 KiB HTTP)
// =============================================================================

func TestTier5_HighThroughput_LargeFramesAndBodies(t *testing.T) {
	// 4A. WebSocket: 1 MiB message transfer in both directions
	t.Run("WebSocket_1MiB_Bidirectional", func(t *testing.T) {
		const size = 1024 * 1024 // 1 MiB
		clientData := make([]byte, size)
		if _, err := rand.Read(clientData); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		clientHash := sha256.Sum256(clientData)

		serverData := make([]byte, size)
		if _, err := rand.Read(serverData); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		serverHash := sha256.Sum256(serverData)

		var serverRecvHash [32]byte
		var serverRecvDone = make(chan struct{})

		wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
			// Read client's 1 MiB binary frame
			op, payload, err := readClientFrame(r)
			if err != nil {
				t.Errorf("server readClientFrame: %v", err)
				close(serverRecvDone)
				return
			}
			if op != client.BinaryMessage {
				t.Errorf("server got opcode %d; want %d", op, client.BinaryMessage)
			}
			serverRecvHash = sha256.Sum256(payload)
			close(serverRecvDone)

			// Send server's 1 MiB binary frame
			_ = writeServerFrame(c, client.BinaryMessage, serverData)
		})
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		ws, resp, err := client.DialWebSocket(ctx, wsURL+"/large", client.Options{})
		if err != nil {
			t.Fatalf("DialWebSocket: %v", err)
		}
		_ = resp.Body.Close()
		defer ws.Close()

		// Write 1 MiB from client
		if err := ws.WriteMessage(client.BinaryMessage, clientData); err != nil {
			t.Fatalf("client WriteMessage(1 MiB): %v", err)
		}

		// Read 1 MiB on client from server
		op, payload, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("client ReadMessage(1 MiB): %v", err)
		}
		if op != client.BinaryMessage {
			t.Fatalf("client got opcode %d; want %d", op, client.BinaryMessage)
		}
		if len(payload) != size {
			t.Fatalf("client got payload len %d; want %d", len(payload), size)
		}
		clientRecvHash := sha256.Sum256(payload)
		if clientRecvHash != serverHash {
			t.Fatal("client received payload SHA-256 hash mismatch!")
		}

		select {
		case <-serverRecvDone:
			if serverRecvHash != clientHash {
				t.Fatal("server received client payload SHA-256 hash mismatch!")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for server receive")
		}
	})

	// 4B. HTTP Streaming Body Reads: 1-byte slow reads vs 64 KiB chunk reads
	t.Run("HTTP_Streaming_1Byte_vs_64KiB_Chunks", func(t *testing.T) {
		const payloadSize = 256 * 1024 // 256 KiB
		data := make([]byte, payloadSize)
		for i := 0; i < payloadSize; i++ {
			data[i] = byte(i % 251)
		}
		expectedHash := sha256.Sum256(data)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		}))
		defer srv.Close()

		tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
		defer tr.Close()

		// Test 1: Adversarial 1-byte read loop
		resp1, err := tr.RoundTrip(httptest.NewRequest("GET", srv.URL+"/1byte", nil))
		if err != nil {
			t.Fatalf("RoundTrip 1-byte: %v", err)
		}
		defer resp1.Body.Close()

		var readBuf1 bytes.Buffer
		oneByte := make([]byte, 1)
		for {
			n, err := resp1.Body.Read(oneByte)
			if n > 0 {
				readBuf1.Write(oneByte[:n])
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("1-byte read error: %v", err)
			}
		}
		if readBuf1.Len() != payloadSize {
			t.Fatalf("1-byte read total length = %d; want %d", readBuf1.Len(), payloadSize)
		}
		if sha256.Sum256(readBuf1.Bytes()) != expectedHash {
			t.Fatal("1-byte read SHA-256 hash mismatch!")
		}

		// Test 2: 64 KiB chunk read loop
		resp2, err := tr.RoundTrip(httptest.NewRequest("GET", srv.URL+"/64kib", nil))
		if err != nil {
			t.Fatalf("RoundTrip 64KiB: %v", err)
		}
		defer resp2.Body.Close()

		var readBuf2 bytes.Buffer
		chunkBuf := make([]byte, 65536)
		for {
			n, err := resp2.Body.Read(chunkBuf)
			if n > 0 {
				readBuf2.Write(chunkBuf[:n])
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("64KiB read error: %v", err)
			}
		}
		if readBuf2.Len() != payloadSize {
			t.Fatalf("64KiB read total length = %d; want %d", readBuf2.Len(), payloadSize)
		}
		if sha256.Sum256(readBuf2.Bytes()) != expectedHash {
			t.Fatal("64KiB read SHA-256 hash mismatch!")
		}
	})

	// 4C. HQQ Body Reader: 1-byte reads vs 64 KiB chunk reads over HQQ engine
	t.Run("HQQ_Streaming_1Byte_vs_64KiB_Chunks", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tr := client.NewTransport(client.Options{InsecureSkipVerify: true})
		defer tr.Close()

		// Test 1: 1-byte read on HQQ engine
		req1, err := http.NewRequestWithContext(ctx, "GET", "hqq://chrome-net-engine/stream-1byte", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp1, err := tr.RoundTrip(req1)
		if err != nil {
			t.Fatalf("HQQ RoundTrip 1-byte: %v", err)
		}
		defer resp1.Body.Close()

		var hqqBytes1 []byte
		oneByte := make([]byte, 1)
		for i := 0; i < 100; i++ {
			n, err := resp1.Body.Read(oneByte)
			if n > 0 {
				hqqBytes1 = append(hqqBytes1, oneByte[:n]...)
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("HQQ 1-byte read error: %v", err)
			}
		}

		// Test 2: 64 KiB read on HQQ engine
		req2, err := http.NewRequestWithContext(ctx, "GET", "hqq://chrome-net-engine/stream-64kib", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp2, err := tr.RoundTrip(req2)
		if err != nil {
			t.Fatalf("HQQ RoundTrip 64KiB: %v", err)
		}
		defer resp2.Body.Close()

		bigBuf := make([]byte, 65536)
		n, err := resp2.Body.Read(bigBuf)
		if err != nil && err != io.EOF {
			t.Fatalf("HQQ 64KiB read error: %v", err)
		}
		t.Logf("HQQ read returned %d bytes", n)
	})
}

// =============================================================================
// Test Suite 5: Broken IPC Pipe & Daemon Crash Recovery
// =============================================================================
// Verifies system behavior when the native worker daemon dies abruptly via SIGKILL:
// 1. In-flight body reads return immediate I/O or EOF/peer error without hanging or deadlocking.
// 2. Client handles severed IPC pipe without panic.
// 3. Stale dead connection is evicted from rt.Pool[*Client].
// 4. EnsureRunning detects dead server and respawns a healthy daemon for subsequent requests.
func TestTier5_BrokenIPCPipe_DaemonCrashAndRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Start dedicated server process
	srv, err := client.StartServer(ctx)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer srv.Close()

	pool := rt.NewPool(2, func(ctx context.Context) (*client.Client, error) {
		return client.Dial(ctx, srv.Listener(), client.DefaultOptions())
	}, func(c *client.Client) error {
		return c.Close()
	})
	defer pool.Close()

	tr := client.NewTransport(client.Options{Pool: pool})
	defer tr.Close()

	req, err := http.NewRequestWithContext(ctx, "GET", "hqq://chrome-net-engine/stream-kill-test", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	// Read initial chunk
	buf := make([]byte, 5)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("initial Read: %v", err)
	}
	if n != 5 {
		t.Fatalf("read %d bytes; want 5", n)
	}

	// 2. Send SIGKILL to daemon worker process while body is open
	daemonPID := srv.PID()
	if err := syscall.Kill(daemonPID, syscall.SIGKILL); err != nil {
		t.Fatalf("failed to send SIGKILL to daemon PID %d: %v", daemonPID, err)
	}

	// Wait for process termination
	select {
	case <-srv.Done():
		t.Logf("daemon PID %d confirmed terminated", daemonPID)
	case <-time.After(3 * time.Second):
		t.Fatalf("daemon PID %d did not terminate within 3 seconds", daemonPID)
	}

	// 3. Read subsequent chunk: MUST return error without deadlock or hang
	readCh := make(chan error, 1)
	go func() {
		b := make([]byte, 64)
		_, rErr := resp.Body.Read(b)
		readCh <- rErr
	}()

	select {
	case rErr := <-readCh:
		if rErr == nil {
			t.Fatal("expected error reading from body after daemon SIGKILL, got nil")
		}
		t.Logf("Read after daemon SIGKILL returned expected error: %v", rErr)
	case <-time.After(3 * time.Second):
		t.Fatal("DEADLOCK: Read blocked indefinitely after daemon SIGKILL!")
	}

	// Close body
	_ = resp.Body.Close()

	// 4. Verify dead connection eviction in rt.Pool
	deadCl, err := pool.Get(ctx)
	if err != nil {
		t.Fatalf("pool.Get: %v", err)
	}
	_, testErr := deadCl.EngineCreate(nil)
	if testErr == nil {
		t.Fatal("expected error on dead connection EngineCreate, got nil")
	}
	// Discard dead connection from pool
	pool.Discard(deadCl)

	// 5. Verify EnsureRunning self-healing respawn
	newListener, err := client.EnsureRunning(ctx)
	if err != nil {
		t.Fatalf("EnsureRunning failed to self-heal respawn: %v", err)
	}
	if newListener == "" {
		t.Fatal("EnsureRunning returned empty listener name")
	}

	freshCl, err := client.Open(ctx)
	if err != nil {
		t.Fatalf("Open after EnsureRunning respawn: %v", err)
	}
	defer freshCl.Close()

	freshEng, err := freshCl.EngineCreate(nil)
	if err != nil {
		t.Fatalf("EngineCreate on respawned daemon: %v", err)
	}
	if freshEng.IsNil() {
		t.Fatal("expected non-nil Engine handle from respawned daemon")
	}
	if err := freshCl.EngineDestroy(freshEng); err != nil {
		t.Fatalf("EngineDestroy on respawned daemon: %v", err)
	}

	_ = client.Shutdown()
}

// =============================================================================
// Test Suite 6: Engine Handle Invalidation & Cascade Destruction
// =============================================================================
// Verifies memory safety and clean error handling when operations target invalid or destroyed handles:
// 1. Double destruction of Engine handle returns clean error without crashing daemon.
// 2. Stale handle calls (HttpRequest on destroyed Engine) return clean bad handle error.
// 3. Cascade destruction: Destroying parent Engine automatically invalidates child handles
//    (Request, WtSession, WsConn), and subsequent operations on them return clean errors.
func TestTier5_EngineHandleInvalidation_DoubleDestroyAndCascade(t *testing.T) {
	client.ResetSharedServerForTesting()
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cl, err := client.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cl.Close()

	// 1. Create Engine
	eng, err := cl.EngineCreate(nil)
	if err != nil {
		t.Fatalf("EngineCreate: %v", err)
	}
	if eng.IsNil() {
		t.Fatal("EngineCreate returned nil handle")
	}

	// First destroy: must succeed
	if err := cl.EngineDestroy(eng); err != nil {
		t.Fatalf("first EngineDestroy: %v", err)
	}

	// 2. Double destroy: must return non-nil error containing bad handle without daemon crash
	err = cl.EngineDestroy(eng)
	if err == nil {
		t.Fatal("expected error on double EngineDestroy, got nil")
	}
	if !strings.Contains(err.Error(), "bad handle") {
		t.Fatalf("expected error containing 'bad handle', got: %v", err)
	}

	// 3. Stale handle call: HttpRequest on destroyed Engine
	headerBuf := make([]byte, 4096)
	_, _, _, err = cl.HttpRequest(eng, []byte("GET /test"), headerBuf)
	if err == nil {
		t.Fatal("expected error on HttpRequest with destroyed engine, got nil")
	}
	if !strings.Contains(err.Error(), "bad handle") {
		t.Fatalf("expected error containing 'bad handle', got: %v", err)
	}

	// 4. Cascade destruction: parent Engine owns child handles
	eng2, err := cl.EngineCreate(nil)
	if err != nil {
		t.Fatalf("EngineCreate eng2: %v", err)
	}

	rHandle, _, _, err := cl.HttpRequest(eng2, []byte("GET /cascade"), headerBuf)
	if err != nil {
		t.Fatalf("HttpRequest on eng2: %v", err)
	}

	wtSession, err := cl.WtDial(eng2, "https://127.0.0.1:443/wt")
	if err != nil {
		t.Fatalf("WtDial on eng2: %v", err)
	}

	wsConn, err := cl.WsDial(eng2, "ws://127.0.0.1:8080/ws")
	if err != nil {
		t.Fatalf("WsDial on eng2: %v", err)
	}

	// Destroy parent engine eng2
	if err := cl.EngineDestroy(eng2); err != nil {
		t.Fatalf("EngineDestroy eng2: %v", err)
	}

	// Verify child handles are invalidated by cascade destruction
	buf := make([]byte, 256)
	_, _, err = cl.HttpReadBody(rHandle, 256, buf)
	if err == nil {
		t.Fatal("expected error on HttpReadBody after parent engine destroyed, got nil")
	}
	if !strings.Contains(err.Error(), "bad handle") {
		t.Fatalf("expected error containing 'bad handle', got: %v", err)
	}

	err = cl.RequestDestroy(rHandle)
	if err == nil {
		t.Fatal("expected error on RequestDestroy after parent engine destroyed, got nil")
	}
	if !strings.Contains(err.Error(), "bad handle") {
		t.Fatalf("expected error containing 'bad handle', got: %v", err)
	}

	err = cl.WtSessionDestroy(wtSession)
	if err == nil {
		t.Fatal("expected error on WtSessionDestroy after parent engine destroyed, got nil")
	}
	if !strings.Contains(err.Error(), "bad handle") {
		t.Fatalf("expected error containing 'bad handle', got: %v", err)
	}

	err = cl.WsConnDestroy(wsConn)
	if err == nil {
		t.Fatal("expected error on WsConnDestroy after parent engine destroyed, got nil")
	}
	if !strings.Contains(err.Error(), "bad handle") {
		t.Fatalf("expected error containing 'bad handle', got: %v", err)
	}
}

// =============================================================================
// Test Suite 7: WebTransport AcceptStream & Concurrent Aborts
// =============================================================================
// Covers AcceptStream edge cases and verifies concurrency safety:
// 1. AcceptStream with pre-cancelled or timed out context returns context error immediately.
// 2. AcceptStream on closed session returns session closed error with code & reason.
// 3. 50 concurrent goroutines racing across OpenStream, Read, Write, and CloseWithError.
//    Verifies zero deadlocks and zero race detector warnings.
func TestTier5_WebTransport_AcceptStream_And_ConcurrentAborts(t *testing.T) {
	ctx := context.Background()

	// 1. AcceptStream with pre-cancelled context
	sess, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}

	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = sess.AcceptStream(cancelledCtx)
	if err == nil {
		t.Fatal("expected error on AcceptStream with cancelled context, got nil")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	// 2. AcceptStream on closed session
	if err := sess.CloseWithError(0x101, "adversarial_close"); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}
	_, err = sess.AcceptStream(ctx)
	if err == nil {
		t.Fatal("expected error on AcceptStream after CloseWithError, got nil")
	}
	if !strings.Contains(err.Error(), "session closed") || !strings.Contains(err.Error(), "0x101") {
		t.Fatalf("expected session closed error with code 0x101, got: %v", err)
	}

	// 3. 50 concurrent goroutines racing between OpenStream, Read, Write, and CloseWithError
	sess2, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport sess2: %v", err)
	}

	const concurrency = 50
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(id int) {
			defer wg.Done()
			stream, err := sess2.OpenStream(ctx)
			if err != nil {
				return
			}
			defer stream.Close()

			var innerWg sync.WaitGroup
			innerWg.Add(2)

			// Reader goroutine
			go func() {
				defer innerWg.Done()
				buf := make([]byte, 64)
				_, _ = stream.Read(buf)
			}()

			// Writer goroutine
			go func() {
				defer innerWg.Done()
				_, _ = stream.Write([]byte("concurrent-stream-chunk"))
			}()

			innerWg.Wait()
		}(i)
	}

	// Sleep briefly then trigger abort during active stream I/O
	time.Sleep(3 * time.Millisecond)
	_ = sess2.CloseWithError(0x999, "rapid_concurrent_abort")

	// Wait for all 50 goroutines to complete
	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
		t.Log("all 50 concurrent streams terminated cleanly without hang or deadlock")
	case <-time.After(3 * time.Second):
		t.Fatal("DEADLOCK: concurrent stream aborts blocked!")
	}
}

// =============================================================================
// Test Suite 8: Direct Generated IDL Client Coverage
// =============================================================================
// Tests direct IDL methods and futures generated by hqq-bindgen:
// WtDial, WtDialAsync, WtSessionDestroy, WtSessionDestroyAsync,
// WsDial, WsDialAsync, WsConnDestroy, WsConnDestroyAsync,
// Client.Conn, Client.Kill.
func TestTier5_GeneratedClient_IDLDirectCoverage(t *testing.T) {
	client.ResetSharedServerForTesting()
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cl, err := client.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cl.Close()

	// 1. Client.Conn
	if conn := cl.Conn(); conn == nil {
		t.Fatal("Client.Conn() returned nil")
	}

	// 2. Engine Create
	eng, err := cl.EngineCreate(nil)
	if err != nil {
		t.Fatalf("EngineCreate: %v", err)
	}

	// 3. WtDial & WtSessionDestroy (synchronous)
	wt1, err := cl.WtDial(eng, "https://127.0.0.1:443/wt-sync")
	if err != nil {
		t.Fatalf("WtDial: %v", err)
	}
	if wt1.IsNil() {
		t.Fatal("WtDial returned nil handle")
	}
	if err := cl.WtSessionDestroy(wt1); err != nil {
		t.Fatalf("WtSessionDestroy: %v", err)
	}

	// 4. WtDialAsync & WtSessionDestroyAsync (asynchronous futures)
	wtFut, err := cl.WtDialAsync(eng, "https://127.0.0.1:443/wt-async")
	if err != nil {
		t.Fatalf("WtDialAsync: %v", err)
	}
	wt2, err := wtFut.Wait()
	if err != nil {
		t.Fatalf("wtFut.Wait(): %v", err)
	}
	if wt2.IsNil() {
		t.Fatal("wtFut.Wait() returned nil handle")
	}
	wtDesFut, err := cl.WtSessionDestroyAsync(wt2)
	if err != nil {
		t.Fatalf("WtSessionDestroyAsync: %v", err)
	}
	if err := wtDesFut.Wait(); err != nil {
		t.Fatalf("wtDesFut.Wait(): %v", err)
	}

	// 5. WsDial & WsConnDestroy (synchronous)
	ws1, err := cl.WsDial(eng, "ws://127.0.0.1:8080/ws-sync")
	if err != nil {
		t.Fatalf("WsDial: %v", err)
	}
	if ws1.IsNil() {
		t.Fatal("WsDial returned nil handle")
	}
	if err := cl.WsConnDestroy(ws1); err != nil {
		t.Fatalf("WsConnDestroy: %v", err)
	}

	// 6. WsDialAsync & WsConnDestroyAsync (asynchronous futures)
	wsFut, err := cl.WsDialAsync(eng, "ws://127.0.0.1:8080/ws-async")
	if err != nil {
		t.Fatalf("WsDialAsync: %v", err)
	}
	ws2, err := wsFut.Wait()
	if err != nil {
		t.Fatalf("wsFut.Wait(): %v", err)
	}
	if ws2.IsNil() {
		t.Fatal("wsFut.Wait() returned nil handle")
	}
	wsDesFut, err := cl.WsConnDestroyAsync(ws2)
	if err != nil {
		t.Fatalf("WsConnDestroyAsync: %v", err)
	}
	if err := wsDesFut.Wait(); err != nil {
		t.Fatalf("wsDesFut.Wait(): %v", err)
	}

	// Destroy engine
	if err := cl.EngineDestroy(eng); err != nil {
		t.Fatalf("EngineDestroy: %v", err)
	}

	// 7. Client.Kill
	cl2, err := client.Open(ctx)
	if err != nil {
		t.Fatalf("Open cl2: %v", err)
	}
	cl2.Kill()
}

// =============================================================================
// Test Suite 9: Zero Resource Leak Stress Loop (500 Iterations)
// =============================================================================
// Runs a 500-iteration stress loop mixing aborted requests, WebTransport streams,
// datagram transmissions, and HQQ calls. Empirically verifies zero goroutine leaks
// and zero open file descriptor leaks (/proc/self/fd).
func TestTier5_ZeroLeak_AdversarialStressLoop(t *testing.T) {
	client.ResetSharedServerForTesting()
	defer client.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 1. Warmup mock server
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("zero-leak-payload"))
	}))
	defer mockSrv.Close()

	// 2. Warmup HQQ background server & client pool
	_, err := client.EnsureRunning(ctx)
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	warmTr := client.NewTransport(client.Options{})
	warmReq, _ := http.NewRequestWithContext(ctx, "GET", mockSrv.URL, nil)
	warmResp, err := warmTr.RoundTrip(warmReq)
	if err == nil {
		_, _ = io.ReadAll(warmResp.Body)
		_ = warmResp.Body.Close()
	}

	hWarmReq, _ := http.NewRequestWithContext(ctx, "GET", "hqq://chrome-net-engine/warmup", nil)
	hWarmResp, err := warmTr.RoundTrip(hWarmReq)
	if err == nil {
		b := make([]byte, 16)
		_, _ = hWarmResp.Body.Read(b)
		_ = hWarmResp.Body.Close()
	}
	_ = warmTr.Close()

	// Stabilize runtime
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
	}

	// Snapshot baseline
	baselineGoroutines := runtime.NumGoroutine()
	baselineFDs := countOpenFDs()
	t.Logf("Baseline (post-warmup): Goroutines=%d, Open FDs=%d", baselineGoroutines, baselineFDs)

	const iterations = 500
	start := time.Now()

	for i := 0; i < iterations; i++ {
		// A. WebTransport session lifecycle
		wtSess, err := client.DialWebTransport(ctx, "https://127.0.0.1:443/wt", client.Options{})
		if err == nil {
			st, err := wtSess.OpenStream(ctx)
			if err == nil {
				_ = st.Close()
			}
			_ = wtSess.SendDatagram([]byte("ping"))
			_, _ = wtSess.ReceiveDatagram(ctx)
			_ = wtSess.CloseWithError(0x200, "adversarial_loop_done")
		}

		// B. HTTP Transport with cancelled context
		cCtx, cancelFn := context.WithCancel(ctx)
		cancelFn()
		trCancel := client.NewTransport(client.Options{})
		cReq, _ := http.NewRequestWithContext(cCtx, "GET", mockSrv.URL, nil)
		_, _ = trCancel.RoundTrip(cReq)
		_ = trCancel.Close()

		// C. HTTP Transport standard request
		trStd := client.NewTransport(client.Options{})
		sReq, _ := http.NewRequestWithContext(ctx, "GET", mockSrv.URL, nil)
		sResp, err := trStd.RoundTrip(sReq)
		if err == nil {
			_, _ = io.ReadAll(sResp.Body)
			_ = sResp.Body.Close()
		}
		_ = trStd.Close()

		// D. HQQ streaming request
		trHqq := client.NewTransport(client.Options{})
		hReq, _ := http.NewRequestWithContext(ctx, "GET", "hqq://chrome-net-engine/ping", nil)
		hResp, err := trHqq.RoundTrip(hReq)
		if err == nil {
			b := make([]byte, 16)
			_, _ = hResp.Body.Read(b)
			_ = hResp.Body.Close()
		}
		_ = trHqq.Close()
	}

	elapsed := time.Since(start)
	t.Logf("Executed %d adversarial iterations in %v", iterations, elapsed)

	// Stabilize and settle
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
	}

	finalGoroutines := runtime.NumGoroutine()
	finalFDs := countOpenFDs()

	deltaGoroutines := finalGoroutines - baselineGoroutines
	deltaFDs := finalFDs - baselineFDs

	t.Logf("Final measurement: Goroutines=%d (delta=%d), Open FDs=%d (delta=%d)",
		finalGoroutines, deltaGoroutines, finalFDs, deltaFDs)

	if deltaGoroutines > 0 {
		t.Fatalf("GOROUTINE LEAK DETECTED: leaked %d goroutines across %d iterations", deltaGoroutines, iterations)
	}
	if deltaFDs > 0 {
		t.Fatalf("FILE DESCRIPTOR LEAK DETECTED: leaked %d FDs across %d iterations", deltaFDs, iterations)
	}
}
