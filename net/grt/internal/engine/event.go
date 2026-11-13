package engine

import (
	"net/netip"
	"sync"

	"gosuda.org/grt/internal/wire"
)

// Event classes, ordered per spec section 23.1: fatal fault and close are
// checked first, then I/O results (fused into RX/TX for the portable
// adapter), network events, crypto completions, app commands, timers, TX.
type evKind uint8

const (
	evRX      evKind = iota // received datagram
	evApp                   // application command
	evTimer                 // named deadline expiry
	evNet                   // local network/interface notification
	evSendRep               // asynchronous socket send report (QueueSocket)
)

// rxDgram is one received datagram inside a batch event.
type rxDgram struct {
	pkt  []byte
	src  netip.AddrPort
	dst  netip.AddrPort
	ecn  uint8
	h    wire.Header
	rxUS uint64
}

// rxBatch carries several datagrams through one mailbox admission. The
// run loop expands it into the rx queue in arrival order and returns it
// to the pool; the datagram buffers themselves are released per packet.
type rxBatch struct {
	dgs []rxDgram
}

var rxBatchPool = sync.Pool{New: func() any {
	return &rxBatch{dgs: make([]rxDgram, 0, 32)}
}}

// connEvent is one mailbox entry. The union fields are valid per kind.
type connEvent struct {
	kind evKind

	// evRX
	pkt   []byte // borrowed; valid until event returns
	src   netip.AddrPort
	dst   netip.AddrPort
	ecn   uint8
	rxUS  uint64
	h     wire.Header // valid when hasH: parsed upstream, skip re-parse
	hasH  bool
	batch *rxBatch // non-nil: evRX carries dgs; pkt/src/dst fields unused

	// evApp
	cmd *appCmd

	// evTimer
	timer  timerKind
	timerg uint32

	// evSendRep
	rep  uint64 // packet number the adapter reported on
	serr error  // nil = committed; non-nil = definitive send failure
}

// appCmdKind enumerates application operations (spec section 23 APP_*).
type appCmdKind uint8

const (
	acOpen appCmdKind = iota
	acOpenUni
	acWrite
	acFin
	acReset
	acStop
	acConsume
	acDatagram
	acClose
	acMigrate
	acPark
	acWake
	acReadDG   // drain one inbound datagram payload
	acNetEvent // local network/interface event: revalidate current remote on this socket
	acDial     // client-only: start the handshake toward remote
	acSrvBuild // server-only: build the server flight from the reassembled client flight
	acQuery    // read conn-held resumption state (code selects the field)
	acRefresh  // client-only: start a key-refresh transaction now
)

// appCmd is one application operation posted to the owner goroutine.
// Results are reported on res (exactly once, buffered).
type appCmd struct {
	kind   appCmdKind
	stream uint64 // StreamID for stream ops
	data   []byte // WRITE payload (ownership transfers on acceptance)
	res    chan appResult
	// acClose, acReset, acStop, acPark (park duration ms)
	code uint32
	// acMigrate
	migrateTo netip.AddrPort
	// acDial
	remote   netip.AddrPort
	identity *ClientIdentity
	// acQuery queryExport
	label, ectx string
}

// Query selectors for acQuery (appCmd.code).
const (
	queryTicket uint32 = iota // stored resumption ticket
	queryToken                // stored REUSE address token
	queryEarly                // early-data outcome (wire EarlyStatus)
	querySlot                 // stream-slot FSM state (diagnostics/tests)
	queryEpoch                // current epoch + refresh state (diagnostics/tests)
	queryPath                 // active path remote tuple (diagnostics/tests)
	queryExport               // application exporter (label/context in cmd)
	queryCipher               // negotiated cipher ID (diagnostics/tests)
	queryPark                 // park FSM state (diagnostics/tests)
	queryDebug                // internal engine debug info
)

type appResult struct {
	stream uint64
	n      int
	err    error
	// acQuery payload
	ticket   *ClientTicket
	token    [64]byte
	hasTok   bool
	status   uint8
	remote   netip.AddrPort
	local    netip.AddrPort
	exported [32]byte
	debugStr string
}

// mailbox is a bounded event queue; overflow drops lowest-priority events
// (RX datagrams may be dropped; app commands are never silently dropped —
// callers apply backpressure).
type mailbox chan connEvent
