package tests

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"gosuda.org/chromenet/client"
)

const wtTimeout = 20 * time.Second

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "chromenet test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startWTServer serves WebTransport on a loopback UDP port and hands every session to onSession,
// which runs on its own goroutine.
func startWTServer(t *testing.T, onSession func(*webtransport.Session)) string {
	t.Helper()
	h3 := &http3.Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}, NextProtos: []string{http3.NextProtoH3}}}
	webtransport.ConfigureHTTP3Server(h3)
	srv := &webtransport.Server{H3: h3, CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		sess, err := srv.Upgrade(w, r)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		onSession(sess)
	})
	h3.Handler = mux
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(udp) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = udp.Close()
	})
	return "https://127.0.0.1:" + strconv.Itoa(udp.LocalAddr().(*net.UDPAddr).Port) + "/wt"
}

func dialWT(t *testing.T, url string) client.WebTransportSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wtTimeout)
	defer cancel()
	s, err := client.DialWebTransport(ctx, url, client.Options{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("DialWebTransport: %v", err)
	}
	t.Cleanup(func() { _ = s.CloseWithError(0, "") })
	return s
}

// echoStreams echoes every bidirectional stream until the session ends.
func echoStreams(sess *webtransport.Session) {
	for {
		st, err := sess.AcceptStream(context.Background())
		if err != nil {
			return
		}
		go func() {
			_, _ = io.Copy(st, st)
			_ = st.Close()
		}()
	}
}

func TestWebTransport_StreamEcho(t *testing.T) {
	url := startWTServer(t, echoStreams)
	s := dialWT(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), wtTimeout)
	defer cancel()
	st, err := s.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := st.Write([]byte("hello webtransport")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := st.Close(); err != nil { // FIN: the echo ends and the read side sees EOF
		t.Fatalf("Close: %v", err)
	}
	got, err := io.ReadAll(st)
	if err != nil || string(got) != "hello webtransport" {
		t.Fatalf("echo = %q, %v", got, err)
	}
}

func TestWebTransport_LargeStreamBothDirections(t *testing.T) {
	url := startWTServer(t, echoStreams)
	s := dialWT(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := s.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	payload := pseudoRandomBytes(8<<20, 11) // far above the 1 MiB receive window
	werr := make(chan error, 1)
	go func() {
		_, err := st.Write(payload)
		if err == nil {
			err = st.Close()
		}
		werr <- err
	}()
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := <-werr; err != nil {
		t.Fatalf("write side: %v", err)
	}
	if sha256Hex(got) != sha256Hex(payload) {
		t.Fatalf("echoed %d bytes with a different digest than the %d sent", len(got), len(payload))
	}
}

func TestWebTransport_ClientUnidirectionalStream(t *testing.T) {
	received := make(chan []byte, 1)
	url := startWTServer(t, func(sess *webtransport.Session) {
		st, err := sess.AcceptUniStream(context.Background())
		if err != nil {
			return
		}
		b, _ := io.ReadAll(st)
		received <- b
	})
	s := dialWT(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), wtTimeout)
	defer cancel()
	st, err := s.OpenUniStream(ctx)
	if err != nil {
		t.Fatalf("OpenUniStream: %v", err)
	}
	if _, err := st.Write([]byte("one way")); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read on a send-only stream succeeded")
	}
	select {
	case b := <-received:
		if string(b) != "one way" {
			t.Fatalf("server read %q", b)
		}
	case <-time.After(wtTimeout):
		t.Fatal("server did not receive the stream")
	}
}

