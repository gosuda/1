package grt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gosuda.org/grt/internal/engine"
)

// Conn is one GRT connection. All methods are safe for concurrent use;
// the owning goroutine inside the engine serializes state commits.
//
// A Conn made by Dial owns its socket and releases it on death; a
// Conn made by DialOver or Accept does not own the socket.
type Conn struct {
	c *engine.Conn
}

// Dial starts a client connection to remote over UDP. remote may be a
// "host:port" name or a numeric address. The handshake proceeds
// asynchronously: the returned Conn is usable immediately, and
// WaitSecureReady/WaitConfirmed report establishment progress
// (SECURE_READY means the server is authenticated; CONFIRMED means the
// peer received our FINISH — spec section 31.1). Terminal failures
// surface through TermError and every pending operation.
func Dial(ctx context.Context, remote string, cfg *Config) (*Conn, error) {
	if cfg == nil || cfg.ServerName == "" {
		return nil, errNeedServerName
	}
	raddr, err := resolveAddr(ctx, remote)
	if err != nil {
		return nil, err
	}
	sock, err := DialUDPSocket(raddr)
	if err != nil {
		return nil, err
	}
	ec := engine.Dial(cfg.engineConfig(), sock, raddr, cfg.Identity)
	go pumpDatagrams(sock, ec)
	// The socket is owned: release it when the conn dies regardless of
	// who initiated termination.
	go func() {
		<-ec.Done()
		_ = sock.Close()
	}()
	return &Conn{c: ec}, nil
}

// DialOver starts a client connection on a caller-supplied socket,
// which must remain open for the connection's life and is NOT closed
// by Conn.Close.
func DialOver(ps engine.PacketSocket, remote netip.AddrPort, cfg *Config) (*Conn, error) {
	if cfg == nil || cfg.ServerName == "" {
		return nil, errNeedServerName
	}
	ec := engine.Dial(cfg.engineConfig(), ps, remote, cfg.Identity)
	go pumpDatagrams(ps, ec)
	// The caller owns the socket, so the pump can outlive the conn;
	// unblock it on death when the socket supports deadlines.
	go func() {
		<-ec.Done()
		if d, ok := ps.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = d.SetReadDeadline(time.Now())
		}
	}()
	return &Conn{c: ec}, nil
}

// resolveAddr accepts numeric host:port or DNS names.
func resolveAddr(ctx context.Context, remote string) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return ap, nil
	}
	host, portStr, err := net.SplitHostPort(remote)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("grt: resolve %q: %w", remote, err)
	}
	var res net.Resolver
	ips, err := res.LookupIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("grt: resolve %q: %w", remote, err)
	}
	if len(ips) == 0 {
		return netip.AddrPort{}, fmt.Errorf("grt: resolve %q: no addresses", remote)
	}
	port, err := net.LookupPort("udp", portStr)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("grt: resolve %q: %w", remote, err)
	}
	ap, _ := netip.AddrFromSlice(ips[0])
	return netip.AddrPortFrom(ap.Unmap(), uint16(port)), nil
}

// pumpDatagrams forwards socket datagrams into the connection until the
// socket errors (closed) or the connection dies.
func pumpDatagrams(ps engine.PacketSocket, c *engine.Conn) {
	for {
		pktBuf := engine.GetPacketBuf()
		n, src, dst, ecn, err := ps.Recv(pktBuf[:cap(pktBuf)])
		if err != nil {
			engine.PutPacketBuf(pktBuf)
			return
		}
		if n == 0 {
			engine.PutPacketBuf(pktBuf)
			continue
		}
		c.OnDatagram(pktBuf[:n], src, dst, ecn)
	}
}

// --- Streams ---

// OpenStream opens a bidirectional stream (spec OPEN). It returns
// ErrWouldBlock when no peer-granted slot is free; OpenStreamSync waits
// for a slot instead.
func (c *Conn) OpenStream() (*Stream, error) {
	sid, err := c.c.OpenStream()
	if err != nil {
		return nil, err
	}
	return &Stream{c: c.c, id: sid}, nil
}

