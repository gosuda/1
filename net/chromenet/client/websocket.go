package client

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RFC 6455 message types / opcodes.
const (
	TextMessage   = 1
	BinaryMessage = 2
	CloseMessage  = 8
	PingMessage   = 9
	PongMessage   = 10
)

// RFC 8441 HTTP/2 Extended CONNECT constants.
const (
	RFC8441MethodConnect = "CONNECT"
	RFC8441ProtocolWS    = "websocket"
)

// NewRFC8441ConnectHeaders returns the standard pseudo-headers for RFC 8441 Extended CONNECT.
func NewRFC8441ConnectHeaders(authority, path, scheme string) map[string]string {
	if scheme == "" {
		scheme = "https"
	}
	return map[string]string{
		":method":    RFC8441MethodConnect,
		":protocol":  RFC8441ProtocolWS,
		":scheme":    scheme,
		":path":      path,
		":authority": authority,
	}
}

// WebSocketConn defines the WebSocket client interface.
type WebSocketConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Ping(data []byte) error
	Close() error
}

// DialWebSocket establishes an RFC 6455 WebSocket connection.
func DialWebSocket(ctx context.Context, rawURL string, opts Options) (WebSocketConn, *http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("websocket: invalid url %q: %w", rawURL, err)
	}

	isTLS := false
	switch u.Scheme {
	case "ws", "http":
		isTLS = false
	case "wss", "https":
		isTLS = true
	default:
		return nil, nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		if isTLS {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	if isTLS {
		serverName := u.Hostname()
		tlsConfig := &tls.Config{
			InsecureSkipVerify: opts.InsecureSkipVerify,
			ServerName:         serverName,
		}
		conn, err = tls.DialWithDialer(dialer, "tcp", host, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, nil, err
	}

	// Generate 16-byte random nonce for Sec-WebSocket-Key
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("websocket: nonce generation failed: %w", err)
	}
	secKey := base64.StdEncoding.EncodeToString(nonce)

	reqURI := u.RequestURI()
	if reqURI == "" {
		reqURI = "/"
	}

	headers := make(http.Header)
	ApplyChromeHeaders(headers, opts.UserAgent)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("GET %s HTTP/1.1\r\n", reqURI))
	sb.WriteString(fmt.Sprintf("Host: %s\r\n", u.Host))
	sb.WriteString("Upgrade: websocket\r\n")
	sb.WriteString("Connection: Upgrade\r\n")
	sb.WriteString(fmt.Sprintf("Sec-WebSocket-Key: %s\r\n", secKey))
	sb.WriteString("Sec-WebSocket-Version: 13\r\n")

	for k, vv := range headers {
		if strings.EqualFold(k, "Upgrade") || strings.EqualFold(k, "Connection") ||
			strings.EqualFold(k, "Sec-WebSocket-Key") || strings.EqualFold(k, "Sec-WebSocket-Version") ||
			strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vv {
			sb.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
		}
	}
	sb.WriteString("\r\n")

	bufrw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	if _, err := bufrw.WriteString(sb.String()); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := bufrw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}

	req := &http.Request{Method: "GET", URL: u}
	resp, err := http.ReadResponse(bufrw.Reader, req)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, resp, fmt.Errorf("websocket: bad handshake status code: %d", resp.StatusCode)
	}

	ws := &wsConn{
		conn:         conn,
		bufrw:        bufrw,
		maxFrameSize: 16 * 1024 * 1024,
	}
	return ws, resp, nil
}

type wsConn struct {
	conn         net.Conn
	bufrw        *bufio.ReadWriter
	writeMu      sync.Mutex
	readMu       sync.Mutex
	closed       atomic.Bool
	maxFrameSize int
}

