package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"gosuda.org/hqq/bindgen/rt"
)

// Options configures the Chromium network engine. Transports with equal options share one
// Chromium URLRequestContext: its connection pools, HTTP/2 sessions, QUIC sessions, and host
// cache.
type Options struct {
	// UserAgent replaces the engine's default desktop Chrome User-Agent. A User-Agent request
	// header still takes precedence.
	UserAgent string
	// AcceptLanguage replaces the default "en-US,en;q=0.9".
	AcceptLanguage string
	// Proxy holds Chromium proxy rules, such as "http://127.0.0.1:8080",
	// "socks5://127.0.0.1:1080", or "https=proxy:443;http=proxy:80". Empty means direct.
	Proxy string
	// DisableHTTP2 and DisableQUIC turn the protocols off.
	DisableHTTP2 bool
	DisableQUIC  bool
	// ForceHTTP3 uses QUIC for every origin without waiting for an Alt-Svc advertisement.
	ForceHTTP3 bool
	// InsecureSkipVerify accepts any server certificate.
	InsecureSkipVerify bool
	// Pool supplies engine sessions. Nil uses DefaultPool().
	Pool *rt.Pool[*Client]
}

func (o Options) engineOptions() []byte {
	var b bytes.Buffer
	add := func(k, v string) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	flag := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	if o.UserAgent != "" {
		add("user_agent", o.UserAgent)
	}
	if o.AcceptLanguage != "" {
		add("accept_language", o.AcceptLanguage)
	}
	if o.Proxy != "" {
		add("proxy", o.Proxy)
	}
	add("http2", flag(!o.DisableHTTP2))
	add("quic", flag(!o.DisableQUIC))
	add("force_quic", flag(o.ForceHTTP3))
	add("insecure_skip_verify", flag(o.InsecureSkipVerify))
	return b.Bytes()
}

func validOptionValue(v string) bool { return !strings.ContainsAny(v, "\r\n") }

// validate rejects option values that would break the line-based engine options.
func (o Options) validate() error {
	for _, v := range []string{o.UserAgent, o.AcceptLanguage, o.Proxy} {
		if !validOptionValue(v) {
			return errors.New("chromenet: option values must not contain line breaks")
		}
	}
	return nil
}

// NetError is a request failure reported by Chromium's network stack.
type NetError struct {
	Code int32  // a negative net::Error value, such as -105
	Name string // the net::Error name, such as "ERR_NAME_NOT_RESOLVED"
	// Detail is what the stack reports besides the code, such as the QUIC error behind a failed
	// WebTransport handshake. Often empty.
	Detail string
}

func (e *NetError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("chromenet: net::%s (%d): %s", e.Name, e.Code, e.Detail)
	}
	return fmt.Sprintf("chromenet: net::%s (%d)", e.Name, e.Code)
}

// ErrTransportClosed is returned by RoundTrip after Close.
var ErrTransportClosed = errors.New("chromenet: transport closed")

const (
	netErrAborted   = -3
	maxChunk        = 1 << 20
	headCap         = 1 << 20
	inlineBodyLimit = 256 << 10
)

type cachedEngine struct {
	opts string
	eng  Engine
}

// sessionEngines caches one Engine handle per pooled *Client session so sequential requests skip
// ChromeEngineCreate / ChromeEngineDestroy RPCs and keep their URLRequestContext warm.
var sessionEngines sync.Map // *Client -> *cachedEngine

func evictSessionEngine(cl *Client) {
	sessionEngines.Delete(cl)
}

// Transport implements net/http.RoundTripper on Chromium's network stack (HTTP/1.1, HTTP/2,
// QUIC/HTTP/3 and BoringSSL) running in the chrome_net_server worker. It does not follow
// redirects and keeps no cookies: http.Client does both.
type Transport struct {
	opts    Options
	engOpts []byte
	closed  atomic.Bool
}

// NewTransport constructs a Transport configured with opts.
func NewTransport(opts Options) *Transport {
	return &Transport{opts: opts, engOpts: opts.engineOptions()}
}

// pool returns Options.Pool, or the current DefaultPool (Shutdown replaces it).
func (t *Transport) pool() *rt.Pool[*Client] {
	if t.opts.Pool != nil {
		return t.opts.Pool
	}
	return DefaultPool()
}

// uploadPool returns the pool used by concurrent request body uploads.
func (t *Transport) uploadPool() *rt.Pool[*Client] {
	if t.opts.Pool != nil {
		return t.opts.Pool
	}
	return defaultUploadPool()
}

// Close makes later RoundTrip calls fail. Responses already returned stay readable.
func (t *Transport) Close() error {
	t.closed.Store(true)
	return nil
}

