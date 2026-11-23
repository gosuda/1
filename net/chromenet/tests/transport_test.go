package tests

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gosuda.org/chromenet/client"
)

const requestTimeout = 30 * time.Second

func newTransport(t *testing.T, opts client.Options) *client.Transport {
	t.Helper()
	tr := client.NewTransport(opts)
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func newRequest(t *testing.T, ctx context.Context, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

// roundTrip performs one request and reads the whole response body.
func roundTrip(t *testing.T, tr http.RoundTripper, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip %s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s %s: %v", req.Method, req.URL, err)
	}
	return resp, body
}

func pseudoRandomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{byte(seed)}).Read(b)
	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestTransport_HTTP1Get(t *testing.T) {
	var gotMethod, gotURI string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotMethod, gotURI = r.Method, r.URL.RequestURI()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Reply", "pong")
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.Header().Set("Content-Length", "11")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, body := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, srv.URL+"/path?q=1&r=a%20b", nil))

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Status, "201") {
		t.Errorf("Status = %q; want prefix 201", resp.Status)
	}
	if resp.ProtoMajor != 1 || resp.ProtoMinor != 1 {
		t.Errorf("protocol = %d.%d; want 1.1", resp.ProtoMajor, resp.ProtoMinor)
	}
	if string(body) != "hello world" {
		t.Errorf("body = %q; want %q", body, "hello world")
	}
	if resp.ContentLength != 11 {
		t.Errorf("ContentLength = %d; want 11", resp.ContentLength)
	}
	if got := resp.Header.Get("X-Reply"); got != "pong" {
		t.Errorf("X-Reply = %q; want pong", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
		t.Errorf("Set-Cookie = %q; want [a=1 b=2]", got)
	}
	if resp.Request == nil || resp.Request.URL.String() != srv.URL+"/path?q=1&r=a%20b" {
		t.Errorf("resp.Request = %v; want the request that was sent", resp.Request)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodGet || gotURI != "/path?q=1&r=a%20b" {
		t.Errorf("server saw %s %s; want GET /path?q=1&r=a%%20b", gotMethod, gotURI)
	}
}

func TestTransport_HTTP2(t *testing.T) {
	var serverProto atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverProto.Store(int32(r.ProtoMajor))
		_, _ = io.WriteString(w, "over h2")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := newTransport(t, client.Options{InsecureSkipVerify: true, DisableQUIC: true})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, body := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, srv.URL, nil))
	if resp.ProtoMajor != 2 {
		t.Errorf("resp.ProtoMajor = %d; want 2", resp.ProtoMajor)
	}
	if got := serverProto.Load(); got != 2 {
		t.Errorf("server saw HTTP/%d; want HTTP/2", got)
	}
	if string(body) != "over h2" {
		t.Errorf("body = %q", body)
	}
}

func TestTransport_HTTP2Disabled(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := newTransport(t, client.Options{InsecureSkipVerify: true, DisableHTTP2: true, DisableQUIC: true})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, _ := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, srv.URL, nil))
	if resp.ProtoMajor != 1 {
		t.Errorf("resp.ProtoMajor = %d with DisableHTTP2; want 1", resp.ProtoMajor)
	}
}

