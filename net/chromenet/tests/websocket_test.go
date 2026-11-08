package tests

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/chromenet/client"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func computeAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func startMockWSServer(t *testing.T, handler func(conn net.Conn, r *bufio.Reader)) (string, func()) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				select {
				case <-closed:
					return
				default:
					return
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
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
				if _, err := c.Write([]byte(resp)); err != nil {
					return
				}

				if handler != nil {
					handler(c, r)
				}
			}(conn)
		}
	}()

	cleanup := func() {
		close(closed)
		_ = l.Close()
	}
	return "ws://" + l.Addr().String(), cleanup
}

// readClientFrame reads a masked frame from client per RFC 6455.
func readClientFrame(r io.Reader) (opcode int, payload []byte, err error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}

	opcode = int(header[0] & 0x0f)
	isMasked := (header[1] & 0x80) != 0
	rawLen := int(header[1] & 0x7f)

	if !isMasked {
		return 0, nil, fmt.Errorf("RFC 6455 violation: unmasked client frame")
	}

	var payloadLen int
	if rawLen == 126 {
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		payloadLen = int(binary.BigEndian.Uint16(ext))
	} else if rawLen == 127 {
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		payloadLen = int(binary.BigEndian.Uint64(ext))
	} else {
		payloadLen = rawLen
	}

	maskKey := make([]byte, 4)
	if _, err := io.ReadFull(r, maskKey); err != nil {
		return 0, nil, err
	}

	payload = make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}

	for i := 0; i < payloadLen; i++ {
		payload[i] ^= maskKey[i%4]
	}

	return opcode, payload, nil
}

// writeServerFrame sends an unmasked frame to client per RFC 6455 Section 5.1.
func writeServerFrame(w io.Writer, opcode int, payload []byte) error {
	var header []byte
	b0 := byte(0x80 | (opcode & 0x0f))
	payloadLen := len(payload)

	if payloadLen < 126 {
		header = append(header, b0, byte(payloadLen))
	} else if payloadLen <= 65535 {
		header = append(header, b0, 126, 0, 0)
		binary.BigEndian.PutUint16(header[2:], uint16(payloadLen))
	} else {
		header = append(header, b0, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:], uint64(payloadLen))
	}

	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func TestWebSocket_DialHandshake(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, nil)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := client.DialWebSocket(ctx, wsURL+"/ws", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d; want 101", resp.StatusCode)
	}
	if strings.ToLower(resp.Header.Get("Upgrade")) != "websocket" {
		t.Fatalf("Upgrade header = %q; want websocket", resp.Header.Get("Upgrade"))
	}
}

