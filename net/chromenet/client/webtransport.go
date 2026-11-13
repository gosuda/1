package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebTransportSession is a WebTransport session over HTTP/3 made by Chromium's network stack.
// Methods may be called from several goroutines.
//
// The streams that OpenStream, OpenUniStream, and AcceptStream return also implement
// CancelRead(code uint32) and CancelWrite(code uint32). Close ends the send side with a FIN, like
// quic-go's stream Close; reading continues until the peer's FIN.
type WebTransportSession interface {
	// OpenStream opens a bidirectional stream, waiting while the peer's stream limit is reached.
	OpenStream(ctx context.Context) (io.ReadWriteCloser, error)
	// OpenUniStream opens a send-only stream.
	OpenUniStream(ctx context.Context) (io.ReadWriteCloser, error)
	// AcceptStream waits for a stream the peer opened. A unidirectional stream is read-only.
	AcceptStream(ctx context.Context) (io.ReadWriteCloser, error)
	// SendDatagram sends an unreliable datagram of at most MaxDatagramSize bytes.
	SendDatagram(payload []byte) error
	// ReceiveDatagram waits for the next datagram. Datagrams the caller does not keep up with are
	// dropped.
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	// MaxDatagramSize is the largest datagram SendDatagram accepts now.
	MaxDatagramSize() (int, error)
	// CloseWithError closes the session with an application error code and reason.
	CloseWithError(code uint32, reason string) error
}

// SessionError is the close of a WebTransport session by the peer or by an error.
type SessionError struct {
	Code   uint32 // the application error code, or the net::Error of a transport failure
	Reason string
	// Transport is true when Code is a (negative) net::Error rather than an application code.
	Transport bool
}

func (e *SessionError) Error() string {
	if e.Transport {
		if e.Reason != "" {
			return fmt.Sprintf("webtransport: session failed: net::Error %d: %s", int32(e.Code), e.Reason)
		}
		return fmt.Sprintf("webtransport: session failed: net::Error %d", int32(e.Code))
	}
	return fmt.Sprintf("webtransport: session closed (code 0x%x: %s)", e.Code, e.Reason)
}

// StreamError is a stream reset or STOP_SENDING from the peer.
type StreamError struct {
	Code   uint32
	Remote bool // the peer aborted the stream (as opposed to this side)
	Read   bool // the read side was aborted; otherwise the write side
}

func (e *StreamError) Error() string {
	side := "write"
	if e.Read {
		side = "read"
	}
	return fmt.Sprintf("webtransport: stream %s side aborted (code 0x%x)", side, e.Code)
}

// DialWebTransport opens a WebTransport session to an https URL through the Chromium network stack.
func DialWebTransport(ctx context.Context, rawURL string, opts Options) (WebTransportSession, error) {
	return DialWebTransportHeader(ctx, rawURL, nil, opts)
}

// DialWebTransportHeader is DialWebTransport with extra CONNECT request headers. Origin sets the
// origin.
func DialWebTransportHeader(ctx context.Context, rawURL string, header http.Header, opts Options) (WebTransportSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("webtransport: invalid url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("webtransport: unsupported scheme %q (want https)", u.Scheme)
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	head, err := connectHead(rawURL, header)
	if err != nil {
		return nil, err
	}
	main, err := OpenWith(ctx, DefaultOptions())
	if err != nil {
		return nil, err
	}
	s := &engineSession{main: main}
	if err := s.start(ctx, opts, head, kindWebTransport); err != nil {
		s.release()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = s.main.Conn().Cancel() })
	code, raw, id, err := s.main.ChromeWtWait(s.wt, make([]byte, 64<<10))
	stop()
	if err != nil {
		s.release()
		return nil, err
	}
	if code != 0 {
		if ctxErr := ctx.Err(); ctxErr != nil {
			s.release()
			return nil, ctxErr
		}
		ne := s.netError(code)
		if e, ok := ne.(*NetError); ok {
			e.Detail = failureDetail(raw)
		}
		s.release()
		return nil, ne
	}
	events, err := OpenWith(ctx, DefaultOptions())
	if err != nil {
		s.release()
		return nil, err
	}
	// Stream writes and opens can block for flow control, and flow control only moves when the
	// caller's reads are acknowledged: acknowledgements, aborts, datagrams, and close go through a
	// session of their own so a blocked write never holds them up.
	ctl, err := OpenWith(ctx, DefaultOptions())
	if err != nil {
		_ = events.Close()
		s.release()
		return nil, err
	}
	sess := &webTransportSession{
		s:         s,
		events:    events,
		ctl:       ctl,
		id:        id,
		streams:   make(map[uint64]*webTransportStream),
		incoming:  make(chan *webTransportStream, 64),
		datagrams: make(chan []byte, 128),
		done:      make(chan struct{}),
		pumpDone:  make(chan struct{}),
	}
	go sess.pump()
	return sess, nil
}

