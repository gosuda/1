package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// RFC 6455 message types / opcodes.
const (
	TextMessage   = 1
	BinaryMessage = 2
	CloseMessage  = 8
	PingMessage   = 9
	PongMessage   = 10
)

const (
	opContinuation = 0
	// maxMessageSize bounds the reassembled size of one message.
	maxMessageSize = 16 << 20
	// maxControlPayload is the RFC 6455 limit of a control frame.
	maxControlPayload      = 125
	netErrConnectionClosed = -100
)

// WebSocketConn is a WebSocket connection made by Chromium's network stack. The stack does the
// opening handshake (RFC 6455 over HTTP/1.1, RFC 8441 over HTTP/2), frame
// parsing and validation, masking, and permessage-deflate. ReadMessage and WriteMessage may run in
// different goroutines; calls of one kind are serialized.
type WebSocketConn interface {
	// ReadMessage returns the next message, reassembled from its fragments. A ping is answered
	// with a pong and also returned (PingMessage); a pong is returned as PongMessage. After a close
	// frame it returns CloseMessage with the close payload (2-byte status code and reason) and
	// io.EOF, and io.EOF afterwards.
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Ping(data []byte) error
	Close() error
}

// DialWebSocket opens a WebSocket connection through the Chromium network stack. rawURL is a ws,
// wss, http, or https URL. When the server refuses the handshake, the error is a *NetError and the
// returned response carries the HTTP status.
func DialWebSocket(ctx context.Context, rawURL string, opts Options) (WebSocketConn, *http.Response, error) {
	return DialWebSocketHeader(ctx, rawURL, nil, opts)
}

// DialWebSocketHeader is DialWebSocket with extra handshake request headers. Sec-WebSocket-Protocol
// lists the subprotocols to offer and Origin sets the origin; the negotiated subprotocol is in the
// response's Sec-WebSocket-Protocol header.
func DialWebSocketHeader(ctx context.Context, rawURL string, header http.Header, opts Options) (WebSocketConn, *http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("websocket: invalid url %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case "ws", "wss", "http", "https":
	default:
		return nil, nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}
	if err := opts.validate(); err != nil {
		return nil, nil, err
	}
	head, err := connectHead(rawURL, header)
	if err != nil {
		return nil, nil, err
	}

	main, err := OpenWith(ctx, DefaultOptions())
	if err != nil {
		return nil, nil, err
	}
	s := &engineSession{main: main}
	if err := s.start(ctx, opts, head, kindWebSocket); err != nil {
		s.release()
		return nil, nil, err
	}
	code, raw, id, err := s.wsWait(ctx)
	if err != nil {
		s.release()
		return nil, nil, err
	}
	if code != 0 {
		resp := failedHandshakeResponse(raw)
		ne := s.netError(code)
		s.release()
		return nil, resp, ne
	}
	reader, err := OpenWith(ctx, DefaultOptions())
	if err != nil {
		s.release()
		return nil, nil, err
	}
	resp, subprotocol := parseWebSocketHead(raw)
	resp.Request = &http.Request{Method: http.MethodGet, URL: u}
	if subprotocol != "" && resp.Header.Get("Sec-WebSocket-Protocol") == "" {
		resp.Header.Set("Sec-WebSocket-Protocol", subprotocol)
	}
	return &wsConn{s: s, reader: reader, id: id}, resp, nil
}

// engineSession is the main session of a WebSocket or WebTransport connection: it holds the engine
// and connection handles, and serves writes. All its calls are serialized by mu.
type engineSession struct {
	main *Client
	mu   sync.Mutex
	eng  Engine
	ws   Ws
	wt   Wt
	kind connKind
}

type connKind int

const (
	kindWebSocket connKind = iota + 1
	kindWebTransport
)

func (s *engineSession) start(ctx context.Context, opts Options, head []byte, kind connKind) error {
	s.kind = kind
	eng, err := s.main.ChromeEngineCreate(opts.engineOptions())
	if err != nil {
		return err
	}
	s.eng = eng
	switch kind {
	case kindWebSocket:
		s.ws, err = s.main.ChromeWsStart(eng, head)
	default:
		s.wt, err = s.main.ChromeWtStart(eng, head)
	}
	return err
}

func (s *engineSession) wsWait(ctx context.Context) (code int32, head []byte, id uint64, err error) {
	stop := context.AfterFunc(ctx, func() { _ = s.main.Conn().Cancel() })
	defer stop()
	code, head, id, err = s.main.ChromeWsWait(s.ws, make([]byte, 64<<10))
	if err == nil && code != 0 {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, 0, ctxErr
		}
	}
	return code, head, id, err
}