// RoundTrip sends req through the Chromium network stack.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	bodyOwnedByUpload := false
	if req.Body != nil {
		defer func() {
			if !bodyOwnedByUpload && req.Body != nil {
				_ = req.Body.Close()
			}
		}()
	}
	if t.closed.Load() {
		return nil, ErrTransportClosed
	}
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return nil, fmt.Errorf("chromenet: unsupported URL scheme in %v", req.URL)
	}
	if err := t.opts.validate(); err != nil {
		return nil, err
	}
	head, err := requestHead(req)
	if err != nil {
		return nil, err
	}
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pool := t.pool()
	cl, err := pool.Get(ctx)
	if err != nil {
		return nil, err
	}
	x := &exchange{t: t, pool: pool, cl: cl, ctx: ctx}
	stop := context.AfterFunc(ctx, x.cancel)
	x.stop = stop

	resp, err := x.start(req, head, &bodyOwnedByUpload)
	if err != nil {
		x.release()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if uerr := x.getUploadErr(); uerr != nil {
			return nil, uerr
		}
		return nil, err
	}
	return resp, nil
}

// exchange is one request on one engine session. The session stays checked out of the pool until
// the response body is closed.
type exchange struct {
	t            *Transport
	pool         *rt.Pool[*Client]
	cl           *Client
	ctx          context.Context
	stop         func() bool
	eng          Engine
	req          Request
	broken       bool
	canceled     atomic.Bool
	closing      atomic.Bool
	once         sync.Once
	uploadCancel context.CancelFunc
	uploadDone   chan struct{}

	uploadErrMu sync.Mutex
	uploadErr   error

	bodyMu  sync.Mutex
	curBody io.Closer
}

func (x *exchange) setUploadErr(err error) {
	if err == nil {
		return
	}
	x.uploadErrMu.Lock()
	if x.uploadErr == nil {
		x.uploadErr = err
	}
	x.uploadErrMu.Unlock()
}

func (x *exchange) getUploadErr() error {
	x.uploadErrMu.Lock()
	defer x.uploadErrMu.Unlock()
	return x.uploadErr
}

func (x *exchange) setCurrentBody(c io.Closer) {
	x.bodyMu.Lock()
	x.curBody = c
	closed := x.closing.Load()
	x.bodyMu.Unlock()
	if closed && c != nil {
		_ = c.Close()
	}
}