func TestWebTransport_ServerOpenedStreams(t *testing.T) {
	url := startWTServer(t, func(sess *webtransport.Session) {
		uni, err := sess.OpenUniStreamSync(context.Background())
		if err != nil {
			return
		}
		_, _ = uni.Write([]byte("from server, uni"))
		_ = uni.Close()
		bidi, err := sess.OpenStreamSync(context.Background())
		if err != nil {
			return
		}
		_, _ = bidi.Write([]byte("from server, bidi"))
		buf := make([]byte, 16)
		n, _ := io.ReadFull(bidi, buf[:4])
		_, _ = bidi.Write(buf[:n]) // echo 4 bytes back
		_ = bidi.Close()
	})
	s := dialWT(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), wtTimeout)
	defer cancel()
	got := map[string]io.ReadWriteCloser{}
	for i := range 2 {
		st, err := s.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("AcceptStream %d: %v", i, err)
		}
		buf := make([]byte, len("from server, bidi"))
		n, _ := io.ReadFull(st, buf)
		if n < len(buf) { // the uni stream is one byte shorter
			buf = buf[:n]
		}
		got[string(bytes.TrimRight(buf, "\x00"))] = st
	}
	if got["from server, uni"] == nil || got["from server, bidi"] == nil {
		t.Fatalf("accepted streams carried %v", keys(got))
	}
	if _, err := got["from server, uni"].Write([]byte("x")); err == nil {
		t.Fatal("Write on a receive-only stream succeeded")
	}
	bidi := got["from server, bidi"]
	if _, err := bidi.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(bidi, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("bidi echo = %q, %v", echo, err)
	}
}

func keys(m map[string]io.ReadWriteCloser) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestWebTransport_Datagrams(t *testing.T) {
	url := startWTServer(t, func(sess *webtransport.Session) {
		for {
			d, err := sess.ReceiveDatagram(context.Background())
			if err != nil {
				return
			}
			_ = sess.SendDatagram(append([]byte("echo:"), d...))
		}
	})
	s := dialWT(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), wtTimeout)
	defer cancel()
	max, err := s.MaxDatagramSize()
	if err != nil || max < 500 || max > 1500 {
		t.Fatalf("MaxDatagramSize = %d, %v; want a QUIC datagram size (about 1.2 KB)", max, err)
	}
	for i := range 5 {
		msg := []byte(fmt.Sprintf("dgram-%d", i))
		if err := s.SendDatagram(msg); err != nil {
			t.Fatalf("SendDatagram: %v", err)
		}
		got, err := s.ReceiveDatagram(ctx)
		if err != nil || string(got) != "echo:"+string(msg) {
			t.Fatalf("datagram %d = %q, %v", i, got, err)
		}
	}
	err = s.SendDatagram(make([]byte, 64<<10))
	var ne *client.NetError
	if !errors.As(err, &ne) || ne.Name != "ERR_MSG_TOO_BIG" {
		t.Fatalf("oversized datagram error = %v; want ERR_MSG_TOO_BIG", err)
	}
}

func TestWebTransport_ClientClosesSession(t *testing.T) {
	closed := make(chan error, 1)
	url := startWTServer(t, func(sess *webtransport.Session) {
		// AcceptStream returns the error the session closed with.
		_, err := sess.AcceptStream(context.Background())
		closed <- err
	})
	s := dialWT(t, url)
	if err := s.CloseWithError(7, "done here"); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}
	select {
	case cause := <-closed:
		var se *webtransport.SessionError
		if !errors.As(cause, &se) || se.ErrorCode != 7 || se.Message != "done here" {
			t.Fatalf("server saw %v; want session error 7 %q", cause, "done here")
		}
	case <-time.After(wtTimeout):
		t.Fatal("server did not see the close")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var se *client.SessionError
	if _, err := s.AcceptStream(ctx); !errors.As(err, &se) || se.Code != 7 {
		t.Fatalf("AcceptStream after close = %v; want session error 7", err)
	}
	if _, err := s.OpenStream(ctx); err == nil {
		t.Fatal("OpenStream after close succeeded")
	}
	if err := s.CloseWithError(8, "again"); err != nil {
		t.Fatalf("second CloseWithError: %v", err)
	}
}

func TestWebTransport_ServerClosesSession(t *testing.T) {
	url := startWTServer(t, func(sess *webtransport.Session) {
		time.Sleep(200 * time.Millisecond)
		_ = sess.CloseWithError(9, "server says bye")
	})
	s := dialWT(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), wtTimeout)
	defer cancel()
	// When the peer closes by sending the close capsule and then stopping the CONNECT stream (as
	// quic-go does), Chromium reports the stream abort before it has processed the capsule and
	// the session ends with a transport error instead of the application code.
	var se *client.SessionError
	_, err := s.ReceiveDatagram(ctx)
	if !errors.As(err, &se) {
		t.Fatalf("ReceiveDatagram = %v; want a session error", err)
	}
	if !se.Transport && (se.Code != 9 || se.Reason != "server says bye") {
		t.Fatalf("session error = %+v; want code 9 %q or a transport error", se, "server says bye")
	}
}

