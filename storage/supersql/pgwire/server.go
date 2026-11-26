package pgwire

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/supersql-alpha"
)

const (
	defaultMaxConnections     = 128
	defaultApplicationVersion = "18.4 (SuperSQL)"
	defaultReadTimeout        = 30 * time.Second
	defaultIdleTimeout        = 5 * time.Minute
	defaultMaxPreparedStmts   = 1024
	defaultMaxPortals         = 256
	defaultNotifyWriteTimeout = 2 * time.Second
	defaultNotifyQueueSize    = 64
)

// Notification represents an asynchronous PostgreSQL LISTEN/NOTIFY payload
// delivered over the wire as a NotificationResponse ('A') frame.
type Notification struct {
	PID     int32
	Channel string
	Payload string
}

// NotifySender delivers asynchronous NotificationResponse ('A') frames to a
// connected client. It is safe for concurrent use from any goroutine.
type NotifySender func(Notification) error

// Config configures a PostgreSQL v3.0 wire protocol Server.
type Config struct {
	// Connector supplies isolated SuperSQL sessions per client connection.
	// If passed to NewServer directly, that argument takes precedence.
	Connector *supersql.Connector

	// Password enables SCRAM-SHA-256 authentication when non-empty and both
	// PasswordLookup and AuthFunc are nil. It may be either a plaintext
	// password or a PostgreSQL SCRAM-SHA-256 verifier string. When Password,
	// PasswordLookup, and AuthFunc are all empty/nil, trust authentication
	// (AuthenticationOk) is used.
	Password string

	// PasswordLookup looks up the expected credential for (user, database)
	// during SCRAM-SHA-256 authentication. It may return either a plaintext
	// password or a PostgreSQL SCRAM-SHA-256 verifier string in the format
	// "SCRAM-SHA-256$<iterations>:<b64salt>$<b64storedKey>:<b64serverKey>".
	// When non-nil, PasswordLookup takes precedence over AuthFunc and Password.
	PasswordLookup func(ctx context.Context, user, database string) (string, error)

	// AuthFunc optionally overrides password verification with custom cleartext
	// password logic when PasswordLookup is nil. Returning a non-nil error
	// rejects the client with SQLSTATE 28P01 (or the error's SQLSTATE if it
	// carries one).
	AuthFunc func(ctx context.Context, user, database, password string) error

	// TLSConfig enables SSLRequest ('S') upgrades via tls.Server and requires
	// TLS for StartupMessage connections when non-nil. When nil (and RequireTLS
	// is false), SSLRequest is declined with 'N'.
	TLSConfig *tls.Config

	// RequireTLS rejects clients that do not upgrade to TLS via SSLRequest.
	// NewServer returns an error if RequireTLS is true and TLSConfig is nil.
	RequireTLS bool

	// MaxConnections caps concurrent active authenticated client sessions.
	// Defaults to 128.
	MaxConnections int

	// MaxPreparedStatements caps the number of prepared statements cached per
	// connection. Defaults to 1024 when zero or negative.
	MaxPreparedStatements int

	// MaxPortals caps the number of open portals per connection. Defaults to
	// 256 when zero or negative.
	MaxPortals int

	// ReadTimeout bounds individual wire frame reads, startup negotiation, and
	// TLS handshakes. Defaults to 30s when zero; set a negative duration to
	// disable read timeouts.
	ReadTimeout time.Duration

	// IdleTimeout bounds how long an idle connection may wait for the next
	// frontend message before being closed. Defaults to 5m when zero; set a
	// negative duration to disable idle timeouts.
	IdleTimeout time.Duration

	// ApplicationVersion is reported in the server_version ParameterStatus.
	// Defaults to "18.4 (SuperSQL)".
	ApplicationVersion string

	// Logger receives optional diagnostic log lines.
	Logger func(format string, args ...any)

	// NotifyHandler is invoked once per authenticated connection with the
	// connection's context, backend PID, and a thread-safe NotifySender that
	// pushes NotificationResponse ('A') frames over the wire. The optional
	// returned cleanup function is called when the connection closes.
	NotifyHandler func(ctx context.Context, pid int32, send NotifySender) (cleanup func())
}