// OpenStreamSync opens a bidirectional stream, waiting for a slot or
// the context.
func (c *Conn) OpenStreamSync(ctx context.Context) (*Stream, error) {
	for {
		s, err := c.OpenStream()
		if err != engine.ErrWouldBlock {
			return s, err
		}
		c.c.WaitWritable(ctx)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-c.c.Done():
			return nil, c.c.TermError()
		default:
		}
	}
}

// OpenUni opens a unidirectional stream (send-only to the peer).
func (c *Conn) OpenUni() (*Stream, error) {
	sid, err := c.c.OpenUni()
	if err != nil {
		return nil, err
	}
	return &Stream{c: c.c, id: sid}, nil
}

// AcceptStream returns the next peer-opened bidirectional stream,
// blocking until one arrives, the context expires, or the connection
// dies.
func (c *Conn) AcceptStream(ctx context.Context) (*Stream, error) {
	for {
		if sid, ok := c.c.AcceptStream(); ok {
			return &Stream{c: c.c, id: sid}, nil
		}
		c.c.WaitReadable(ctx)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-c.c.Done():
			return nil, c.c.TermError()
		default:
		}
	}
}

// AcceptUniStream returns the next peer-opened unidirectional stream.
func (c *Conn) AcceptUniStream(ctx context.Context) (*Stream, error) {
	for {
		if sid, ok := c.c.AcceptUni(); ok {
			return &Stream{c: c.c, id: sid}, nil
		}
		c.c.WaitReadable(ctx)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-c.c.Done():
			return nil, c.c.TermError()
		default:
		}
	}
}

// --- Datagrams ---

// SendDatagram queues one unreliable datagram (spec SEND_DATAGRAM).
// Returns ErrWouldBlock when the outbound queue is full and an error
// when datagrams were not negotiated or the payload exceeds the peer's
// limit.
func (c *Conn) SendDatagram(b []byte) error { return c.c.SendDatagram(b) }

// ReceiveDatagram waits for the next inbound datagram.
func (c *Conn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	buf := make([]byte, 64*1024)
	for {
		n, err := c.c.ReadDatagram(buf)
		if err != engine.ErrWouldBlock {
			if err != nil {
				return nil, err
			}
			return buf[:n], nil
		}
		c.c.WaitDatagram(ctx)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-c.c.Done():
			return nil, c.c.TermError()
		default:
		}
	}
}

// --- Lifecycle ---

// Close gracefully closes the connection (spec CLOSE; NO_ERROR). The
// owned socket is released automatically when the conn reaches DEAD.
func (c *Conn) Close() error {
	c.c.Close(0)
	return nil
}

// CloseWithError closes with an application abort code.
func (c *Conn) CloseWithError(code uint32) error {
	c.c.Close(code)
	return nil
}

// Done closes when the connection reaches DEAD.
func (c *Conn) Done() <-chan struct{} { return c.c.Done() }

// TermError returns the terminal error after Done closes. It is a
// *TransportError (Remote distinguishes peer closes) — a clean local
// Close surfaces as NO_ERROR.
func (c *Conn) TermError() error { return c.c.TermError() }

// State returns the current lifecycle snapshot.
func (c *Conn) State() State { return c.c.State() }

// RetrySeen reports whether the handshake consumed a stateless RETRY.
func (c *Conn) RetrySeen() bool { return c.c.RetrySeen() }

// WaitSecureReady blocks until the peer is authenticated (client) —
// the SECURE_READY point of spec section 31.1 — or the connection dies.
// A nil error means the peer is authenticated; use WaitConfirmed before
// side effects that require the peer to have received our FINISH.
func (c *Conn) WaitSecureReady(ctx context.Context) error {
	c.c.WaitSecureReady(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.c.Done():
		return c.c.TermError()
	default:
		return nil
	}
}

// WaitConfirmed blocks until the handshake is CONFIRMED or the
// connection dies.
func (c *Conn) WaitConfirmed(ctx context.Context) error {
	c.c.WaitConfirmed(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.c.Done():
		return c.c.TermError()
	default:
		return nil
	}
}

// Cipher returns the negotiated cipher ID (e.g. CipherXChaCha20Poly1305 or CipherAES256GCM).
func (c *Conn) Cipher() uint16 {
	return c.c.Cipher()
}

// --- Mobile (optional commands, spec section 35) ---