func TestTransport_RequestHeaders(t *testing.T) {
	var mu sync.Mutex
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	do := func(t *testing.T, opts client.Options, mutate func(*http.Request)) http.Header {
		t.Helper()
		req := newRequest(t, ctx, http.MethodGet, srv.URL, nil)
		if mutate != nil {
			mutate(req)
		}
		roundTrip(t, newTransport(t, opts), req)
		mu.Lock()
		defer mu.Unlock()
		return seen
	}

	t.Run("custom and repeated headers", func(t *testing.T) {
		h := do(t, client.Options{}, func(r *http.Request) {
			r.Header.Set("X-Custom", "custom-value")
			r.Header.Add("X-Multi", "one")
			r.Header.Add("X-Multi", "two")
			r.Header.Add("Cookie", "a=1")
			r.Header.Add("Cookie", "b=2")
		})
		if got := h.Get("X-Custom"); got != "custom-value" {
			t.Errorf("X-Custom = %q", got)
		}
		if got := h.Get("X-Multi"); got != "one, two" {
			t.Errorf("X-Multi = %q; want %q", got, "one, two")
		}
		if got := h.Values("Cookie"); len(got) != 1 || got[0] != "a=1; b=2" {
			t.Errorf("Cookie = %q; want a single %q", got, "a=1; b=2")
		}
	})

	t.Run("default User-Agent is Chrome", func(t *testing.T) {
		h := do(t, client.Options{}, nil)
		if ua := h.Get("User-Agent"); !strings.Contains(ua, "Chrome/") {
			t.Errorf("User-Agent = %q; want it to contain Chrome/", ua)
		}
	})

	t.Run("Options.UserAgent", func(t *testing.T) {
		const ua = "chromenet-test/1.0 (custom)"
		h := do(t, client.Options{UserAgent: ua}, nil)
		if got := h.Get("User-Agent"); got != ua {
			t.Errorf("User-Agent = %q; want %q", got, ua)
		}
	})

	t.Run("request User-Agent beats Options.UserAgent", func(t *testing.T) {
		h := do(t, client.Options{UserAgent: "from-options"}, func(r *http.Request) {
			r.Header.Set("User-Agent", "from-request")
		})
		if got := h.Get("User-Agent"); got != "from-request" {
			t.Errorf("User-Agent = %q; want from-request", got)
		}
	})

	t.Run("Options.AcceptLanguage", func(t *testing.T) {
		h := do(t, client.Options{AcceptLanguage: "ko-KR,ko;q=0.9"}, nil)
		if got := h.Get("Accept-Language"); got != "ko-KR,ko;q=0.9" {
			t.Errorf("Accept-Language = %q", got)
		}
	})
}

func TestTransport_RejectsHeaderInjection(t *testing.T) {
	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req := newRequest(t, ctx, http.MethodGet, "http://127.0.0.1:1/", nil)
	req.Header.Set("X-Evil", "a\nHost: evil.example")
	if resp, err := tr.RoundTrip(req); err == nil {
		resp.Body.Close()
		t.Fatal("RoundTrip accepted a header value containing a line break")
	}

	bad := client.Options{UserAgent: "ua\nX-Injected: 1"}
	req = newRequest(t, ctx, http.MethodGet, "http://127.0.0.1:1/", nil)
	if resp, err := newTransport(t, bad).RoundTrip(req); err == nil {
		resp.Body.Close()
		t.Fatal("RoundTrip accepted an Options.UserAgent containing a line break")
	}
}

func TestTransport_PostKnownLength(t *testing.T) {
	// Not a multiple of the 1 MiB chunk size, and spanning several chunks.
	payload := pseudoRandomBytes(3<<20+17, 1)

	var gotLen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen.Store(r.ContentLength)
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, "%d %s", len(b), sha256Hex(b))
	}))
	defer srv.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req := newRequest(t, ctx, http.MethodPost, srv.URL, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	if req.ContentLength != int64(len(payload)) {
		t.Fatalf("test setup: ContentLength = %d", req.ContentLength)
	}
	resp, body := roundTrip(t, tr, req)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body %q", resp.StatusCode, body)
	}
	if want := fmt.Sprintf("%d %s", len(payload), sha256Hex(payload)); string(body) != want {
		t.Errorf("server received %q; want %q", body, want)
	}
	if got := gotLen.Load(); got != int64(len(payload)) {
		t.Errorf("server saw Content-Length %d; want %d", got, len(payload))
	}
}