// Server accepts PostgreSQL v3.0 wire protocol connections and routes each
// client to its own isolated SuperSQL session.
type Server struct {
	connector        *supersql.Connector
	cfg              Config
	maxConns         int
	maxPreAuth       int
	maxPreparedStmts int
	maxPortals       int
	readTimeout      time.Duration
	idleTimeout      time.Duration
	appVersion       string

	nextPID      atomic.Int32
	activeConns  atomic.Int32
	preAuthCount atomic.Int32

	mu           sync.Mutex
	closed       bool
	listeners    []net.Listener
	addr         net.Addr
	conns        map[int32]*clientConn
	preAuthConns map[*clientConn]struct{}

	wg sync.WaitGroup
}

// NewServer constructs a PostgreSQL v3.0 wire protocol Server over connector.
// The server borrows connector and never closes it when Server.Close is called.
func NewServer(connector *supersql.Connector, cfg Config) (*Server, error) {
	if connector == nil {
		connector = cfg.Connector
	}
	if connector == nil {
		return nil, errors.New("pgwire: connector is required")
	}
	if cfg.RequireTLS && cfg.TLSConfig == nil {
		return nil, errors.New("pgwire: RequireTLS requires non-nil TLSConfig")
	}
	maxConns := cfg.MaxConnections
	if maxConns <= 0 {
		maxConns = defaultMaxConnections
	}
	maxPreparedStmts := cfg.MaxPreparedStatements
	if maxPreparedStmts <= 0 {
		maxPreparedStmts = defaultMaxPreparedStmts
	}
	maxPortals := cfg.MaxPortals
	if maxPortals <= 0 {
		maxPortals = defaultMaxPortals
	}
	readTimeout := cfg.ReadTimeout
	if readTimeout == 0 {
		readTimeout = defaultReadTimeout
	} else if readTimeout < 0 {
		readTimeout = 0
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = defaultIdleTimeout
	} else if idleTimeout < 0 {
		idleTimeout = 0
	}
	appVersion := cfg.ApplicationVersion
	if appVersion == "" {
		appVersion = defaultApplicationVersion
	}
	maxPreAuth := max(maxConns*2, 64)
	s := &Server{
		connector:        connector,
		cfg:              cfg,
		maxConns:         maxConns,
		maxPreAuth:       maxPreAuth,
		maxPreparedStmts: maxPreparedStmts,
		maxPortals:       maxPortals,
		readTimeout:      readTimeout,
		idleTimeout:      idleTimeout,
		appVersion:       appVersion,
		conns:            make(map[int32]*clientConn),
		preAuthConns:     make(map[*clientConn]struct{}),
	}
	s.nextPID.Store(1000)
	return s, nil
}

// NewServerFromCatalog is a convenience constructor wrapping an already opened
// *supersql.Catalog. Closing the returned Server does not close db.
func NewServerFromCatalog(db *supersql.Catalog, cfg Config) (*Server, error) {
	if db == nil {
		return nil, errors.New("pgwire: catalog is required")
	}
	return NewServer(supersql.NewConnectorWithCatalog(db), cfg)
}

// ListenAndServe listens on TCP address addr (e.g. "127.0.0.1:5432" or
// "127.0.0.1:0") and serves PostgreSQL v3.0 wire protocol clients until ctx is
// canceled or Close is called.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve accepts incoming connections on ln and serves them until ctx is
// canceled, Close is called, or ln returns a fatal error.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		return errors.New("pgwire: listener is required")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return net.ErrClosed
	}
	s.listeners = append(s.listeners, ln)
	if s.addr == nil {
		s.addr = ln.Addr()
	}
	s.mu.Unlock()

	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()

	// Unblock Accept promptly when ctx is canceled.
	stopWatch := context.AfterFunc(serveCtx, func() {
		_ = ln.Close()
	})
	defer stopWatch()

	for {
		rawConn, err := ln.Accept()
		if err != nil {
			if serveCtx.Err() != nil || s.isClosed() || errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			s.wg.Wait()
			return err
		}

		if !s.acquirePreAuthSlot() {
			_ = rawConn.Close()
			continue
		}
		cc := s.newClientConn(rawConn)
		if !s.registerPreAuth(cc) {
			s.releasePreAuthSlot()
			_ = rawConn.Close()
			s.wg.Wait()
			return nil
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					s.logf("pgwire: recovered panic in connection goroutine pid=%d: %v\n%s", cc.pid, r, debug.Stack())
					cc.safeSendFatalPanic()
					cc.forceClose()
				}
			}()
			cc.serve(serveCtx)
		}()
	}
}

// Addr returns the listener's network address once Serve or ListenAndServe has
// bound a listener, or nil before serving starts.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// ActiveConnections reports the number of currently active authenticated client
// sessions.
func (s *Server) ActiveConnections() int {
	return int(s.activeConns.Load())
}