// failureDetail joins the "name: value" lines after the first line of a failed handshake's head.
func failureDetail(head []byte) string {
	_, rest, _ := strings.Cut(string(head), "\n")
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(rest), "\n", "; ")), " ")
}

type webTransportSession struct {
	s      *engineSession
	events *Client
	ctl    *Client
	ctlMu  sync.Mutex
	id     uint64

	mu        sync.Mutex
	streams   map[uint64]*webTransportStream
	incoming  chan *webTransportStream
	datagrams chan []byte
	err       error // the terminal error, set once
	done      chan struct{}
	pumpDone  chan struct{}
	closeOnce sync.Once
}

// pump delivers the engine's session events to the streams and queues until the session ends.
func (w *webTransportSession) pump() {
	defer close(w.pumpDone)
	buf := make([]byte, maxChunk)
	for {
		data, kind, streamID, fin, code, err := w.events.ChromeWtEvent(w.id, buf)
		if err != nil {
			w.finish(err)
			return
		}
		switch kind {
		case 1, 2:
			st := newWebTransportStream(w, streamID, kind == 1, true)
			w.mu.Lock()
			w.streams[streamID] = st
			w.mu.Unlock()
			select {
			case w.incoming <- st:
			case <-w.done:
				return
			}
		case 3:
			select {
			case w.datagrams <- append([]byte(nil), data...):
			default: // the caller is behind: datagrams are unreliable
			}
		case 4:
			if st := w.stream(streamID); st != nil {
				st.deliver(data, fin)
			}
		case 6, 7:
			if st := w.stream(streamID); st != nil {
				st.abortedByPeer(code, kind == 6)
			}
		case 5:
			w.finish(&SessionError{Code: code, Reason: string(data)})
			return
		case 8:
			w.finish(&SessionError{Code: code, Reason: string(data), Transport: true})
			return
		}
	}
}

func (w *webTransportSession) stream(id uint64) *webTransportStream {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.streams[id]
}

// finish records the terminal error, fails every stream, and wakes every waiter.
func (w *webTransportSession) finish(err error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = err
		close(w.done)
	}
	streams := make([]*webTransportStream, 0, len(w.streams))
	for _, st := range w.streams {
		streams = append(streams, st)
	}
	w.mu.Unlock()
	for _, st := range streams {
		st.sessionEnded(w.sessionErr())
	}
}

func (w *webTransportSession) sessionErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	return errors.New("webtransport: session closed")
}

func (w *webTransportSession) closedErr() error {
	select {
	case <-w.done:
		return w.sessionErr()
	default:
		return nil
	}
}

func (w *webTransportSession) open(ctx context.Context, bidi bool) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := w.closedErr(); err != nil {
		return nil, err
	}
	w.s.mu.Lock()
	code, streamID, err := w.s.main.ChromeWtOpenStream(w.id, bidi)
	var ne error
	if err == nil && code != 0 {
		ne = w.s.netError(code)
	}
	w.s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if ne != nil {
		if sessErr := w.closedErr(); sessErr != nil {
			return nil, sessErr
		}
		return nil, ne
	}
	st := newWebTransportStream(w, streamID, bidi, false)
	w.mu.Lock()
	w.streams[streamID] = st
	w.mu.Unlock()
	return st, nil
}

func (w *webTransportSession) OpenStream(ctx context.Context) (io.ReadWriteCloser, error) {
	return w.open(ctx, true)
}