func (x *exchange) closeCurrentBody() {
	x.bodyMu.Lock()
	c := x.curBody
	x.curBody = nil
	x.bodyMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (x *exchange) cancel() {
	x.canceled.Store(true)
	_ = x.cl.Conn().Cancel()
}

func (x *exchange) acquireEngine() (Engine, error) {
	optsStr := string(x.t.engOpts)
	if v, ok := sessionEngines.Load(x.cl); ok {
		ce := v.(*cachedEngine)
		if ce.opts == optsStr && !ce.eng.IsNil() {
			return ce.eng, nil
		}
		sessionEngines.Delete(x.cl)
		if !ce.eng.IsNil() {
			if err := x.cl.ChromeEngineDestroy(ce.eng); err != nil {
				x.broken = true
				return Engine{}, err
			}
		}
	}
	eng, err := x.cl.ChromeEngineCreate(x.t.engOpts)
	if err != nil {
		x.broken = true
		return Engine{}, err
	}
	sessionEngines.Store(x.cl, &cachedEngine{opts: optsStr, eng: eng})
	return eng, nil
}

func (x *exchange) start(req *http.Request, head []byte, bodyOwnedByUpload *bool) (*http.Response, error) {
	eng, err := x.acquireEngine()
	if err != nil {
		return nil, err
	}
	x.eng = eng

	bodyLen := int64(0)
	if req.Body != nil && req.Body != http.NoBody {
		bodyLen = req.ContentLength
		if bodyLen <= 0 {
			bodyLen = -1 // unknown length: chunked upload
		}
	}

	var (
		flags    uint32
		initBody []byte
	)
	if bodyLen > 0 && bodyLen <= inlineBodyLimit {
		// Small known-length body fast path: read inline so the engine uses a rewindable
		// in-memory upload stream with zero extra RPCs or upload sessions.
		b, rerr := io.ReadAll(io.LimitReader(req.Body, bodyLen+1))
		if rerr != nil {
			return nil, fmt.Errorf("chromenet: read request body: %w", rerr)
		}
		if int64(len(b)) != bodyLen {
			return nil, fmt.Errorf("chromenet: ContentLength=%d with Body length %d", bodyLen, len(b))
		}
		initBody = b
		flags = CHROME_REQ_BODY_COMPLETE
	} else if bodyLen != 0 && req.GetBody != nil {
		flags = CHROME_REQ_CAN_REWIND
	}

	r, reqID, err := x.cl.ChromeRequestStart(eng, head, bodyLen, flags, initBody)
	if err != nil {
		x.broken = true
		return nil, err
	}
	x.req = r

	if bodyLen != 0 && (flags&CHROME_REQ_BODY_COMPLETE) == 0 {
		uploadCtx, uploadCancel := context.WithCancel(x.ctx)
		x.uploadCancel = uploadCancel
		x.uploadDone = make(chan struct{})
		*bodyOwnedByUpload = true
		x.setCurrentBody(req.Body)
		go x.runUpload(uploadCtx, req, bodyLen, reqID, req.GetBody != nil)
	}

	code, raw, err := x.cl.ChromeRequestWait(r, make([]byte, headCap))
	if err != nil {
		x.broken = true
		return nil, err
	}
	if code != 0 {
		return nil, x.netError(code)
	}
	resp, err := parseHead(raw)
	if err != nil {
		return nil, err
	}
	resp.Request = req
	resp.Body = &engineBody{x: x, buf: make([]byte, maxChunk)}
	return resp, nil
}

// runUpload streams the request body concurrently on a dedicated upload session while the main
// session waits for response headers / reads the response body. When Chromium requests a rewind
// (ChromeNetRewind), it reopens the body via req.GetBody and replays from byte 0 at epoch+1.
func (x *exchange) runUpload(uploadCtx context.Context, req *http.Request, bodyLen int64, reqID uint64, canRewind bool) {
	defer close(x.uploadDone)
	defer x.closeCurrentBody()

	upPool := x.t.uploadPool()
	ucl, err := upPool.Get(uploadCtx)
	if err != nil {
		if !x.closing.Load() {
			x.setUploadErr(err)
			_ = x.cl.Conn().Cancel()
		}
		return
	}
	broken := false
	stopCancel := context.AfterFunc(uploadCtx, func() {
		_ = ucl.Conn().Cancel()
	})
	defer func() {
		stopCancel()
		if broken {
			evictSessionEngine(ucl)
			upPool.Discard(ucl)
		} else {
			upPool.Put(ucl)
		}
	}()

	buf := make([]byte, maxChunk)
	var epoch uint32
	curBody := req.Body

	for {
		var reader io.Reader = curBody
		if bodyLen > 0 {
			reader = io.LimitReader(curBody, bodyLen+1)
		}
		var sent int64
		rewound := false
		for {
			n, rerr := reader.Read(buf)
			eof := rerr == io.EOF
			if rerr != nil && !eof {
				if !x.closing.Load() {
					x.setUploadErr(fmt.Errorf("chromenet: read request body: %w", rerr))
					if _, werr := ucl.ChromeRequestWrite(reqID, epoch, CHROME_WRITE_ABORT, nil); werr != nil {
						broken = true
					}
				}
				return
			}
			sent += int64(n)
			if bodyLen > 0 && (sent > bodyLen || (eof && sent != bodyLen)) {
				x.setUploadErr(fmt.Errorf("chromenet: ContentLength=%d with Body length %d", bodyLen, sent))
				if _, werr := ucl.ChromeRequestWrite(reqID, epoch, CHROME_WRITE_ABORT, nil); werr != nil {
					broken = true
				}
				return
			}
			if n == 0 && !eof {
				continue
			}
			var wflags uint32
			if eof {
				wflags |= CHROME_WRITE_EOF
			}
			code, werr := ucl.ChromeRequestWrite(reqID, epoch, wflags, buf[:n])
			if werr != nil {
				broken = true
				if !x.closing.Load() {
					x.setUploadErr(werr)
					_ = x.cl.Conn().Cancel()
				}
				return
			}
			if code == CHROME_NET_REWIND {
				if !canRewind {
					return
				}
				x.closeCurrentBody()
				nextBody, gerr := req.GetBody()
				if gerr != nil {
					x.setUploadErr(fmt.Errorf("chromenet: rewind request body: %w", gerr))
					if _, awerr := ucl.ChromeRequestWrite(reqID, epoch, CHROME_WRITE_ABORT, nil); awerr != nil {
						broken = true
					}
					return
				}
				curBody = nextBody
				x.setCurrentBody(curBody)
				epoch++
				rewound = true
				break
			}
			if code != 0 {
				return
			}
			if eof {
				return
			}
		}
		if !rewound {
			return
		}
	}
}

func (x *exchange) netError(code int32) error {
	if code == netErrAborted && x.canceled.Load() {
		if err := x.ctx.Err(); err != nil {
			return err
		}
	}
	name, err := x.cl.ChromeNetErrorName(code)
	if err != nil {
		x.broken = true
		name = "ERR_UNKNOWN"
	}
	return &NetError{Code: code, Name: name}
}

// release destroys the request handle, stops any concurrent upload, and returns the session to the
// pool (keeping its cached Engine handle warm). A session whose RPC failed is discarded.
func (x *exchange) release() {
	x.once.Do(func() {
		x.closing.Store(true)
		if x.stop != nil {
			x.stop()
		}
		if !x.broken && !x.req.IsNil() {
			if err := x.cl.ChromeRequestDestroy(x.req); err != nil {
				x.broken = true
			}
		}
		if x.uploadCancel != nil {
			x.uploadCancel()
		}
		x.closeCurrentBody()
		if x.uploadDone != nil {
			<-x.uploadDone
		}
		if x.broken {
			evictSessionEngine(x.cl)
			x.pool.Discard(x.cl)
			return
		}
		x.pool.Put(x.cl)
	})
}

// engineBody streams the decoded response body from the engine.
type engineBody struct {
	mu     sync.Mutex
	x      *exchange
	buf    []byte
	rest   []byte
	err    error
	closed bool
}

func (b *engineBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, errors.New("chromenet: read on closed response body")
	}
	for len(b.rest) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		data, eof, code, err := b.x.cl.ChromeRequestRead(b.x.req, b.buf)
		switch {
		case err != nil:
			b.x.broken = true
			b.err = err
		case code != 0:
			if code == netErrAborted {
				if uerr := b.x.getUploadErr(); uerr != nil && b.x.ctx.Err() == nil {
					b.err = uerr
					break
				}
			}
			b.err = b.x.netError(code)
		case eof:
			b.err = io.EOF
		}
		b.rest = data
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	return n, nil
}