func TestTransport_PostUnknownLength(t *testing.T) {
	payload := pseudoRandomBytes(3<<20+5, 2)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, "%d %s", len(b), sha256Hex(b))
	}))
	defer srv.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	pr, pw := io.Pipe()
	go func() {
		// Irregular write sizes so chunk boundaries never line up with the engine's.
		rest := payload
		for step := 1000; len(rest) > 0; step = step*3 + 7 {
			n := min(step, len(rest))
			if _, err := pw.Write(rest[:n]); err != nil {
				return
			}
			rest = rest[n:]
		}
		_ = pw.Close()
	}()

	req := newRequest(t, ctx, http.MethodPost, srv.URL, pr)
	if req.ContentLength > 0 {
		t.Fatalf("test setup: ContentLength = %d; want unknown", req.ContentLength)
	}
	resp, body := roundTrip(t, tr, req)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body %q", resp.StatusCode, body)
	}
	if want := fmt.Sprintf("%d %s", len(payload), sha256Hex(payload)); string(body) != want {
		t.Errorf("server received %q; want %q", body, want)
	}
}

func TestTransport_PostBodyReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	boom := errors.New("boom")
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("partial"))
		_ = pw.CloseWithError(boom)
	}()
	resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodPost, srv.URL, pr))
	if err == nil {
		resp.Body.Close()
		t.Fatal("RoundTrip succeeded although the request body failed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("RoundTrip error = %v; want it to wrap the body error", err)
	}
}

func TestTransport_RedirectsAreNotFollowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "landed")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, _ := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, srv.URL+"/start", nil))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d; want 302", resp.StatusCode)
	}
	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("Location: %v (header %q)", err, resp.Header.Get("Location"))
	}
	if loc.String() != srv.URL+"/target" {
		t.Errorf("Location = %s; want %s", loc, srv.URL+"/target")
	}
}

func TestTransport_HTTPClientFollowsRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "landed")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &http.Client{Transport: newTransport(t, client.Options{}), Timeout: requestTimeout}
	resp, err := c.Get(srv.URL + "/start")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "landed" {
		t.Errorf("got %d %q; want 200 %q", resp.StatusCode, body, "landed")
	}
	if got := resp.Request.URL.String(); got != srv.URL+"/target" {
		t.Errorf("final URL = %s; want %s", got, srv.URL+"/target")
	}
}

func TestTransport_GzipIsDecoded(t *testing.T) {
	plain := bytes.Repeat([]byte("compress me, compress me. "), 8192)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write(plain)
	_ = zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer srv.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, body := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, srv.URL, nil))
	if !bytes.Equal(body, plain) {
		t.Errorf("body is %d bytes, not the %d-byte plain text", len(body), len(plain))
	}
	if !resp.Uncompressed {
		t.Error("resp.Uncompressed = false; want true")
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q after decoding; want none", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q after decoding; want none", got)
	}
}

func TestTransport_ContextCancelWaitingForHeaders(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(300*time.Millisecond, cancel)

	start := time.Now()
	resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodGet, srv.URL, nil))
	if err == nil {
		resp.Body.Close()
		t.Fatal("RoundTrip succeeded against a server that never responds")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v; want context.Canceled", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("cancellation took %v; want < 5s", d)
	}
}

func TestTransport_ContextCancelDuringBody(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodGet, srv.URL, nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "first" {
		t.Fatalf("first chunk = %q, %v", buf, err)
	}

	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("body read error = %v; want context.Canceled", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("cancellation took %v; want < 5s", d)
	}
}

func TestTransport_ContextDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodGet, srv.URL, nil))
	if err == nil {
		resp.Body.Close()
		t.Fatal("RoundTrip succeeded against a server that never responds")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v; want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("deadline took %v to fire; want < 5s", d)
	}
}

func TestTransport_ConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodGet, "http://"+addr+"/", nil))
	if err == nil {
		resp.Body.Close()
		t.Fatal("RoundTrip to a closed port succeeded")
	}
	var ne *client.NetError
	if !errors.As(err, &ne) {
		t.Fatalf("error = %T %v; want *client.NetError", err, err)
	}
	if ne.Name != "ERR_CONNECTION_REFUSED" || ne.Code != -102 {
		t.Errorf("NetError = %s (%d); want ERR_CONNECTION_REFUSED (-102)", ne.Name, ne.Code)
	}
}