func (c *wsConn) WriteMessage(messageType int, data []byte) error {
	if c.closed.Load() {
		return fmt.Errorf("websocket: write after close")
	}
	if len(data) > c.maxFrameSize {
		return fmt.Errorf("websocket: payload size %d exceeds max frame size %d", len(data), c.maxFrameSize)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.closed.Load() {
		return fmt.Errorf("websocket: write after close")
	}

	return c.writeFrameLocked(messageType, data)
}

func (c *wsConn) Ping(data []byte) error {
	if c.closed.Load() {
		return fmt.Errorf("websocket: ping after close")
	}
	if len(data) > 125 {
		return fmt.Errorf("websocket: control frame payload %d exceeds RFC 6455 maximum limit of 125 bytes", len(data))
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.closed.Load() {
		return fmt.Errorf("websocket: ping after close")
	}

	return c.writeFrameLocked(PingMessage, data)
}

func (c *wsConn) writeFrameLocked(messageType int, data []byte) error {
	var maskKey [4]byte
	if _, err := rand.Read(maskKey[:]); err != nil {
		return err
	}

	var header []byte
	b0 := byte(0x80 | (messageType & 0x0f)) // FIN = 1
	payloadLen := len(data)

	if payloadLen < 126 {
		header = append(header, b0, byte(0x80|payloadLen))
	} else if payloadLen <= 65535 {
		header = append(header, b0, byte(0x80|126), 0, 0)
		binary.BigEndian.PutUint16(header[2:], uint16(payloadLen))
	} else {
		header = append(header, b0, byte(0x80|127), 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:], uint64(payloadLen))
	}

	header = append(header, maskKey[:]...)

	maskedData := make([]byte, payloadLen)
	for i := 0; i < payloadLen; i++ {
		maskedData[i] = data[i] ^ maskKey[i%4]
	}

	if _, err := c.bufrw.Write(header); err != nil {
		return err
	}
	if len(maskedData) > 0 {
		if _, err := c.bufrw.Write(maskedData); err != nil {
			return err
		}
	}
	return c.bufrw.Flush()
}

func (c *wsConn) ReadMessage() (int, []byte, error) {
	if c.closed.Load() {
		return 0, nil, io.EOF
	}

	c.readMu.Lock()
	defer c.readMu.Unlock()

	if c.closed.Load() {
		return 0, nil, io.EOF
	}

	header := make([]byte, 2)
	if _, err := io.ReadFull(c.bufrw.Reader, header); err != nil {
		return 0, nil, err
	}

	fin := (header[0] & 0x80) != 0
	rsv := (header[0] & 0x70) >> 4
	if rsv != 0 {
		return 0, nil, fmt.Errorf("websocket: non-zero RSV bits: 0x%x", rsv)
	}

	opcode := int(header[0] & 0x0f)
	if (opcode >= 0x3 && opcode <= 0x7) || (opcode >= 0xB && opcode <= 0xF) {
		return 0, nil, fmt.Errorf("websocket: reserved opcode 0x%x", opcode)
	}

	if (opcode >= 0x8) && !fin {
		return 0, nil, fmt.Errorf("websocket: fragmented control frame (FIN=0)")
	}

	isMasked := (header[1] & 0x80) != 0
	rawLen := int(header[1] & 0x7f)

	var payloadLen int
	if rawLen == 126 {
		ext := make([]byte, 2)
		if _, err := io.ReadFull(c.bufrw.Reader, ext); err != nil {
			return 0, nil, err
		}
		payloadLen = int(binary.BigEndian.Uint16(ext))
		if payloadLen < 126 {
			return 0, nil, fmt.Errorf("websocket: non-minimal 16-bit payload length encoding: %d < 126", payloadLen)
		}
	} else if rawLen == 127 {
		ext := make([]byte, 8)
		if _, err := io.ReadFull(c.bufrw.Reader, ext); err != nil {
			return 0, nil, err
		}
		uLen := binary.BigEndian.Uint64(ext)
		if (uLen & (1 << 63)) != 0 {
			return 0, nil, fmt.Errorf("websocket: 64-bit frame length has most significant bit set: 0x%x", uLen)
		}
		if uLen <= 65535 {
			return 0, nil, fmt.Errorf("websocket: non-minimal 64-bit payload length encoding: %d <= 65535", uLen)
		}
		if uLen > uint64(c.maxFrameSize) {
			return 0, nil, fmt.Errorf("websocket: payload size %d exceeds max frame size %d", uLen, c.maxFrameSize)
		}
		payloadLen = int(uLen)
	} else {
		payloadLen = rawLen
	}

	if opcode >= 0x8 && (rawLen > 125 || payloadLen > 125) {
		return 0, nil, fmt.Errorf("websocket: control frame payload %d exceeds RFC 6455 limit of 125 bytes", payloadLen)
	}

	if payloadLen > c.maxFrameSize {
		return 0, nil, fmt.Errorf("websocket: payload size %d exceeds max frame size %d", payloadLen, c.maxFrameSize)
	}

	var maskKey []byte
	if isMasked {
		maskKey = make([]byte, 4)
		if _, err := io.ReadFull(c.bufrw.Reader, maskKey); err != nil {
			return 0, nil, err
		}
	}

	payload := make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(c.bufrw.Reader, payload); err != nil {
			return 0, nil, err
		}
	}

	if isMasked {
		for i := 0; i < payloadLen; i++ {
			payload[i] ^= maskKey[i%4]
		}
	}

	if opcode == CloseMessage {
		c.closed.Store(true)
		_ = c.conn.Close()
		return CloseMessage, payload, io.EOF
	}

	if opcode == PingMessage {
		_ = c.WriteMessage(PongMessage, payload)
		return PingMessage, payload, nil
	}

	return opcode, payload, nil
}

func (c *wsConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}

	c.writeMu.Lock()
	_ = c.writeFrameLocked(CloseMessage, []byte{0x03, 0xe8})
	c.writeMu.Unlock()

	return c.conn.Close()
}
