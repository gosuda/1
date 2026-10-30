package grt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func testPair(t *testing.T, datagrams bool) (*Listener, *Config) {
	t.Helper()
	// The service identity binds the authority string (host:port), so
	// bind the socket first, then mint the identity for the real port.
	sock, err := ListenUDP(&net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	authority := sock.LocalAddr().String()
	id, err := GenerateServerIdentity(authority, [16]byte{7})
	if err != nil {
		t.Fatal(err)
	}
	params := DefaultParams()
	params.InitialUniSlots = 2 // Compact default grants zero uni slots
	if datagrams {
		params.MaxDatagramData = 1200
	}
	scfg := &Config{
		ServerIdentity: id,
		ServerName:     authority,
		AppID:          [16]byte{7},
		Params:         params,
		NowUnix:        func() int64 { return time.Now().Unix() },
		MaxLiveSent:    4096,
	}
	l := ListenOver(sock, scfg)
	ccfg := &Config{
		ServerName:  authority,
		AppID:       [16]byte{7},
		Params:      params,
		ExpectedKey: id.IdentityPub,
		NowUnix:     func() int64 { return time.Now().Unix() },
		MaxLiveSent: 4096,
	}
	return l, ccfg
}

// TestUDPLoopback drives a full client/server exchange over a real UDP
// socket on 127.0.0.1: handshake, bidirectional streams, FIN/EOF.
func TestUDPLoopback(t *testing.T) {
	l, ccfg := testPair(t, false)
	defer l.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cli, err := Dial(ctx, l.Addr().String(), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := cli.WaitConfirmed(ctx); err != nil {
		t.Fatalf("client confirm: %v", err)
	}

	srv, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer srv.Close()

	// Server echo goroutine: accept one stream, read to EOF, reply.
	srvErr := make(chan error, 1)
	go func() {
		s, err := srv.AcceptStream(ctx)
		if err != nil {
			srvErr <- err
			return
		}
		got, err := io.ReadAll(s)
		if err != nil {
			srvErr <- err
			return
		}
		reply, err := srv.OpenStream()
		if err != nil {
			srvErr <- err
			return
		}
		if _, err := reply.Write(append([]byte("echo:"), got...)); err != nil {
			srvErr <- err
			return
		}
		srvErr <- reply.Close()
	}()

	s, err := cli.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello over udp")
	if _, err := s.Write(msg); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	rs, err := cli.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rs)
	if err != nil {
		t.Fatal(err)
	}
	if want := append([]byte("echo:"), msg...); !bytes.Equal(got, want) {
		t.Fatalf("reply %q, want %q", got, want)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server echo: %v", err)
	}
}

// TestUDPUniAndDatagram exercises unidirectional streams and datagrams
// in both directions.
func TestUDPUniAndDatagram(t *testing.T) {
	l, ccfg := testPair(t, true)
	defer l.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cli, err := Dial(ctx, l.Addr().String(), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	srv, err := l.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := cli.WaitConfirmed(ctx); err != nil {
		t.Fatal(err)
	}

	us, err := cli.OpenUni()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := us.Write([]byte("uni-payload")); err != nil {
		t.Fatal(err)
	}
	if err := us.Close(); err != nil {
		t.Fatal(err)
	}
	sus, err := srv.AcceptUniStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(sus)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("uni-payload")) {
		t.Fatalf("uni payload %q", got)
	}

	// Datagrams both ways.
	if err := cli.SendDatagram([]byte("dgram-c2s")); err != nil {
		t.Fatal(err)
	}
	dg, err := srv.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dg, []byte("dgram-c2s")) {
		t.Fatalf("server datagram %q", dg)
	}
	if err := srv.SendDatagram([]byte("dgram-s2c")); err != nil {
		t.Fatal(err)
	}
	dg, err = cli.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dg, []byte("dgram-s2c")) {
		t.Fatalf("client datagram %q", dg)
	}
}

func TestStreamDeadlinesAndListenerStats(t *testing.T) {
	l, ccfg := testPair(t, false)
	defer l.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cli, err := Dial(ctx, l.Addr().String(), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := cli.WaitConfirmed(ctx); err != nil {
		t.Fatalf("client confirm: %v", err)
	}

	srv, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer srv.Close()

	// Verify Listener.Stats() reports the admitted connection and stateless RETRY count.
	st := l.Stats()
	if st.AttemptsAdmitted == 0 || st.ActiveConnections == 0 || st.RetriesSent == 0 {
		t.Fatalf("unexpected listener stats: %+v", st)
	}

	cs, err := cli.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	ss, err := srv.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := ss.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("read = %q, err = %v", buf[:n], err)
	}

	// 1. Expired read deadline returns os.ErrDeadlineExceeded without killing the stream.
	if err := ss.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err = ss.Read(buf)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected os.ErrDeadlineExceeded, got %v", err)
	}

	// 2. Clearing the deadline and sending more data succeeds on the same stream.
	if err := ss.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	n, err = ss.Read(buf)
	if err != nil || string(buf[:n]) != "pong" {
		t.Fatalf("read after deadline reset = %q, err = %v", buf[:n], err)
	}

	// 3. Concurrent SetReadDeadline unblocks an already-waiting Read call immediately.
	readDone := make(chan error, 1)
	go func() {
		_, rerr := ss.Read(buf)
		readDone <- rerr
	}()
	time.Sleep(15 * time.Millisecond)
	_ = ss.SetReadDeadline(time.Now())
	if rerr := <-readDone; !errors.Is(rerr, os.ErrDeadlineExceeded) {
		t.Fatalf("expected concurrent SetReadDeadline to unblock with os.ErrDeadlineExceeded, got %v", rerr)
	}
}