func TestTransport_UntrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secret")
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	t.Run("verification on", func(t *testing.T) {
		tr := newTransport(t, client.Options{})
		resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodGet, srv.URL, nil))
		if err == nil {
			resp.Body.Close()
			t.Fatal("RoundTrip trusted a self-signed certificate")
		}
		var ne *client.NetError
		if !errors.As(err, &ne) {
			t.Fatalf("error = %T %v; want *client.NetError", err, err)
		}
		if !strings.HasPrefix(ne.Name, "ERR_CERT_") {
			t.Errorf("NetError name = %s; want ERR_CERT_*", ne.Name)
		}
	})

	t.Run("InsecureSkipVerify", func(t *testing.T) {
		tr := newTransport(t, client.Options{InsecureSkipVerify: true})
		_, body := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, srv.URL, nil))
		if string(body) != "secret" {
			t.Errorf("body = %q", body)
		}
	})
}

func TestTransport_ConcurrentRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "echo:"+r.URL.Query().Get("i"))
	}))
	defer srv.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*requestTimeout)
	defer cancel()

	const n = 32
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/?i=%d", srv.URL, i), nil)
			if err != nil {
				errs <- err
				return
			}
			resp, err := tr.RoundTrip(req)
			if err != nil {
				errs <- fmt.Errorf("request %d: %w", i, err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errs <- fmt.Errorf("request %d: read: %w", i, err)
				return
			}
			if want := fmt.Sprintf("echo:%d", i); string(body) != want {
				errs <- fmt.Errorf("request %d: body %q; want %q", i, body, want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestTransport_ClosedTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "before")
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "-after")
	}))
	defer srv.Close()

	tr := client.NewTransport(client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	// A response obtained before Close stays readable.
	resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodGet, srv.URL, nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "before-after" {
		t.Errorf("body after Close = %q, %v; want %q", body, err, "before-after")
	}

	_, err = tr.RoundTrip(newRequest(t, ctx, http.MethodGet, srv.URL, nil))
	if !errors.Is(err, client.ErrTransportClosed) {
		t.Fatalf("RoundTrip after Close = %v; want client.ErrTransportClosed", err)
	}
}

func TestTransport_LargeStreamingResponse(t *testing.T) {
	data := pseudoRandomBytes(8<<20, 3)
	want := sha256Hex(data)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		flusher := w.(http.Flusher)
		for rest := data; len(rest) > 0; {
			n := min(64<<10, len(rest))
			if _, err := w.Write(rest[:n]); err != nil {
				return
			}
			flusher.Flush()
			rest = rest[n:]
		}
	}))
	defer srv.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*requestTimeout)
	defer cancel()

	resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodGet, srv.URL, nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	h := sha256.New()
	// Odd read size so reads straddle the engine's internal chunk boundaries.
	n, err := io.CopyBuffer(h, struct{ io.Reader }{resp.Body}, make([]byte, 100003))
	if err != nil {
		t.Fatalf("read body after %d bytes: %v", n, err)
	}
	if n != int64(len(data)) {
		t.Errorf("received %d bytes; want %d", n, len(data))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		t.Errorf("sha256 = %s; want %s", got, want)
	}
}

func TestTransport_EarlyBodyCloseReleasesSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("X"), 4<<20))
	}))
	defer srv.Close()

	tr := newTransport(t, client.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*requestTimeout)
	defer cancel()

	// More abandoned responses than the pool has sessions: each Close must return its session.
	for i := range 64 {
		resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodGet, srv.URL, nil))
		if err != nil {
			t.Fatalf("RoundTrip %d: %v", i, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
}

func TestTransport_SequentialConnectionReuse(t *testing.T) {
	t.Run("HTTP/1.1", func(t *testing.T) {
		var mu sync.Mutex
		var addrs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			addrs = append(addrs, r.RemoteAddr)
			mu.Unlock()
			_, _ = io.WriteString(w, "ok")
		}))
		defer srv.Close()

		tr := newTransport(t, client.Options{})
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		for i := range 5 {
			resp, body := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, srv.URL, nil))
			if resp.StatusCode != http.StatusOK || string(body) != "ok" {
				t.Fatalf("req %d: got %d %q", i, resp.StatusCode, body)
			}
			// Brief idle gap so in-flight concurrency drops to 0 between requests.
			time.Sleep(15 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(addrs) != 5 {
			t.Fatalf("got %d requests; want 5", len(addrs))
		}
		for i := 1; i < len(addrs); i++ {
			if addrs[i] != addrs[0] {
				t.Errorf("request %d used RemoteAddr %s; want reused %s (all: %v)", i, addrs[i], addrs[0], addrs)
			}
		}
	})

	t.Run("HTTP/2", func(t *testing.T) {
		var mu sync.Mutex
		var addrs []string
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			addrs = append(addrs, r.RemoteAddr)
			mu.Unlock()
			_, _ = io.WriteString(w, "h2")
		}))
		srv.EnableHTTP2 = true
		srv.StartTLS()
		defer srv.Close()

		tr := newTransport(t, client.Options{InsecureSkipVerify: true, DisableQUIC: true})
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		for i := range 5 {
			resp, body := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, srv.URL, nil))
			if resp.ProtoMajor != 2 || string(body) != "h2" {
				t.Fatalf("req %d: proto=%d body=%q", i, resp.ProtoMajor, body)
			}
			time.Sleep(15 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(addrs) != 5 {
			t.Fatalf("got %d requests; want 5", len(addrs))
		}
		for i := 1; i < len(addrs); i++ {
			if addrs[i] != addrs[0] {
				t.Errorf("request %d used RemoteAddr %s; want reused %s (all: %v)", i, addrs[i], addrs[0], addrs)
			}
		}
	})

	t.Run("survives zero active engine handles", func(t *testing.T) {
		var mu sync.Mutex
		var addrs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			addrs = append(addrs, r.RemoteAddr)
			mu.Unlock()
			_, _ = io.WriteString(w, "ok")
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		// Use a dedicated options value and close the pool between requests so active engine
		// handles on the server drop to 0; the server's idle SharedContext TTL must preserve the
		// keep-alive socket.
		ua := fmt.Sprintf("chromenet-ttl-test-%d", time.Now().UnixNano())
		for i := range 2 {
			pool := client.NewClientPool(1, client.DefaultOptions())
			tr := client.NewTransport(client.Options{UserAgent: ua, Pool: pool})
			_, body := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, srv.URL, nil))
			if string(body) != "ok" {
				t.Fatalf("req %d: body=%q", i, body)
			}
			_ = tr.Close()
			_ = pool.Close()
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(addrs) != 2 || addrs[0] != addrs[1] {
			t.Errorf("expected connection reuse across zero active handles; got RemoteAddrs %v", addrs)
		}
	})
}

// startDropOnceUploadServer accepts HTTP/1.1 connections:
//   - On conn 1: answers GET /warm with keep-alive, then reads headers + dropAfterBytes of the
//     next POST /upload and closes the socket with no response (simulating a stale keep-alive drop).
//   - On conn 2: reads the full POST /upload body and responds 200 with "<len> <sha256>".
func startDropOnceUploadServer(t *testing.T, dropAfterBytes int64) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	readHTTPHead := func(br *bufio.Reader) (string, int64, error) {
		reqLine, err := br.ReadString('\n')
		if err != nil {
			return "", 0, err
		}
		var cl int64
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return "", 0, err
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				break
			}
			if k, v, ok := strings.Cut(trimmed, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
				cl, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			}
		}
		return strings.TrimSpace(reqLine), cl, nil
	}

	var connNum atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			id := connNum.Add(1)
			go func(conn net.Conn, connID int32) {
				defer conn.Close()
				br := bufio.NewReaderSize(conn, 64<<10)
				for {
					reqLine, cl, err := readHTTPHead(br)
					if err != nil {
						return
					}
					if strings.HasPrefix(reqLine, "GET /warm ") {
						_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nok")
						continue
					}
					if connID == 1 {
						// Drop the reused keep-alive connection during/after reading dropAfterBytes.
						toRead := cl
						if dropAfterBytes >= 0 && dropAfterBytes < toRead {
							toRead = dropAfterBytes
						}
						if toRead > 0 {
							_, _ = io.CopyN(io.Discard, br, toRead)
						}
						return
					}
					body := make([]byte, cl)
					if _, err := io.ReadFull(br, body); err != nil {
						return
					}
					reply := fmt.Sprintf("%d %s", len(body), sha256Hex(body))
					_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(reply), reply)
					return
				}
			}(c, id)
		}
	}()
	return "http://" + ln.Addr().String()
}