func (b *engineBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	b.x.release()
	return nil
}

// hopHeaders are set by the network stack itself, or belong to one connection.
var hopHeaders = map[string]bool{
	"Connection":        true,
	"Content-Length":    true,
	"Host":              true,
	"Keep-Alive":        true,
	"Proxy-Connection":  true,
	"Te":                true,
	"Trailer":           true,
	"Transfer-Encoding": true,
	"Upgrade":           true,
}

// requestHead encodes "METHOD URL\n" and one "Name: value\n" line per header. Repeated values are
// joined as HTTP allows ("; " for Cookie, ", " otherwise).
func requestHead(req *http.Request) ([]byte, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if strings.ContainsAny(method, " \r\n") {
		return nil, fmt.Errorf("chromenet: invalid method %q", method)
	}
	u := req.URL.String()
	if strings.ContainsAny(u, " \r\n") {
		return nil, fmt.Errorf("chromenet: invalid URL %q", u)
	}
	var b bytes.Buffer
	b.WriteString(method)
	b.WriteByte(' ')
	b.WriteString(u)
	b.WriteByte('\n')
	for name, values := range req.Header {
		key := textproto.CanonicalMIMEHeaderKey(name)
		if hopHeaders[key] || len(values) == 0 {
			continue
		}
		sep := ", "
		if key == "Cookie" {
			sep = "; "
		}
		v := strings.Join(values, sep)
		if strings.ContainsAny(name, ":\r\n") || strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("chromenet: invalid header %q", name)
		}
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// decodedEncodings are the content codings the network stack removes before the body reaches
// the caller.
var decodedEncodings = map[string]bool{"gzip": true, "x-gzip": true, "deflate": true, "br": true, "zstd": true}

// parseHead decodes "CODE PROTOCOL REDIRECT\nSTATUS TEXT\nName: value\n...".
func parseHead(raw []byte) (*http.Response, error) {
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("chromenet: malformed response head %q", raw)
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 3 {
		return nil, fmt.Errorf("chromenet: malformed response status %q", lines[0])
	}
	code, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil, fmt.Errorf("chromenet: malformed status code %q", fields[0])
	}
	resp := &http.Response{
		StatusCode:    code,
		Status:        strings.TrimSpace(fields[0] + " " + lines[1]),
		Header:        make(http.Header, len(lines)-2),
		ContentLength: -1,
	}
	switch fields[1] {
	case "h2":
		resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/2.0", 2, 0
	case "h3":
		resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/3.0", 3, 0
	default:
		resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	}
	for _, line := range lines[2:] {
		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			name, value, ok = strings.Cut(line, ":")
		}
		if !ok {
			return nil, fmt.Errorf("chromenet: malformed response header %q", line)
		}
		resp.Header.Add(name, value)
	}
	if ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); ce != "" {
		decoded := true
		for _, c := range strings.Split(ce, ",") {
			decoded = decoded && decodedEncodings[strings.TrimSpace(c)]
		}
		if decoded {
			resp.Header.Del("Content-Encoding")
			resp.Header.Del("Content-Length")
			resp.Uncompressed = true
		}
	}
	if !resp.Uncompressed {
		if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && n >= 0 {
			resp.ContentLength = n
		}
	}
	return resp, nil
}
