package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gosuda.org/hqq/bindgen/rt"
)

// Options configures browser signature and engine behavior.
type Options struct {
	UserAgent          string
	Ja3Spec            string
	ForceHTTP3         bool
	InsecureSkipVerify bool
	Timeout            time.Duration
	Pool               *rt.Pool[*Client]
}

// Transport implements net/http.RoundTripper using the isolated Chromium network engine.
type Transport struct {
	opts   Options
	closed atomic.Bool
	mu     sync.RWMutex
	netTr  *http.Transport
	pool   *rt.Pool[*Client]
}

// NewTransport constructs a new Transport configured with the given Options.
func NewTransport(opts Options) *Transport {
	if opts.UserAgent == "" {
		opts.UserAgent = DefaultUserAgent
	}
	if opts.Ja3Spec == "" {
		opts.Ja3Spec = "chrome-latest"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: opts.InsecureSkipVerify,
		NextProtos:         []string{"h2", "http/1.1"},
	}

	netTr := &http.Transport{
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: opts.Timeout,
		DialContext: (&net.Dialer{
			Timeout:   opts.Timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	p := opts.Pool
	if p == nil {
		p = DefaultPool()
	}

	return &Transport{
		opts:  opts,
		netTr: netTr,
		pool:  p,
	}
}

// RoundTrip executes a single HTTP/HTTPS transaction with authentic Chrome signatures.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.closed.Load() {
		return nil, errors.New("transport closed")
	}

	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	clone := req.Clone(ctx)
	ApplyChromeHeaders(clone.Header, t.opts.UserAgent)

	if clone.URL != nil && clone.URL.Scheme == "hqq" {
		cl, err := t.pool.Get(ctx)
		if err != nil {
			return nil, err
		}

		eng, err := cl.EngineCreate(nil)
		if err != nil {
			t.pool.Put(cl)
			return nil, err
		}

		var reqBytes []byte
		if clone.Body != nil {
			reqBytes, _ = io.ReadAll(clone.Body)
			_ = clone.Body.Close()
		}
		if len(reqBytes) == 0 {
			method := clone.Method
			if method == "" {
				method = "GET"
			}
			reqURL := "/"
			if clone.URL != nil {
				reqURL = clone.URL.String()
			}
			reqBytes = []byte(fmt.Sprintf("%s %s", method, reqURL))
		}

		headerBuf := make([]byte, 65536)
		rHandle, respHeadersRaw, status, err := cl.HttpRequest(eng, reqBytes, headerBuf)
		if err != nil {
			_ = cl.EngineDestroy(eng)
			t.pool.Put(cl)
			return nil, err
		}

		respHeader := parseResponseHeaders(respHeadersRaw)

		resp := &http.Response{
			Status:     http.StatusText(int(status)),
			StatusCode: int(status),
			Header:     respHeader,
			Body:       newHqqBodyReader(t.pool, cl, eng, rHandle),
			Request:    clone,
		}
		return resp, nil
	}

	resp, err := t.netTr.RoundTrip(clone)
	if err != nil {
		return nil, err
	}

	resp.Body = newBodyReader(resp.Body, ctx)
	return resp, nil
}

// Close marks the Transport as closed and terminates all idle network connections.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed.Store(true)
	if t.netTr != nil {
		t.netTr.CloseIdleConnections()
	}
	return nil
}

// parseResponseHeaders parses raw header lines (including HTTP/2 pseudo-headers) into http.Header.
func parseResponseHeaders(raw []byte) http.Header {
	hdr := make(http.Header)
	if len(raw) == 0 {
		return hdr
	}
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			colonIdx := strings.Index(line[1:], ":")
			if colonIdx != -1 {
				key := line[:1+colonIdx]
				val := strings.TrimSpace(line[1+colonIdx+1:])
				hdr.Add(key, val)
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			hdr.Add(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}
	return hdr
}

// bodyReader wraps an io.ReadCloser to enforce thread-safety and respect context cancellation.
type bodyReader struct {
	r      io.ReadCloser
	ctx    context.Context
	mu     sync.Mutex
	closed bool
}

func newBodyReader(r io.ReadCloser, ctx context.Context) *bodyReader {
	return &bodyReader{
		r:   r,
		ctx: ctx,
	}
}

func (b *bodyReader) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, errors.New("read on closed body")
	}
	b.mu.Unlock()

	if len(p) == 0 {
		return 0, nil
	}

	if err := b.ctx.Err(); err != nil {
		return 0, err
	}

	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		if ctxErr := b.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
	}
	return n, err
}

func (b *bodyReader) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	return b.r.Close()
}

// hqqBodyReader wraps an HQQ Request handle for chunked streaming body reads.
type hqqBodyReader struct {
	pool   *rt.Pool[*Client]
	cl     *Client
	eng    Engine
	req    Request
	mu     sync.Mutex
	closed bool
	eof    bool
	buf    []byte
}

func newHqqBodyReader(pool *rt.Pool[*Client], cl *Client, eng Engine, req Request) *hqqBodyReader {
	return &hqqBodyReader{
		pool: pool,
		cl:   cl,
		eng:  eng,
		req:  req,
		buf:  make([]byte, 65536),
	}
}

func (h *hqqBodyReader) Read(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, errors.New("read on closed HQQ body")
	}
	if h.eof {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	maxLen := uint32(len(p))
	if maxLen > uint32(len(h.buf)) {
		maxLen = uint32(len(h.buf))
	}
	chunk, eof, err := h.cl.HttpReadBody(h.req, maxLen, h.buf[:maxLen])
	if err != nil {
		return 0, err
	}
	if eof {
		h.eof = true
	}
	n := copy(p, chunk)
	if eof && n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (h *hqqBodyReader) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	var errs []error
	if err := h.cl.RequestDestroy(h.req); err != nil {
		errs = append(errs, err)
	}
	if err := h.cl.EngineDestroy(h.eng); err != nil {
		errs = append(errs, err)
	}
	if h.pool != nil && h.cl != nil {
		h.pool.Put(h.cl)
	}
	return errors.Join(errs...)
}