func TestTransport_UploadRewind(t *testing.T) {
	t.Run("inline small body rewinds without GetBody", func(t *testing.T) {
		baseURL := startDropOnceUploadServer(t, -1)
		tr := newTransport(t, client.Options{})
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		_, body := roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, baseURL+"/warm", nil))
		if string(body) != "ok" {
			t.Fatalf("warm body = %q", body)
		}

		payload := pseudoRandomBytes(64<<10+13, 11)
		req := newRequest(t, ctx, http.MethodPost, baseURL+"/upload", io.NopCloser(bytes.NewReader(payload)))
		req.ContentLength = int64(len(payload))
		req.GetBody = nil

		resp, got := roundTrip(t, tr, req)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d; body %q", resp.StatusCode, got)
		}
		if want := fmt.Sprintf("%d %s", len(payload), sha256Hex(payload)); string(got) != want {
			t.Errorf("server received %q; want %q", got, want)
		}
	})

	t.Run("streaming body mid-upload rewind with GetBody", func(t *testing.T) {
		baseURL := startDropOnceUploadServer(t, 64<<10)
		tr := newTransport(t, client.Options{})
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		_, _ = roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, baseURL+"/warm", nil))

		payload := pseudoRandomBytes(2<<20+31, 12)
		var getBodyCalls atomic.Int32
		req := newRequest(t, ctx, http.MethodPost, baseURL+"/upload", bytes.NewReader(payload))
		req.GetBody = func() (io.ReadCloser, error) {
			getBodyCalls.Add(1)
			return io.NopCloser(bytes.NewReader(payload)), nil
		}

		resp, got := roundTrip(t, tr, req)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d; body %q", resp.StatusCode, got)
		}
		if want := fmt.Sprintf("%d %s", len(payload), sha256Hex(payload)); string(got) != want {
			t.Errorf("server received %q; want %q", got, want)
		}
		if getBodyCalls.Load() < 1 {
			t.Errorf("GetBody called %d times; want >= 1", getBodyCalls.Load())
		}
	})

	t.Run("streaming body post-EOF rewind with GetBody", func(t *testing.T) {
		baseURL := startDropOnceUploadServer(t, -1)
		tr := newTransport(t, client.Options{})
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		_, _ = roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, baseURL+"/warm", nil))

		payload := pseudoRandomBytes(2<<20+7, 13)
		var getBodyCalls atomic.Int32
		req := newRequest(t, ctx, http.MethodPost, baseURL+"/upload", bytes.NewReader(payload))
		req.GetBody = func() (io.ReadCloser, error) {
			getBodyCalls.Add(1)
			return io.NopCloser(bytes.NewReader(payload)), nil
		}

		resp, got := roundTrip(t, tr, req)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d; body %q", resp.StatusCode, got)
		}
		if want := fmt.Sprintf("%d %s", len(payload), sha256Hex(payload)); string(got) != want {
			t.Errorf("server received %q; want %q", got, want)
		}
		if getBodyCalls.Load() < 1 {
			t.Errorf("GetBody called %d times; want >= 1", getBodyCalls.Load())
		}
	})

	t.Run("streaming body without GetBody returns ERR_UPLOAD_STREAM_REWIND_NOT_SUPPORTED", func(t *testing.T) {
		baseURL := startDropOnceUploadServer(t, 64<<10)
		tr := newTransport(t, client.Options{})
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		_, _ = roundTrip(t, tr, newRequest(t, ctx, http.MethodGet, baseURL+"/warm", nil))

		payload := pseudoRandomBytes(2<<20+19, 14)
		req := newRequest(t, ctx, http.MethodPost, baseURL+"/upload", io.NopCloser(bytes.NewReader(payload)))
		req.ContentLength = int64(len(payload))
		req.GetBody = nil

		resp, err := tr.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
			t.Fatal("RoundTrip succeeded without GetBody although reused connection dropped mid-upload")
		}
		var ne *client.NetError
		if !errors.As(err, &ne) {
			t.Fatalf("error = %T %v; want *client.NetError", err, err)
		}
		if ne.Code != -25 {
			t.Errorf("NetError = %s (%d); want ERR_UPLOAD_STREAM_REWIND_NOT_SUPPORTED (-25)", ne.Name, ne.Code)
		}
	})
}