func (w *webTransportSession) OpenUniStream(ctx context.Context) (io.ReadWriteCloser, error) {
	return w.open(ctx, false)
}

func (w *webTransportSession) AcceptStream(ctx context.Context) (io.ReadWriteCloser, error) {
	select {
	case st := <-w.incoming:
		return st, nil
	default:
	}
	select {
	case st := <-w.incoming:
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.done:
		return nil, w.sessionErr()
	}
}

func (w *webTransportSession) SendDatagram(payload []byte) error {
	if err := w.closedErr(); err != nil {
		return err
	}
	w.ctlMu.Lock()
	defer w.ctlMu.Unlock()
	code, err := w.ctl.ChromeWtSendDatagram(w.id, payload)
	if err != nil {
		return err
	}
	if code != 0 {
		return netErrorOf(w.ctl, code)
	}
	return nil
}

func (w *webTransportSession) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case d := <-w.datagrams:
		return d, nil
	default:
	}
	select {
	case d := <-w.datagrams:
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.done:
		return nil, w.sessionErr()
	}
}

func (w *webTransportSession) MaxDatagramSize() (int, error) {
	if err := w.closedErr(); err != nil {
		return 0, err
	}
	w.ctlMu.Lock()
	defer w.ctlMu.Unlock()
	n, err := w.ctl.ChromeWtMaxDatagram(w.id)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, netErrorOf(w.ctl, int32(n))
	}
	return int(n), nil
}

func (w *webTransportSession) CloseWithError(code uint32, reason string) error {
	var result error
	w.closeOnce.Do(func() {
		if len(reason) > 1024 {
			reason = reason[:1024]
		}
		// Record the local close first, so waiters see this error and not the pump's own.
		w.finish(&SessionError{Code: code, Reason: reason})
		w.ctlMu.Lock()
		_, _ = w.ctl.ChromeWtClose(w.id, code, reason)
		w.ctlMu.Unlock()
		// The stack reports the session closed once the close capsule has been sent.
		select {
		case <-w.pumpDone:
		case <-time.After(3 * time.Second):
		}
		// Destroying the session handle wakes the event pump if it is still waiting.
		w.s.mu.Lock()
		_ = w.s.main.ChromeWtDestroy(w.s.wt)
		w.s.wt = Wt{}
		w.s.mu.Unlock()
		<-w.pumpDone
		_ = w.events.Close()
		w.ctlMu.Lock()
		_ = w.ctl.Close()
		w.ctlMu.Unlock()
		w.s.mu.Lock()
		w.s.release()
		w.s.mu.Unlock()
	})
	return result
}

// webTransportStream is one stream of a session. Received bytes are buffered until Read takes
// them; Read tells the engine how many it took so the engine can read more from the network.
type webTransportStream struct {
	w        *webTransportSession
	id       uint64
	bidi     bool
	incoming bool

	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	fin     bool  // the peer's FIN arrived
	readErr error // sticky: the read side failed
	taken   int   // bytes Read took and the engine does not know about yet

	writeMu    sync.Mutex
	writeErr   error
	finSent    bool
	readClosed bool
}

func newWebTransportStream(w *webTransportSession, id uint64, bidi, incoming bool) *webTransportStream {
	st := &webTransportStream{w: w, id: id, bidi: bidi, incoming: incoming}
	st.cond = sync.NewCond(&st.mu)
	return st
}

func (st *webTransportStream) canRead() bool  { return st.bidi || st.incoming }
func (st *webTransportStream) canWrite() bool { return st.bidi || !st.incoming }

func (st *webTransportStream) deliver(data []byte, fin bool) {
	st.mu.Lock()
	st.buf = append(st.buf, data...)
	if fin {
		st.fin = true
	}
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *webTransportStream) abortedByPeer(code uint32, read bool) {
	err := &StreamError{Code: code, Remote: true, Read: read}
	if read {
		st.mu.Lock()
		st.readErr = err
		st.cond.Broadcast()
		st.mu.Unlock()
		return
	}
	st.writeMu.Lock()
	st.writeErr = err
	st.writeMu.Unlock()
}