func (s *engineSession) netError(code int32) error { return netErrorOf(s.main, code) }

// netErrorOf names a net::Error through c, whose caller must have exclusive use of it.
func netErrorOf(c *Client, code int32) error {
	name, err := c.ChromeNetErrorName(code)
	if err != nil {
		name = "ERR_UNKNOWN"
	}
	return &NetError{Code: code, Name: name}
}

// release destroys the handles and closes the session. It is safe to call once.
func (s *engineSession) release() {
	if !s.ws.IsNil() {
		_ = s.main.ChromeWsDestroy(s.ws)
	}
	if !s.wt.IsNil() {
		_ = s.main.ChromeWtDestroy(s.wt)
	}
	if !s.eng.IsNil() {
		_ = s.main.ChromeEngineDestroy(s.eng)
	}
	_ = s.main.Close()
}

// connectHead encodes "URL\n" and one "Name: value\n" line per header.
func connectHead(rawURL string, header http.Header) ([]byte, error) {
	if strings.ContainsAny(rawURL, " \r\n") {
		return nil, fmt.Errorf("chromenet: invalid URL %q", rawURL)
	}
	var b strings.Builder
	b.WriteString(rawURL)
	b.WriteByte('\n')
	for name, values := range header {
		v := strings.Join(values, ", ")
		if strings.ContainsAny(name, ":\r\n") || strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("chromenet: invalid header %q", name)
		}
		b.WriteString(name + ": " + v + "\n")
	}
	return []byte(b.String()), nil
}

// parseWebSocketHead decodes "CODE SUBPROTOCOL\nName: value\n...".
func parseWebSocketHead(raw []byte) (*http.Response, string) {
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	status := http.StatusSwitchingProtocols
	subprotocol := ""
	if f := strings.SplitN(lines[0], " ", 2); len(f) > 0 {
		if n, err := strconv.Atoi(f[0]); err == nil {
			status = n
		}
		if len(f) == 2 {
			subprotocol = f[1]
		}
	}
	resp := &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status) + " " + http.StatusText(status),
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: make(http.Header),
		Body:   http.NoBody,
	}
	for _, line := range lines[1:] {
		if name, value, ok := strings.Cut(line, ": "); ok {
			resp.Header.Add(name, value)
		}
	}
	return resp, subprotocol
}

func failedHandshakeResponse(raw []byte) *http.Response {
	status, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(raw), " ", 2)[0]))
	if status == 0 {
		return nil
	}
	return &http.Response{StatusCode: status, Status: strconv.Itoa(status) + " " + http.StatusText(status), Header: make(http.Header), Body: http.NoBody}
}

type wsConn struct {
	s      *engineSession
	reader *Client
	id     uint64

	writeMu sync.Mutex
	readMu  sync.Mutex
	closed  atomic.Bool
	// gotClose is set once a close frame arrived; readErr is the sticky error of the read side.
	gotClose bool
	readErr  error
	buf      []byte
	msg      []byte
	msgType  int
	// inMessage is true between the first fragment of a message and its last.
	inMessage bool
}

// protocolError fails the connection: it sends a close frame with status 1002 and tears the
// connection down, and the read side reports the error from then on.
func (c *wsConn) protocolError(msg string) error {
	c.readErr = fmt.Errorf("websocket: protocol error: %s", msg)
	if !c.closed.Swap(true) {
		c.writeMu.Lock()
		_ = c.writeFrame(CloseMessage, true, []byte{0x03, 0xea})
		c.writeMu.Unlock()
		c.teardown()
	}
	return c.readErr
}

func (c *wsConn) writeFrame(opcode int, fin bool, data []byte) error {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	code, err := c.s.main.ChromeWsWrite(c.id, int32(opcode), fin, data)
	if err != nil {
		return err
	}
	if code != 0 {
		return c.s.netError(code)
	}
	return nil
}

func (c *wsConn) WriteMessage(messageType int, data []byte) error {
	if c.closed.Load() {
		return errors.New("websocket: write after close")
	}
	if len(data) > maxMessageSize {
		return fmt.Errorf("websocket: payload size %d exceeds max message size %d", len(data), maxMessageSize)
	}
	if messageType == CloseMessage || messageType == PingMessage || messageType == PongMessage {
		if len(data) > maxControlPayload {
			return fmt.Errorf("websocket: control frame payload %d exceeds RFC 6455 maximum limit of %d bytes", len(data), maxControlPayload)
		}
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return errors.New("websocket: write after close")
	}
	// Control frames are never fragmented; other messages go out in frames of at most 1 MiB.
	if messageType >= CloseMessage || len(data) <= maxChunk {
		return c.writeFrame(messageType, true, data)
	}
	opcode := messageType
	for len(data) > 0 {
		n := min(len(data), maxChunk)
		if err := c.writeFrame(opcode, n == len(data), data[:n]); err != nil {
			return err
		}
		data = data[n:]
		opcode = opContinuation
	}
	return nil
}

