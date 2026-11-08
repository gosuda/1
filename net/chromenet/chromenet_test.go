package chromenet_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	chromenet "gosuda.org/chromenet"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

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
				h := sha1.New()
				h.Write([]byte(secKey + wsGUID))
				acceptKey := base64.StdEncoding.EncodeToString(h.Sum(nil))

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

func readWSClientFrame(r io.Reader) (opcode int, payload []byte, err error) {
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

	maskedPayload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, maskedPayload); err != nil {
		return 0, nil, err
	}

	payload = make([]byte, payloadLen)
	for i := 0; i < payloadLen; i++ {
		payload[i] = maskedPayload[i] ^ maskKey[i%4]
	}

	return opcode, payload, nil
}

func writeWSServerFrame(w io.Writer, opcode int, payload []byte) error {
	var header []byte
	header = append(header, byte(0x80|(opcode&0x0f)))

	payloadLen := len(payload)
	if payloadLen <= 125 {
		header = append(header, byte(payloadLen))
	} else if payloadLen <= 65535 {
		header = append(header, 126)
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(payloadLen))
		header = append(header, ext...)
	} else {
		header = append(header, 127)
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(payloadLen))
		header = append(header, ext...)
	}

	if _, err := w.Write(header); err != nil {
		return err
	}
	if payloadLen > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func TestNewTransport_EndToEnd(t *testing.T) {
	var mu sync.Mutex
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("X-Reply", "pong")
		_, _ = io.WriteString(w, "chromenet-response-body")
	}))
	defer srv.Close()

	const customUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) ChromeNet/1.0"
	tr := chromenet.NewTransport(chromenet.Options{UserAgent: customUA})
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := &http.Client{Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "chromenet-response-body" {
		t.Errorf("got %d %q; want 200 %q", resp.StatusCode, body, "chromenet-response-body")
	}
	if got := resp.Header.Get("X-Reply"); got != "pong" {
		t.Errorf("X-Reply = %q; want pong", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := seen.Get("User-Agent"); got != customUA {
		t.Errorf("server saw User-Agent %q; want %q", got, customUA)
	}
}

func TestDialWebTransport_FullLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Invalid scheme rejection
	_, err := chromenet.DialWebTransport(ctx, "ftp://127.0.0.1:443/wt", chromenet.Options{})
	if err == nil {
		t.Fatal("expected error on invalid scheme ftp://, got nil")
	}

	// Valid dial
	sess, err := chromenet.DialWebTransport(ctx, "https://127.0.0.1:443/wt", chromenet.Options{})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}

	// Open stream and exchange data
	st, err := sess.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer st.Close()

	payload := []byte("webtransport-chromenet-data")
	go func() {
		_, _ = st.Write(payload)
	}()

	recvBuf := make([]byte, len(payload))
	n, err := io.ReadFull(st, recvBuf)
	if err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if n != len(payload) || !bytes.Equal(recvBuf, payload) {
		t.Fatalf("stream data mismatch: got %q; want %q", recvBuf, payload)
	}

	// Datagram transmission
	dg := []byte("datagram-payload")
	if err := sess.SendDatagram(dg); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	recvdDg, err := sess.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("ReceiveDatagram: %v", err)
	}
	if !bytes.Equal(recvdDg, dg) {
		t.Fatalf("datagram mismatch: got %q, want %q", recvdDg, dg)
	}

	// MTU limit rejection (> 1472 bytes)
	giantDg := make([]byte, 1500)
	if err := sess.SendDatagram(giantDg); err == nil {
		t.Fatal("expected MTU limit error on 1500-byte datagram, got nil")
	}

	// AcceptStream on closed session
	if err := sess.CloseWithError(0x42, "graceful_abort"); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}

	_, err = sess.AcceptStream(ctx)
	if err == nil {
		t.Fatal("expected error on AcceptStream after CloseWithError, got nil")
	}
	if !strings.Contains(err.Error(), "session closed") {
		t.Fatalf("expected error containing 'session closed', got: %v", err)
	}
}

func TestDialWebSocket_FullLifecycle(t *testing.T) {
	url, cleanup := startMockWSServer(t, func(c net.Conn, r *bufio.Reader) {
		for {
			op, payload, err := readWSClientFrame(r)
			if err != nil {
				return
			}
			switch op {
			case 1: // Text
				_ = writeWSServerFrame(c, 1, append([]byte("echo: "), payload...))
			case 2: // Binary
				_ = writeWSServerFrame(c, 2, payload)
			case 9: // Ping
				_ = writeWSServerFrame(c, 10, payload) // Pong
			case 8: // Close
				_ = writeWSServerFrame(c, 8, payload)
				return
			}
		}
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := chromenet.DialWebSocket(ctx, url, chromenet.Options{})
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	if resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake response status = %v; want 101", resp)
	}

	// Text Message
	if err := ws.WriteMessage(chromenet.TextMessage, []byte("hello-ws")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	op, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != chromenet.TextMessage || string(data) != "echo: hello-ws" {
		t.Fatalf("unexpected message: op=%d, data=%q", op, string(data))
	}

	// Binary Message
	binPayload := []byte{0x00, 0xFF, 0x42, 0xAA}
	if err := ws.WriteMessage(chromenet.BinaryMessage, binPayload); err != nil {
		t.Fatalf("WriteMessage(binary): %v", err)
	}
	op, data, err = ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != chromenet.BinaryMessage || !bytes.Equal(data, binPayload) {
		t.Fatalf("unexpected binary message: op=%d, data=%v", op, data)
	}

	// Ping
	if err := ws.Ping([]byte("heartbeat")); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// Close
	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Write after close must error
	if err := ws.WriteMessage(chromenet.TextMessage, []byte("after-close")); err == nil {
		t.Fatal("expected error on WriteMessage after close, got nil")
	}
}