func TestWebSocket_TextFrame(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
		op, payload, err := readClientFrame(r)
		if err != nil {
			return
		}
		_ = writeServerFrame(c, op, payload)
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := client.DialWebSocket(ctx, wsURL+"/echo", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	msg := []byte("hello websocket text message")
	if err := ws.WriteMessage(client.TextMessage, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	op, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != client.TextMessage {
		t.Fatalf("opcode = %d; want %d", op, client.TextMessage)
	}
	if string(payload) != string(msg) {
		t.Fatalf("got %q; want %q", string(payload), string(msg))
	}
}

func TestWebSocket_BinaryFrame(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
		op, payload, err := readClientFrame(r)
		if err != nil {
			return
		}
		_ = writeServerFrame(c, op, payload)
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := client.DialWebSocket(ctx, wsURL+"/binary", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	data := []byte{0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe, 0xba, 0xbe}
	if err := ws.WriteMessage(client.BinaryMessage, data); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	op, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != client.BinaryMessage {
		t.Fatalf("opcode = %d; want %d", op, client.BinaryMessage)
	}
	if string(payload) != string(data) {
		t.Fatalf("got payload %x; want %x", payload, data)
	}
}

func TestWebSocket_PingPong(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
		op, payload, err := readClientFrame(r)
		if err != nil {
			return
		}
		if op == client.PingMessage {
			_ = writeServerFrame(c, client.PongMessage, payload)
		}
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := client.DialWebSocket(ctx, wsURL+"/ping", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	pingData := []byte("heartbeat-ping-payload")
	if err := ws.Ping(pingData); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	op, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != client.PongMessage {
		t.Fatalf("opcode = %d; want %d (Pong)", op, client.PongMessage)
	}
	if string(payload) != string(pingData) {
		t.Fatalf("got pong payload %q; want %q", string(payload), string(pingData))
	}
}

func TestWebSocket_PingPayloadLimit(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, nil)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := client.DialWebSocket(ctx, wsURL+"/limit", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	// 125-byte ping (valid)
	validPing := make([]byte, 125)
	if err := ws.Ping(validPing); err != nil {
		t.Fatalf("Ping(125): %v", err)
	}

	// 126-byte ping (must be rejected per RFC 6455 Section 5.5)
	oversizedPing := make([]byte, 126)
	err = ws.Ping(oversizedPing)
	if err == nil {
		t.Fatal("expected error for 126-byte ping, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds RFC 6455 maximum limit of 125 bytes") {
		t.Fatalf("expected RFC 6455 125-byte limit error, got: %v", err)
	}
}

func TestWebSocket_FrameSizeLimit(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, nil)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := client.DialWebSocket(ctx, wsURL+"/framesize", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	// 17 MiB payload exceeds 16 MiB default maxFrameSize
	oversized := make([]byte, 17*1024*1024)
	err = ws.WriteMessage(client.BinaryMessage, oversized)
	if err == nil {
		t.Fatal("expected error exceeding max frame size, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds max frame size") {
		t.Fatalf("expected error containing 'exceeds max frame size', got: %v", err)
	}
}

func TestWebSocket_HandshakeFailureOn4xx5xx(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				_, _ = http.ReadRequest(r)
				resp := "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n"
				_, _ = conn.Write([]byte(resp))
			}(c)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := client.DialWebSocket(ctx, "ws://"+l.Addr().String()+"/unauth", client.Options{})
	if err == nil {
		if ws != nil {
			_ = ws.Close()
		}
		t.Fatal("expected error on 401 handshake, got nil")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 response, got: %v", resp)
	}
}

func TestWebSocket_WriteAfterClose(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, nil)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := client.DialWebSocket(ctx, wsURL+"/closed", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}

	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = ws.WriteMessage(client.TextMessage, []byte("hello"))
	if err == nil {
		t.Fatal("expected error writing after close, got nil")
	}
	if !strings.Contains(err.Error(), "after close") {
		t.Fatalf("expected error containing 'after close', got: %v", err)
	}

	err = ws.Ping([]byte("hb"))
	if err == nil {
		t.Fatal("expected error pinging after close, got nil")
	}
	if !strings.Contains(err.Error(), "after close") {
		t.Fatalf("expected error containing 'after close', got: %v", err)
	}
}

func TestWebSocket_RFC8441Headers(t *testing.T) {
	if client.RFC8441MethodConnect != "CONNECT" {
		t.Errorf("RFC8441MethodConnect = %q; want CONNECT", client.RFC8441MethodConnect)
	}
	if client.RFC8441ProtocolWS != "websocket" {
		t.Errorf("RFC8441ProtocolWS = %q; want websocket", client.RFC8441ProtocolWS)
	}

	headers := client.NewRFC8441ConnectHeaders("example.com", "/ws", "https")
	if headers[":method"] != "CONNECT" {
		t.Errorf(":method = %q; want CONNECT", headers[":method"])
	}
	if headers[":protocol"] != "websocket" {
		t.Errorf(":protocol = %q; want websocket", headers[":protocol"])
	}
	if headers[":scheme"] != "https" {
		t.Errorf(":scheme = %q; want https", headers[":scheme"])
	}
	if headers[":path"] != "/ws" {
		t.Errorf(":path = %q; want /ws", headers[":path"])
	}
	if headers[":authority"] != "example.com" {
		t.Errorf(":authority = %q; want example.com", headers[":authority"])
	}
}

func TestWebSocket_ConcurrentWrites(t *testing.T) {
	var mu sync.Mutex
	receivedCount := 0

	wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
		for {
			_, _, err := readClientFrame(r)
			if err != nil {
				return
			}
			mu.Lock()
			receivedCount++
			mu.Unlock()
		}
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := client.DialWebSocket(ctx, wsURL+"/concurrent", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	const concurrent = 20
	var wg sync.WaitGroup
	wg.Add(concurrent)
	errCh := make(chan error, concurrent)

	for i := 0; i < concurrent; i++ {
		go func(id int) {
			defer wg.Done()
			msg := []byte(fmt.Sprintf("concurrent-msg-%04d", id))
			if err := ws.WriteMessage(client.TextMessage, msg); err != nil {
				errCh <- err
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent WriteMessage error: %v", err)
	}
}

func TestWebSocket_PingBoundaries(t *testing.T) {
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

	// 0, 1 and 125 bytes (the RFC 6455 control frame maximum) must round-trip.
	for _, n := range []int{0, 1, 125} {
		ping := bytes.Repeat([]byte{0x7f}, n)
		if err := ws.Ping(ping); err != nil {
			t.Fatalf("Ping(%d bytes): %v", n, err)
		}
		op, pong, err := ws.ReadMessage()
		if err != nil || op != client.PongMessage || !bytes.Equal(pong, ping) {
			t.Fatalf("Ping(%d bytes): got op=%d payload=%d bytes err=%v", n, op, len(pong), err)
		}
	}

	// Anything larger is rejected locally and must not desynchronize the stream.
	for _, n := range []int{126, 1000} {
		if err := ws.Ping(make([]byte, n)); err == nil {
			t.Fatalf("Ping(%d bytes) succeeded; want error", n)
		}
	}
	if err := ws.Ping([]byte("still-in-sync")); err != nil {
		t.Fatalf("Ping after rejected ping: %v", err)
	}
	op, pong, err := ws.ReadMessage()
	if err != nil || op != client.PongMessage || string(pong) != "still-in-sync" {
		t.Fatalf("pong after rejected ping: op=%d payload=%q err=%v", op, pong, err)
	}
}

func TestWebSocket_OperationsAfterClose(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, nil)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := client.DialWebSocket(ctx, wsURL+"/ops-after-close", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	_ = resp.Body.Close()

	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ws.Ping([]byte("ping")); err == nil {
		t.Fatal("Ping after Close succeeded; want error")
	}
	if err := ws.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := ws.ReadMessage(); err != io.EOF {
		t.Fatalf("ReadMessage after Close = %v; want io.EOF", err)
	}
}

func TestWebSocket_TruncatedFrame(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
		// One byte of a frame header, then the connection drops.
		_, _ = c.Write([]byte{0x81})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ws, resp, err := client.DialWebSocket(ctx, wsURL+"/truncated", client.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	_ = resp.Body.Close()
	defer ws.Close()

	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("ReadMessage on truncated frame succeeded; want error")
	}
}

func TestWebSocket_MalformedServerFrames(t *testing.T) {
	big := make([]byte, 2+2+126)
	big[0], big[1], big[3] = 0x89, 126, 126 // ping with a 126-byte payload

	tests := []struct {
		name  string
		frame []byte
		want  string
	}{
		{"64bit length with MSB set", []byte{0x81, 127, 0x80, 0, 0, 0, 0, 0, 0, 1, 'A'}, "most significant bit"},
		{"declared length over 16 MiB", []byte{0x82, 127, 0, 0, 0, 0, 0x01, 0, 0, 0x01}, "exceeds max frame size"},
		{"non-minimal 16-bit length", []byte{0x81, 126, 0, 5, 'h', 'e', 'l', 'l', 'o'}, "non-minimal"},
		{"non-minimal 64-bit length", append([]byte{0x82, 127, 0, 0, 0, 0, 0, 0, 1, 0}, make([]byte, 256)...), "non-minimal"},
		{"reserved data opcode", []byte{0x83, 0}, "reserved opcode"},
		{"reserved control opcode", []byte{0x8b, 0}, "reserved opcode"},
		{"fragmented control frame", []byte{0x09, 4, 'p', 'i', 'n', 'g'}, "fragmented control frame"},
		{"control frame over 125 bytes", big, "125"},
		{"RSV1 without extension", []byte{0xc1, 4, 't', 'e', 's', 't'}, "non-zero RSV"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
				_, _ = c.Write(tc.frame)
			})
			defer cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			ws, resp, err := client.DialWebSocket(ctx, wsURL+"/malformed", client.Options{})
			if err != nil {
				t.Fatalf("DialWebSocket: %v", err)
			}
			_ = resp.Body.Close()
			defer ws.Close()

			_, _, err = ws.ReadMessage()
			if err == nil {
				t.Fatal("ReadMessage succeeded; want protocol error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadMessage error = %v; want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestWebSocket_MaskedServerFrame(t *testing.T) {
	wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
		maskKey := []byte{0x12, 0x34, 0x56, 0x78}
		data := []byte("masked-server-payload")
		masked := make([]byte, len(data))
		for i := range data {
			masked[i] = data[i] ^ maskKey[i%4]
		}
		frame := append([]byte{0x81, byte(0x80 | len(data))}, maskKey...)
		_, _ = c.Write(append(frame, masked...))
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

	// RFC 6455 §5.1 lets the client either reject the frame or unmask it; it must not
	// deliver the payload still masked or desynchronize.
	op, payload, err := ws.ReadMessage()
	if err == nil && (op != client.TextMessage || string(payload) != "masked-server-payload") {
		t.Fatalf("masked frame delivered as op=%d payload=%q", op, payload)
	}
}

func TestWebSocket_LargeMessageBothDirections(t *testing.T) {
	const size = 1 << 20
	clientData := make([]byte, size)
	serverData := make([]byte, size)
	if _, err := rand.Read(clientData); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(serverData); err != nil {
		t.Fatal(err)
	}

	received := make(chan [32]byte, 1)
	wsURL, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
		op, payload, err := readClientFrame(r)
		if err != nil || op != client.BinaryMessage {
			received <- [32]byte{}
			return
		}
		received <- sha256.Sum256(payload)
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

	if err := ws.WriteMessage(client.BinaryMessage, clientData); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	op, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != client.BinaryMessage || sha256.Sum256(payload) != sha256.Sum256(serverData) {
		t.Fatalf("client received corrupted message: op=%d len=%d", op, len(payload))
	}
	select {
	case got := <-received:
		if got != sha256.Sum256(clientData) {
			t.Fatal("server received corrupted client message")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for server to receive message")
	}
}