func TestTransport_EarlyResponseDuringUpload(t *testing.T) {
	run := func(t *testing.T, h2 bool, status int, reply string) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 16)
			_, _ = io.ReadFull(r.Body, buf)
			if !h2 {
				w.Header().Set("Connection", "close")
			}
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, reply)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		})
		var srv *httptest.Server
		var opts client.Options
		if h2 {
			srv = httptest.NewUnstartedServer(handler)
			srv.EnableHTTP2 = true
			srv.StartTLS()
			opts = client.Options{InsecureSkipVerify: true, DisableQUIC: true}
		} else {
			srv = httptest.NewServer(handler)
		}
		defer srv.Close()

		tr := newTransport(t, opts)
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		pr, pw := io.Pipe()
		unblock := make(chan struct{})
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			defer pw.Close()
			if _, err := pw.Write([]byte("0123456789abcdef")); err != nil {
				return
			}
			// Keep writing until the pipe is closed or test finishes.
			chunk := bytes.Repeat([]byte("A"), 16<<10)
			for {
				select {
				case <-unblock:
					return
				default:
					if _, err := pw.Write(chunk); err != nil {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}()
		defer func() {
			close(unblock)
			_ = pw.Close()
			<-writerDone
		}()

		start := time.Now()
		resp, err := tr.RoundTrip(newRequest(t, ctx, http.MethodPost, srv.URL, pr))
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		if resp.StatusCode != status {
			resp.Body.Close()
			t.Fatalf("status = %d; want %d", resp.StatusCode, status)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			resp.Body.Close()
			t.Fatalf("ReadAll: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if string(body) != reply {
			t.Errorf("body = %q; want %q", body, reply)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("early %d response took %v; want < 5s", status, d)
		}
	}

	t.Run("HTTP/1.1 413", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			// On Windows loopback TCP, closing a socket while the client is still writing unread
			// bytes sends a TCP RST that discards the buffered 413 response before HttpStreamParser
			// can read it.
			t.Skip("Windows Winsock RST discards unread HTTP/1.1 response on server close during active upload")
		}
		run(t, false, http.StatusRequestEntityTooLarge, "too large")
	})
	t.Run("HTTP/2 413", func(t *testing.T) { run(t, true, http.StatusRequestEntityTooLarge, "too large") })
	t.Run("HTTP/2 401", func(t *testing.T) { run(t, true, http.StatusUnauthorized, "unauthorized") })
}

func TestTransport_HTTP2StreamingUpload(t *testing.T) {
	payload := pseudoRandomBytes(2<<20+11, 21)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, "%d %s", len(b), sha256Hex(b))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := newTransport(t, client.Options{InsecureSkipVerify: true, DisableQUIC: true})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	pr, pw := io.Pipe()
	go func() {
		for rest := payload; len(rest) > 0; {
			n := min(64<<10, len(rest))
			if _, err := pw.Write(rest[:n]); err != nil {
				return
			}
			rest = rest[n:]
		}
		_ = pw.Close()
	}()

	resp, body := roundTrip(t, tr, newRequest(t, ctx, http.MethodPost, srv.URL, pr))
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK {
		t.Fatalf("got HTTP/%d status %d; want HTTP/2 200", resp.ProtoMajor, resp.StatusCode)
	}
	if want := fmt.Sprintf("%d %s", len(payload), sha256Hex(payload)); string(body) != want {
		t.Errorf("server received %q; want %q", body, want)
	}
}