// Notify delivers an asynchronous NotificationResponse ('A') frame to the
// client connection identified by pid.
func (s *Server) Notify(pid int32, channel, payload string) error {
	s.mu.Lock()
	cc := s.conns[pid]
	s.mu.Unlock()
	if cc == nil {
		return fmt.Errorf("pgwire: backend pid %d not found", pid)
	}
	return cc.sendNotification(Notification{
		PID:     pid,
		Channel: channel,
		Payload: payload,
	})
}

// BroadcastNotify enqueues an asynchronous NotificationResponse ('A') frame to
// every currently connected client without blocking on slow or stalled peers.
func (s *Server) BroadcastNotify(senderPID int32, channel, payload string) {
	s.mu.Lock()
	targets := make([]*clientConn, 0, len(s.conns))
	for _, cc := range s.conns {
		targets = append(targets, cc)
	}
	s.mu.Unlock()
	n := Notification{PID: senderPID, Channel: channel, Payload: payload}
	for _, cc := range targets {
		if cc.broadcastCh == nil {
			continue
		}
		select {
		case cc.broadcastCh <- n:
		default:
		}
	}
}

// Close stops accepting new connections, closes all active client connections,
// and waits for all connection goroutines to finish. It does not close the
// underlying *supersql.Connector.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	listeners := append([]net.Listener(nil), s.listeners...)
	all := make([]*clientConn, 0, len(s.conns)+len(s.preAuthConns))
	for _, cc := range s.conns {
		all = append(all, cc)
	}
	for cc := range s.preAuthConns {
		all = append(all, cc)
	}
	s.mu.Unlock()

	var closeErr error
	for _, ln := range listeners {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = errors.Join(closeErr, err)
		}
	}
	for _, cc := range all {
		cc.cancelActiveQuery()
		cc.forceClose()
	}
	s.wg.Wait()
	return closeErr
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) newClientConn(rawConn net.Conn) *clientConn {
	pid := s.nextPID.Add(1)
	secret := randomSecretKey()
	return &clientConn{
		server:      s,
		rawConn:     rawConn,
		br:          bufio.NewReaderSize(rawConn, 16*1024),
		bw:          bufio.NewWriterSize(rawConn, 16*1024),
		pid:         pid,
		secretKey:   secret,
		isSuperuser: false,
		txStatus:    txStatusIdle,
		stmts:       make(map[string]*preparedStmt),
		portals:     make(map[string]*portal),
		broadcastCh: make(chan Notification, defaultNotifyQueueSize),
	}
}

func (s *Server) acquireSlot() bool {
	for {
		cur := s.activeConns.Load()
		if int(cur) >= s.maxConns {
			return false
		}
		if s.activeConns.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (s *Server) releaseSlot() {
	s.activeConns.Add(-1)
}

func (s *Server) acquirePreAuthSlot() bool {
	for {
		cur := s.preAuthCount.Load()
		if int(cur) >= s.maxPreAuth {
			return false
		}
		if s.preAuthCount.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (s *Server) releasePreAuthSlot() {
	s.preAuthCount.Add(-1)
}

func (s *Server) registerPreAuth(cc *clientConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.preAuthConns[cc] = struct{}{}
	return true
}

func (s *Server) unregisterPreAuth(cc *clientConn) {
	s.mu.Lock()
	delete(s.preAuthConns, cc)
	s.mu.Unlock()
}

func (s *Server) registerConn(cc *clientConn) {
	s.mu.Lock()
	delete(s.preAuthConns, cc)
	s.conns[cc.pid] = cc
	s.mu.Unlock()
}

func (s *Server) unregisterConn(pid int32) {
	s.mu.Lock()
	delete(s.conns, pid)
	s.mu.Unlock()
}

func (s *Server) cancelQuery(pid, secret int32) {
	s.mu.Lock()
	cc := s.conns[pid]
	s.mu.Unlock()
	if cc == nil {
		return
	}
	var want, got [4]byte
	binary.BigEndian.PutUint32(want[:], uint32(cc.secretKey))
	binary.BigEndian.PutUint32(got[:], uint32(secret))
	if subtle.ConstantTimeCompare(want[:], got[:]) == 1 {
		cc.cancelActiveQuery()
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Logger != nil {
		s.cfg.Logger(format, args...)
	}
}

func randomSecretKey() int32 {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return int32(time.Now().UnixNano())
	}
	v := int32(binary.BigEndian.Uint32(buf[:]))
	if v == 0 {
		return 1
	}
	return v
}