func TestWebTransport_StreamAborts(t *testing.T) {
	serverSawStop := make(chan error, 1)
	url := startWTServer(t, func(sess *webtransport.Session) {
		// Stream 1: the server resets its send side.
		st, err := sess.OpenStreamSync(context.Background())
		if err != nil {
			return
		}
		_, _ = st.Write([]byte("partial"))
		time.Sleep(200 * time.Millisecond)
		st.CancelWrite(5)
		// Stream 2: the client asks the server to stop sending.
		st2, err := sess.AcceptStream(context.Background())
		if err != nil {
			return
		}
		for {
			if _, err := st2.Write(make([]byte, 16<<10)); err != nil {
				serverSawStop <- err
				return
			}
		}
	})
	s := dialWT(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), wtTimeout)
	defer cancel()
	st, err := s.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	got, err := io.ReadAll(st)
	var stErr *client.StreamError
	if !errors.As(err, &stErr) || stErr.Code != 5 || !stErr.Read || !stErr.Remote {
		t.Fatalf("read after reset = %q, %v; want remote read-side stream error 5", got, err)
	}

	st2, err := s.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := st2.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(st2, make([]byte, 1024)); err != nil {
		t.Fatalf("read before cancel: %v", err)
	}
	st2.(interface{ CancelRead(uint32) }).CancelRead(3)
	select {
	case err := <-serverSawStop:
		var se *webtransport.StreamError
		if !errors.As(err, &se) || se.ErrorCode != 3 {
			t.Fatalf("server write error = %v; want stream error 3", err)
		}
	case <-time.After(wtTimeout):
		t.Fatal("server never saw STOP_SENDING")
	}
	if _, err := st2.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read after CancelRead succeeded")
	}
}

func TestWebTransport_ConcurrentStreams(t *testing.T) {
	url := startWTServer(t, echoStreams)
	s := dialWT(t, url)

	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			st, err := s.OpenStream(ctx)
			if err != nil {
				errs <- fmt.Errorf("stream %d open: %w", i, err)
				return
			}
			payload := pseudoRandomBytes(256<<10+i, int64(i))
			go func() {
				_, _ = st.Write(payload)
				_ = st.Close()
			}()
			got, err := io.ReadAll(st)
			if err != nil || !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("stream %d: %d bytes back, err %v", i, len(got), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestWebTransport_DialErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.DialWebTransport(ctx, "ftp://127.0.0.1:443/wt", client.Options{}); err == nil {
		t.Fatal("dial with an ftp scheme succeeded")
	}
	if _, err := client.DialWebTransport(ctx, "http://127.0.0.1:443/wt", client.Options{}); err == nil {
		t.Fatal("dial with an http scheme succeeded")
	}

	// A UDP port nobody answers on: the dial ends with the caller's deadline.
	silent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	url := "https://127.0.0.1:" + strconv.Itoa(silent.LocalAddr().(*net.UDPAddr).Port) + "/wt"
	short, cancelShort := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancelShort()
	start := time.Now()
	if _, err := client.DialWebTransport(short, url, client.Options{InsecureSkipVerify: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial to a silent port = %v; want the context deadline", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("dial returned after %v; the deadline was 1.5s", d)
	}
}

func TestWebTransport_UntrustedCertificate(t *testing.T) {
	url := startWTServer(t, func(*webtransport.Session) {})
	ctx, cancel := context.WithTimeout(context.Background(), wtTimeout)
	defer cancel()
	_, err := client.DialWebTransport(ctx, url, client.Options{}) // no InsecureSkipVerify
	if err == nil {
		t.Fatal("dial with a self-signed certificate succeeded")
	}
}