// Migrate initiates elective migration to a new remote endpoint.
func (c *Conn) Migrate(remote netip.AddrPort) error { return c.c.Migrate(remote) }

// RequestPark requests a mobile parking lease (client only).
func (c *Conn) RequestPark(d time.Duration) error {
	return c.c.Park(uint32(d / time.Millisecond))
}

// Wake sends PARK_WAKE for a parked session.
func (c *Conn) Wake() error { return c.c.Wake() }

// NetworkEvent reports a local interface/network change; it authorizes
// revalidating the current remote on this socket's local tuple.
func (c *Conn) NetworkEvent() error { return c.c.NetworkEvent() }

// LocalAddr returns the connection's local endpoint.
func (c *Conn) LocalAddr() netip.AddrPort { return c.c.LocalAddr() }

// Ticket returns the resumption ticket stored from the peer's
// NEW_TICKET frame, or nil when none was issued yet. Persist it and
// pass it as Config.Ticket on a later Dial to offer resumption.
func (c *Conn) Ticket() *ClientTicket { return c.c.ResumeTicket() }

// AddressToken returns the newest REUSE token from a NEW_TOKEN frame.
// Persist it and pass it as Config.AddressToken on a later Dial to the
// same service; it may skip the stateless Retry round trip.
func (c *Conn) AddressToken() ([64]byte, bool) { return c.c.StoredToken() }

// EarlyDataStatus reports the server's verdict on Config.EarlyData:
// 0 absent, 1 accepted, 2 rejected. Rejected early bytes are re-sent
// through the ordinary path automatically.
func (c *Conn) EarlyDataStatus() uint8 { return c.c.EarlyDataStatus() }

// Exporter derives a 32-byte application exporter from the connection's
// exporter master (spec section 11.3). Both endpoints derive identical
// values for the same label and context. It fails before the handshake
// completes and after the connection dies.
func (c *Conn) Exporter(label, ctx string) ([32]byte, error) {
	return c.c.Exporter(label, ctx)
}

// Stream is one reliable ordered byte stream (spec section 12). Read
// returns received bytes; Write appends to the send buffer; Close sends
// FIN. CancelRead/CancelWrite map to STOP_RECV/RESET.
type Stream struct {
	c             *engine.Conn
	id            uint64
	hasDeadline   atomic.Bool
	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	readCancel    chan struct{}
	writeCancel   chan struct{}
	readTimer     *time.Timer
	writeTimer    *time.Timer
}

// ID returns the wire stream identifier (generation-tagged handle).
func (s *Stream) ID() uint64 { return s.id }

// SetDeadline sets both the read and write deadlines associated with the
// stream. A zero value for t means I/O operations will not time out.
func (s *Stream) SetDeadline(t time.Time) error {
	_ = s.SetReadDeadline(t)
	return s.SetWriteDeadline(t)
}

// SetReadDeadline sets the deadline for current and future Read calls.
// Even if Read times out, the stream state remains intact and future
// reads may continue if the deadline is extended.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.hasDeadline.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readDeadline = t
	if s.readTimer != nil {
		s.readTimer.Stop()
		s.readTimer = nil
	}
	if s.readCancel != nil {
		close(s.readCancel)
		s.readCancel = nil
	}
	if !t.IsZero() {
		ch := make(chan struct{})
		s.readCancel = ch
		d := time.Until(t)
		if d <= 0 {
			close(ch)
			s.readCancel = nil
			return nil
		}
		s.readTimer = time.AfterFunc(d, func() {
			s.mu.Lock()
			if s.readCancel == ch {
				close(ch)
				s.readCancel = nil
			}
			s.mu.Unlock()
		})
	}
	return nil
}

// SetWriteDeadline sets the deadline for current and future Write calls.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.hasDeadline.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeDeadline = t
	if s.writeTimer != nil {
		s.writeTimer.Stop()
		s.writeTimer = nil
	}
	if s.writeCancel != nil {
		close(s.writeCancel)
		s.writeCancel = nil
	}
	if !t.IsZero() {
		ch := make(chan struct{})
		s.writeCancel = ch
		d := time.Until(t)
		if d <= 0 {
			close(ch)
			s.writeCancel = nil
			return nil
		}
		s.writeTimer = time.AfterFunc(d, func() {
			s.mu.Lock()
			if s.writeCancel == ch {
				close(ch)
				s.writeCancel = nil
			}
			s.mu.Unlock()
		})
	}
	return nil
}