func (st *webTransportStream) sessionEnded(err error) {
	st.mu.Lock()
	if st.readErr == nil && !(st.fin && len(st.buf) == 0) {
		st.readErr = err
	}
	st.cond.Broadcast()
	st.mu.Unlock()
	st.writeMu.Lock()
	if st.writeErr == nil {
		st.writeErr = err
	}
	st.writeMu.Unlock()
}

func (st *webTransportStream) Read(p []byte) (int, error) {
	if !st.canRead() {
		return 0, errors.New("webtransport: read on a send-only stream")
	}
	if len(p) == 0 {
		return 0, nil
	}
	st.mu.Lock()
	for len(st.buf) == 0 && !st.fin && st.readErr == nil && !st.readClosed {
		st.cond.Wait()
	}
	if len(st.buf) == 0 || st.readClosed {
		var err error
		switch {
		case st.readErr != nil:
			err = st.readErr
		case st.readClosed:
			err = &StreamError{Code: 0, Read: true}
		default:
			err = io.EOF
		}
		st.mu.Unlock()
		return 0, err
	}
	n := copy(p, st.buf)
	st.buf = st.buf[n:]
	if len(st.buf) == 0 {
		st.buf = nil
	}
	st.taken += n
	flush := st.taken >= 32<<10 || len(st.buf) == 0
	taken := st.taken
	if flush {
		st.taken = 0
	}
	st.mu.Unlock()
	if flush && taken > 0 {
		st.w.ctlMu.Lock()
		_, _ = st.w.ctl.ChromeWtStreamConsume(st.w.id, st.id, uint32(taken))
		st.w.ctlMu.Unlock()
	}
	return n, nil
}

func (st *webTransportStream) Write(p []byte) (int, error) {
	if !st.canWrite() {
		return 0, errors.New("webtransport: write on a receive-only stream")
	}
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	if st.writeErr != nil {
		return 0, st.writeErr
	}
	if st.finSent {
		return 0, errors.New("webtransport: write after close")
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxChunk)
		if err := st.sendLocked(p[:n], false); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (st *webTransportStream) sendLocked(data []byte, fin bool) error {
	st.w.s.mu.Lock()
	code, err := st.w.s.main.ChromeWtStreamWrite(st.w.id, st.id, data, fin)
	var ne error
	if err == nil && code != 0 {
		ne = st.w.s.netError(code)
	}
	st.w.s.mu.Unlock()
	if err != nil {
		st.writeErr = err
		return err
	}
	if ne != nil {
		if sessErr := st.w.closedErr(); sessErr != nil {
			ne = sessErr
		}
		st.writeErr = ne
		return ne
	}
	return nil
}

// Close ends the send side with a FIN.
func (st *webTransportStream) Close() error {
	if !st.canWrite() {
		return nil
	}
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	if st.finSent || st.writeErr != nil {
		return nil
	}
	st.finSent = true
	return st.sendLocked(nil, true)
}

// CancelWrite resets the send side with an application error code.
func (st *webTransportStream) CancelWrite(code uint32) {
	if !st.canWrite() {
		return
	}
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	if st.finSent {
		return
	}
	st.finSent = true
	st.abort(1, code)
}

// CancelRead asks the peer to stop sending, with an application error code, and fails pending and
// later reads.
func (st *webTransportStream) CancelRead(code uint32) {
	if !st.canRead() {
		return
	}
	st.mu.Lock()
	if st.fin || st.readClosed {
		st.mu.Unlock()
		return
	}
	st.readClosed = true
	dropped := len(st.buf) + st.taken
	st.buf, st.taken = nil, 0
	st.cond.Broadcast()
	st.mu.Unlock()
	if dropped > 0 { // the engine's receive window counts these bytes as read
		st.w.ctlMu.Lock()
		_, _ = st.w.ctl.ChromeWtStreamConsume(st.w.id, st.id, uint32(dropped))
		st.w.ctlMu.Unlock()
	}
	st.abort(2, code)
}

func (st *webTransportStream) abort(mode int32, code uint32) {
	st.w.ctlMu.Lock()
	_, _ = st.w.ctl.ChromeWtStreamAbort(st.w.id, st.id, mode, code)
	st.w.ctlMu.Unlock()
}
