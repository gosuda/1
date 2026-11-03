package grt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"weak"

	"gosuda.org/grt/internal/engine"
)

func testPair(t *testing.T, datagrams bool) (*Listener, *Config) {
	t.Helper()
	return testPairOver(t, datagrams, func(s *UDPSocket) PacketSocket { return s })
}

// testPairOver binds a loopback listener whose socket is wrapped by wrap
// and returns it with a matching client config. The socket is closed in
// test cleanup.
func testPairOver(t *testing.T, datagrams bool, wrap func(*UDPSocket) PacketSocket) (*Listener, *Config) {
	t.Helper()
	// The service identity binds the authority string (host:port), so
	// bind the socket first, then mint the identity for the real port.
	sock, err := ListenUDP(&net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sock.Close() })
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
	l := ListenOver(wrap(sock), scfg)
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
	if cap(dg) > 64 {
		t.Fatalf("ReceiveDatagram buffer cap = %d, want right-sized copy (<= 64)", cap(dg))
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

// scriptedSocket is a PacketSocket whose Recv results come from recv,
// called with the 1-based call number.
type scriptedSocket struct {
	recv  func(call int, buf []byte) (int, error)
	calls int
}

func (s *scriptedSocket) Send([]byte, netip.AddrPort, uint8) error { return nil }

func (s *scriptedSocket) LocalAddr() netip.AddrPort { return netip.AddrPort{} }

func (s *scriptedSocket) Recv(buf []byte) (int, netip.AddrPort, netip.AddrPort, uint8, error) {
	s.calls++
	n, err := s.recv(s.calls, buf)
	return n, netip.AddrPort{}, netip.AddrPort{}, 0, err
}

var errClosedSocket = &net.OpError{Op: "read", Net: "udp", Err: net.ErrClosed}

func TestReadPumpDeliversAcrossTransientErrors(t *testing.T) {
	script := []struct {
		data string
		err  error
	}{
		{err: syscall.ECONNREFUSED},
		{data: "a"},
		{err: syscall.ENOBUFS},
		{err: syscall.ENOBUFS},
		{err: os.ErrDeadlineExceeded},
		{data: "b"},
	}
	ps := &scriptedSocket{recv: func(call int, buf []byte) (int, error) {
		if call > len(script) {
			return 0, errClosedSocket
		}
		step := script[call-1]
		return copy(buf, step.data), step.err
	}}
	var got []string
	err := readPump(ps, nil, func(pkt []byte, _, _ netip.AddrPort, _ uint8) {
		got = append(got, string(pkt))
		engine.PutPacketBuf(pkt)
	})
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("readPump returned %v, want net.ErrClosed", err)
	}
	if want := []string{"a", "b"}; !slices.Equal(got, want) {
		t.Fatalf("delivered %q, want %q", got, want)
	}
	if ps.calls != len(script)+1 {
		t.Fatalf("Recv calls = %d, want %d", ps.calls, len(script)+1)
	}
}

func TestReadPumpStopsDuringErrorBackoff(t *testing.T) {
	const stopAfter = 5
	stop := make(chan struct{})
	ps := &scriptedSocket{recv: func(call int, _ []byte) (int, error) {
		if call == stopAfter {
			close(stop)
		}
		return 0, os.ErrDeadlineExceeded
	}}
	err := readPump(ps, stop, func([]byte, netip.AddrPort, netip.AddrPort, uint8) {
		t.Error("deliver called for a failed Recv")
	})
	if err != nil {
		t.Fatalf("readPump returned %v, want nil after stop", err)
	}
	if ps.calls != stopAfter {
		t.Fatalf("Recv calls = %d, want %d (no Recv after stop)", ps.calls, stopAfter)
	}
}

// flakySocket fails its first failures Recv calls with a transient error
// before forwarding to the underlying UDPSocket.
type flakySocket struct {
	*UDPSocket
	failures int32
	calls    atomic.Int32
}

func (f *flakySocket) Recv(buf []byte) (int, netip.AddrPort, netip.AddrPort, uint8, error) {
	if n := f.calls.Add(1); n <= f.failures {
		return 0, netip.AddrPort{}, netip.AddrPort{}, 0, syscall.ECONNRESET
	}
	return f.UDPSocket.Recv(buf)
}

func TestListenerSurvivesTransientRecvErrors(t *testing.T) {
	var flaky *flakySocket
	l, ccfg := testPairOver(t, false, func(s *UDPSocket) PacketSocket {
		flaky = &flakySocket{UDPSocket: s, failures: 6}
		return flaky
	})
	defer l.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	cli, err := Dial(ctx, l.Addr().String(), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := cli.WaitConfirmed(ctx); err != nil {
		t.Fatalf("client confirm through flaky listener socket: %v", err)
	}
	srv, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("accept after transient Recv errors: %v", err)
	}
	defer srv.Close()
	if got := flaky.calls.Load(); got <= flaky.failures {
		t.Fatalf("listener Recv calls = %d; handshake finished before all %d injected errors", got, flaky.failures)
	}
}

func TestDialOverSurvivesTransientRecvErrors(t *testing.T) {
	l, ccfg := testPair(t, false)
	defer l.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	sock, err := DialUDPSocket(l.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sock.Close() })
	flaky := &flakySocket{UDPSocket: sock, failures: 4}
	cli, err := DialOver(flaky, l.Addr(), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := cli.WaitConfirmed(ctx); err != nil {
		t.Fatalf("client confirm through flaky socket: %v", err)
	}
	if got := flaky.calls.Load(); got <= flaky.failures {
		t.Fatalf("client Recv calls = %d; handshake finished before all %d injected errors", got, flaky.failures)
	}
}

func TestStreamEndReleasesDeadlineTimers(t *testing.T) {
	l, ccfg := testPair(t, false)
	defer l.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	cli, err := Dial(ctx, l.Addr().String(), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := cli.WaitConfirmed(ctx); err != nil {
		t.Fatalf("client confirm: %v", err)
	}
	s, err := cli.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRead(1); err != nil {
		t.Fatalf("CancelRead: %v", err)
	}
	if err := s.CancelWrite(1); err != nil {
		t.Fatalf("CancelWrite: %v", err)
	}

	// A pending deadline timer references its Stream; once both halves
	// end, nothing else does and the Stream must become collectable.
	wp := weak.Make(s)
	s = nil
	for range 10 {
		runtime.GC()
		if wp.Value() == nil {
			return
		}
	}
	t.Fatal("Stream still reachable after CancelRead and CancelWrite: a deadline timer retains it")
}