func (c *wsConn) Ping(data []byte) error {
	if c.closed.Load() {
		return errors.New("websocket: ping after close")
	}
	if len(data) > maxControlPayload {
		return fmt.Errorf("websocket: control frame payload %d exceeds RFC 6455 maximum limit of %d bytes", len(data), maxControlPayload)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return errors.New("websocket: ping after close")
	}
	return c.writeFrame(PingMessage, true, data)
}

func (c *wsConn) ReadMessage() (int, []byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readErr != nil {
		return 0, nil, c.readErr
	}
	if c.closed.Load() {
		return 0, nil, io.EOF
	}
	if c.buf == nil {
		c.buf = make([]byte, maxChunk)
	}
	for {
		data, opcode, fin, code, err := c.reader.ChromeWsRead(c.id, c.buf)
		if err != nil {
			c.readErr = err
			return 0, nil, err
		}
		if code != 0 {
			if c.closed.Load() {
				c.readErr = io.EOF
			} else if code == netErrConnectionClosed || code == -101 {
				c.readErr = io.ErrUnexpectedEOF
			} else {
				c.readErr = c.s.netErrorLocked(code)
			}
			return 0, nil, c.readErr
		}
		switch int(opcode) {
		case opContinuation, TextMessage, BinaryMessage:
			// The stack validates frames; the order of data frames is the message layer's job.
			if int(opcode) != opContinuation {
				if c.inMessage {
					return 0, nil, c.protocolError("data frame inside a fragmented message")
				}
				c.msgType = int(opcode)
				c.msg = c.msg[:0]
				c.inMessage = true
			} else if !c.inMessage {
				return 0, nil, c.protocolError("continuation frame without a message")
			}
			if len(c.msg)+len(data) > maxMessageSize {
				c.readErr = fmt.Errorf("websocket: message exceeds max message size %d", maxMessageSize)
				return 0, nil, c.readErr
			}
			c.msg = append(c.msg, data...)
			if fin {
				out := append([]byte(nil), c.msg...)
				c.msg = c.msg[:0]
				c.inMessage = false
				if c.msgType == TextMessage && !utf8.Valid(out) {
					return 0, nil, c.protocolError("text message is not valid UTF-8")
				}
				return c.msgType, out, nil
			}
		case PingMessage:
			payload := append([]byte(nil), data...)
			_ = c.WriteMessage(PongMessage, payload)
			return PingMessage, payload, nil
		case PongMessage:
			return PongMessage, append([]byte(nil), data...), nil
		case CloseMessage:
			payload := append([]byte(nil), data...)
			c.gotClose = true
			c.readErr = io.EOF
			// Echo the close frame so the peer's closing handshake completes, then drop the
			// connection.
			if !c.closed.Swap(true) {
				c.writeMu.Lock()
				_ = c.writeFrame(CloseMessage, true, closeEcho(payload))
				c.writeMu.Unlock()
				c.teardown()
			}
			return CloseMessage, payload, io.EOF
		}
	}
}

// closeEcho returns the payload of the close frame that answers payload: the same status code, no
// reason.
func closeEcho(payload []byte) []byte {
	if len(payload) >= 2 {
		return payload[:2]
	}
	return nil
}

func (c *wsConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.writeMu.Lock()
	_ = c.writeFrame(CloseMessage, true, []byte{0x03, 0xe8})
	c.writeMu.Unlock()
	c.teardown()
	return nil
}

// teardown destroys the connection, which wakes a blocked reader, then closes both sessions.
func (c *wsConn) teardown() {
	c.s.mu.Lock()
	_ = c.s.main.ChromeWsDestroy(c.s.ws)
	c.s.ws = Ws{}
	c.s.mu.Unlock()
	// A reader blocked in ChromeWsRead returns once the connection is gone.
	go func() {
		c.readMu.Lock()
		_ = c.reader.Close()
		c.readMu.Unlock()
	}()
	c.s.mu.Lock()
	c.s.release()
	c.s.mu.Unlock()
}

func (s *engineSession) netErrorLocked(code int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.netError(code)
}
