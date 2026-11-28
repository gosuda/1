package grt

import (
	"errors"

	"gosuda.org/grt/internal/engine"
)

// Sentinel errors. Stream.Read maps the end-of-stream terminator to
// io.EOF; the raw values remain available for callers that distinguish
// them.
var (
	// ErrClosed reports an operation on a dead or closing connection,
	// or on a stream whose generation no longer exists.
	ErrClosed = engine.ErrClosed
	// ErrWouldBlock reports an operation that cannot complete now
	// (flow control, bounded queue, parked session).
	ErrWouldBlock = engine.ErrWouldBlock
	// ErrHandshakeFailed reports an establishment failure before any
	// close frame existed.
	ErrHandshakeFailed = engine.ErrHandshakeFailed
	// ErrStreamReset reports a peer RESET_STREAM on the receive half.
	ErrStreamReset = engine.ErrStreamReset
	// ErrPeerStop reports the peer's STOP_SENDING on the send half.
	ErrPeerStop = engine.ErrPeerStop
	// ErrStreamEOF reports a fully consumed FIN (Stream.Read maps this
	// to io.EOF).
	ErrStreamEOF = engine.ErrStreamEOF
	// ErrNoDatagrams reports SendDatagram before the feature was
	// negotiated by both peers.
	ErrNoDatagrams = engine.ErrNoDatagrams
	// ErrDatagramTooBig reports a datagram larger than the peer's
	// advertised limit.
	ErrDatagramTooBig = engine.ErrDatagramTooBig
	// ErrNoMigration reports Migrate without the negotiated feature.
	ErrNoMigration = engine.ErrNoMigration
	// ErrNoPark reports parking commands without the negotiated
	// feature.
	ErrNoPark = engine.ErrNoPark
)

// Configuration errors returned by Dial and Listen.
var (
	errNeedServerName = errors.New("grt: Config.ServerName is required for Dial")
	errNeedServerID   = errors.New("grt: Config.ServerIdentity is required for Listen")
)