func (s *Stream) readExpired() bool {
	if !s.hasDeadline.Load() {
		return false
	}
	s.mu.Lock()
	dl := s.readDeadline
	s.mu.Unlock()
	return !dl.IsZero() && !time.Now().Before(dl)
}

func (s *Stream) prepareReadWait() (expired bool, ch <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.readDeadline.IsZero() && !time.Now().Before(s.readDeadline) {
		return true, nil
	}
	if s.readCancel == nil {
		s.readCancel = make(chan struct{})
	}
	return false, s.readCancel
}

func (s *Stream) writeExpired() bool {
	if !s.hasDeadline.Load() {
		return false
	}
	s.mu.Lock()
	dl := s.writeDeadline
	s.mu.Unlock()
	return !dl.IsZero() && !time.Now().Before(dl)
}

func (s *Stream) prepareWriteWait() (expired bool, ch <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writeDeadline.IsZero() && !time.Now().Before(s.writeDeadline) {
		return true, nil
	}
	if s.writeCancel == nil {
		s.writeCancel = make(chan struct{})
	}
	return false, s.writeCancel
}

// Read consumes ordered stream bytes, blocking until data, FIN (io.EOF),
// RESET_STREAM (ErrStreamReset), read deadline expiry (os.ErrDeadlineExceeded),
// or connection death.
func (s *Stream) Read(b []byte) (int, error) {
	return s.ReadContext(context.Background(), b)
}

// ReadContext consumes ordered stream bytes, honoring both ctx and any
// deadline configured via SetReadDeadline / SetDeadline.
func (s *Stream) ReadContext(ctx context.Context, b []byte) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if s.readExpired() {
			return 0, os.ErrDeadlineExceeded
		}
		n, err := s.c.Read(s.id, b)
		if n > 0 || err != nil {
			if errors.Is(err, engine.ErrStreamEOF) {
				err = io.EOF
			}
			return n, err
		}
		expired, waitCh := s.prepareReadWait()
		if expired {
			return 0, os.ErrDeadlineExceeded
		}
		s.c.WaitReadableOr(ctx, waitCh)
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if s.readExpired() {
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-s.c.Done():
			return 0, s.c.TermError()
		default:
		}
	}
}

// Write appends bytes to the send buffer, blocking while the bounded
// buffer is full (spec WRITE: acceptance copies the bytes; it is not
// network delivery).
func (s *Stream) Write(b []byte) (int, error) {
	return s.WriteContext(context.Background(), b)
}

// WriteContext appends bytes to the send buffer, honoring both ctx and
// any deadline configured via SetWriteDeadline / SetDeadline.
func (s *Stream) WriteContext(ctx context.Context, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if s.writeExpired() {
			return total, os.ErrDeadlineExceeded
		}
		n, err := s.c.Write(s.id, b[total:])
		total += n
		if err != engine.ErrWouldBlock {
			return total, err
		}
		expired, waitCh := s.prepareWriteWait()
		if expired {
			return total, os.ErrDeadlineExceeded
		}
		s.c.WaitWritableOr(ctx, waitCh)
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if s.writeExpired() {
			return total, os.ErrDeadlineExceeded
		}
		select {
		case <-s.c.Done():
			return total, s.c.TermError()
		default:
		}
	}
	return total, nil
}

// TryWrite appends without blocking; ErrWouldBlock means no capacity.
func (s *Stream) TryWrite(b []byte) (int, error) { return s.c.Write(s.id, b) }

// Close sends FIN, freezing the send half's final size (spec FIN).
// The receive half stays open.
func (s *Stream) Close() error { return s.c.Fin(s.id) }

// CancelWrite abandons the send half with an application code (RESET).
func (s *Stream) CancelWrite(code uint32) error { return s.c.Reset(s.id, code) }

// CancelRead asks the peer to stop sending (STOP_RECV).
func (s *Stream) CancelRead(code uint32) error { return s.c.Stop(s.id, code) }
