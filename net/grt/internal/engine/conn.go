package engine

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// Socket is the dataplane egress contract a platform adapter provides.
// Send is synchronous: a nil return commits the packet to EMITTED. A
// definitive failure returns non-nil and the packet is BURNED.
type Socket interface {
	Send(payload []byte, dst netip.AddrPort, ecn uint8) error
	LocalAddr() netip.AddrPort
}

// SourceSocket is an optional Socket capability for multi-homed
// endpoints: SendFrom selects the datagram's local address. The engine
// uses it when a path's local tuple differs from the socket's bound
// address (spec section 17/30: each send is bound to its authorized
// path tuple, not whichever default route exists).
type SourceSocket interface {
	Socket
	SendFrom(payload []byte, src, dst netip.AddrPort, ecn uint8) error
}

// GSOSocket is an optional Socket capability for UDP segmentation offload
// (spec section 30). Multiple packets of uniform segmentSize (with the final
// packet possibly shorter) concatenated in payload are delivered in a single
// system call.
type GSOSocket interface {
	Socket
	SendGSO(payload []byte, segmentSize uint16, dst netip.AddrPort, ecn uint8) error
}

// SourceGSOSocket is an optional Socket capability combining source address
// selection with UDP segmentation offload (spec section 30).
type SourceGSOSocket interface {
	GSOSocket
	SendFromGSO(payload []byte, segmentSize uint16, src, dst netip.AddrPort, ecn uint8) error
}

// QueueSocket is an optional Socket capability for adapters with an
// asynchronous transmit queue (spec section 30, implementation option).
// QueueSend hands the datagram to the adapter's queue; the payload is a
// pooled packet buffer whose ownership transfers to the adapter — it must
// release it with PutPacketBuf once transmitted or abandoned. Every
// outcome is reported to the reporter installed by SetSendReporter,
// tagged with the opaque tag the engine supplied (the packet number).
// Reports may arrive on any goroutine. A nil error commits the send;
// non-nil reports a definitive failure, which the engine rolls back like
// a synchronous Send error — burned PN, restored items — when the report
// event reaches the owner loop. Ordering is preserved: the engine submits
// strictly in packet-number order and expects FIFO transmission.
type QueueSocket interface {
	Socket
	QueueSend(payload []byte, dst netip.AddrPort, ecn uint8, tag uint64)
	SetSendReporter(func(tag uint64, err error))
}

// Config carries the engine-level knobs the public API supplies.
type Config struct {
	Params       wire.Params
	ServerName   string // authority for client
	AppID        [16]byte
	AuthMode     uint8 // 0 PINNED, 1 CHAIN
	ExpectedKey  [32]byte
	ServerCert   *ServerIdentity // server: signing identity
	TrustAnchors [][32]byte      // client: CHAIN anchors
	NowUnix      func() int64    // credential/ticket time source; nil = system clock
	// ReplayLedger consumes (domain,ticketID) durably before early exposure.
	ReplayLedger ReplayLedger
	// TicketStore issues/opens resumption tickets (server side).
	TicketKey    [32]byte
	TicketKeyID  uint32
	TicketIssuer func() ([16]byte, uint64) // prefix, counter
	// PolicyHash binds issued tickets to the server's current
	// authentication-policy digest (spec section 9.1). A ticket minted
	// under a different policy fails validation and the attempt falls
	// back to a full handshake.
	PolicyHash [32]byte
	// ReplayDomain is the server's locally authorized replay domain
	// (spec section 9.1): it is stamped into issued tickets and their
	// metadata, verified on resumption, and scopes replay-ledger
	// consumption of early data.
	ReplayDomain [16]byte
	// Client-side resumption (spec section 9): a stored ticket is offered
	// in the next handshake; EarlyData is opt-in replayable application
	// bytes carried as the first client bidi stream's gen-0 batch.
	Ticket          *ClientTicket
	EarlyData       []byte
	AddressToken    [wire.TokenBytes]byte // stored REUSE token, if any
	HasAddressToken bool
	// Local admission limits (manifest-declared).
	MaxLiveSent    int
	MaxPendingCtrl int
	// MaxAttempts bounds the listener's live post-admission server
	// attempts (spec sections 20.1/37.6); 0 uses defaultMaxAttempts.
	MaxAttempts int
	// MaxConnections bounds total live server connections on a listener
	// (spec section 37.6); 0 means no explicit connection ceiling beyond
	// system resources.
	MaxConnections int
	// SourceAttemptRate and SourceAttemptBurst bound per-source-IP
	// handshake admissions per second (spec section 37.6); 0 uses
	// defaultAttemptSrcRate / defaultAttemptSrcBurst.
	SourceAttemptRate  float64
	SourceAttemptBurst float64
	// AdmissionMode selects the section-20.1 new-attempt policy for a
	// listener: AdmissionRetry (default), AdmissionTokenless, or
	// AdmissionShedding.
	AdmissionMode uint8
	// ResetSecret derives stateless-reset tokens; optional — falls back to
	// TicketKey, else a fresh random per-process secret.
	ResetSecret [32]byte
	// CipherPreference specifies ordered cipher IDs for client offer (spec section 4.1).
	CipherPreference []uint16
	// DisableAES disables AES-256-GCM in server's cipher support mask (spec section 4.1).
	DisableAES bool
	// NowUS overrides the monotonic microsecond clock source (for deterministic simulation).
	NowUS func() uint64
}

// nowUnix is the wall clock for ticket and credential expiry. A nil
// NowUnix must not freeze time at zero: that makes tickets immortal.
func (c *Config) nowUnix() int64 {
	if c.NowUnix != nil {
		return c.NowUnix()
	}
	return time.Now().Unix()
}

// Listener admission modes for new attempts (spec sections 6 and 20.1).
const (
	// AdmissionRetry is REQUIRED_RETRY: a tokenless first flight always
	// draws a stateless RETRY. This is the default.
	AdmissionRetry uint8 = iota
	// AdmissionTokenless is bounded ALLOW_TOKENLESS: a small number of
	// tokenless flights may spool; the reply is sent only when it fits
	// in three times the datagrams actually received on that attempt.
	AdmissionTokenless
	// AdmissionShedding silently drops every new attempt.
	AdmissionShedding
)

// ReplayLedger durably consumes an early-data ticket binding.
// consume returns true iff this attempt may expose early data.
type ReplayLedger interface {
	ConsumeEarly(domain [16]byte, ticketID [16]byte, attempt [16]byte, flightHash [32]byte) bool
}

// ServerIdentity is the server's long-term signing key pair.
type ServerIdentity struct {
	IdentityPub  [32]byte // IK_S (X25519 public, XEdDSA-signable)
	IdentityPriv []byte   // 32-byte X25519 private scalar
	ServiceHash  [32]byte
	Chain        [][wire.CredentialBytes]byte // CHAIN credentials, leaf first
}

// epochSlot is one installed key epoch (spec section 19).
type epochSlot struct {
	state epochSlotState
	epoch uint32
	gener uint32 // slot generation for staleness
	tx    gc.EpochAEAD
	rx    gc.EpochAEAD
	// usage accounting (spec section 4.5)
	sealCount    uint64
	sealBytes    uint64
	graceUntilUS uint64
	installedUS  uint64 // install time, for the soft rekey-time threshold
}

// keyState holds the at-most-three epoch slots and roots. cur indexes the
// CURRENT slot: the epoch under which ordinary traffic is sealed. A STAGED
// slot is installed but not current until promoteStaged; PREVIOUS slots
// keep decrypting during their grace window.
type keyState struct {
	slots [3]epochSlot
	root  [3][32]byte // root[e] for refresh chaining; slot i holds epoch base+i
	base  uint32      // epoch of slots[0]
	cur   int         // index of the CURRENT slot
}

func (k *keyState) find(epoch uint32) *epochSlot {
	for i := range k.slots {
		if k.slots[i].state != epochSlotEmpty && k.slots[i].epoch == epoch {
			return &k.slots[i]
		}
	}
	return nil
}

// current returns the CURRENT epoch slot — never a merely staged or
// grace-period slot (spec section 19.4 TX epoch selection).
func (k *keyState) current() *epochSlot {
	if k.cur >= 0 && k.cur < len(k.slots) && k.slots[k.cur].state != epochSlotEmpty {
		return &k.slots[k.cur]
	}
	// Fallback before the first promotion bookkeeping runs.
	var best *epochSlot
	for i := range k.slots {
		s := &k.slots[i]
		if s.state == epochSlotInstalled && (best == nil || s.epoch < best.epoch) {
			best = s
		}
	}
	return best
}

// staged returns the installed non-current slot (the refresh STAGED slot).
func (k *keyState) staged() *epochSlot {
	for i := range k.slots {
		s := &k.slots[i]
		if i != k.cur && s.state == epochSlotInstalled {
			return s
		}
	}
	return nil
}

// isCurrent reports whether s is the CURRENT slot.
func (k *keyState) isCurrent(s *epochSlot) bool {
	return k.cur >= 0 && k.cur < len(k.slots) && &k.slots[k.cur] == s
}

// cidRec is one connection-ID record.
type cidRec struct {
	cid    uint64
	seq    uint64
	token  [16]byte // reset token (peer CIDs) or our token (local CIDs)
	retire bool
	valid  bool
}

// pathRec is one path slot (spec section 17). Each path owns its RTT,
// loss, congestion, and pacing state (spec sections 14.3, 15.1).
type pathRec struct {
	role   pathRole
	remote netip.AddrPort
	local  netip.AddrPort
	gen    uint32 // path generation
	// anti-amplification accounting for unvalidated remote
	rxBytes   uint64
	rxPkts    uint64
	txBytes   uint64
	txPkts    uint64
	validated bool
	// validation state (section 17.3)
	challenge       [16]byte
	challengeBytes  int    // wire size of our latest emitted challenge
	challengeSentUS uint64 // first-send anchor (t0) of the current challenge transaction
	responseBytes   int    // wire size of the matching response
	validating      bool
	localInit       bool // we initiated this validation round
	stale           bool // parked path: needs revalidation on wake
	validUntilUS    uint64
	nextProbeUS     uint64 // next permitted challenge send (retry spacing)
	probesSent      int
	// implicit-promotion evidence (section 17.4)
	peerPN           uint64 // largest committed peer PN on this path
	peerSet          bool   // a peer packet was committed on this path
	createdWatermark uint64 // largest committed peer PN at creation
	evidencePN       uint64 // largest committed non-probing PN here
	intentSelected   bool   // our SELECT=1 intent already queued
	// promotion bookkeeping
	cutoverPN  uint64
	promotedUS uint64 // nonzero once this record became ACTIVE via promotion
	migSampled bool   // one valid same-path RTT sample since promotion
	pstate     pathSt
	// PMTU probing (section 17.2)
	mtuHi       uint64 // current ceiling (0 = derive from peer params)
	mtuTries    int    // failed attempts at the current target
	mtuTarget   uint64 // in-flight probe target size
	mtuNextUS   uint64 // next permitted probe send
	mtuProbeID  uint64
	mtuProbeOut bool // a probe is outstanding
	baseAcked   bool // a non-probe eliciting packet was acked on this path
	// persistent-congestion run (section 15.2): the send-ordered span of
	// ack-eliciting packets declared lost since the run started; any
	// acknowledged or still-outstanding transmission inside the span
	// disqualifies it.
	pcLoOrd  uint64
	pcHiOrd  uint64
	pcLoSent uint64
	pcHiSent uint64
	pcN      int
	// ECN per-path outcome
	ecn ecnOutcome
	// RTT/loss (section 14)
	srtt         uint64
	rttvar       uint64
	minRTT       uint64
	minSet       bool
	latestRTT    uint64
	firstSample  bool
	ptoCount     uint32
	lastElicitUS uint64
	ordinal      uint64 // monotonic path emission ordinal
	// congestion (section 15)
	cwnd        uint64
	ssthresh    uint64
	inFlight    uint64
	caAccum     uint64
	m           uint64 // current max datagram size
	recBoundary uint64 // path ordinal of last congestion event
	inRecovery  bool
	// pacing (section 15.3): token bucket, fixed-point
	paceTokens uint64 // scaled by paceScale
	paceLastUS uint64
	// ECN send-side counters (section 15.4)
	sentECT0      uint64
	ackedECT0     uint64
	lastCountedE0 uint64 // ackedECT0 at the last accepted ECN report
	lastAckECT0   uint64
	lastAckCE     uint64
	ecnDisabled   bool
	// highest path ordinal ever acknowledged on this path
	maxAckOrd uint64
	hadAck    bool
	// liveHead/liveTail root this path's live sent packets in send-ordinal
	// order (intrusive list owned by core.ledger).
	liveHead *sentPkt
	liveTail *sentPkt
}

// ctrlItem is one reliable control-frame obligation. agen is a per-slot
// allocation generation: a sent packet's loss list refers to an item by
// (index, agen), so a recycled slot can never be resolved by a stale
// reference from an earlier occupant.
type ctrlItem struct {
	kind uint8
	slot uint16 // stream slot for stream-scoped kinds
	path int16  // pinned path index; -1 emits on whichever path is built
	gen  uint64 // stream generation for stream-scoped kinds
	a2   uint64 // PATH_INTENT/PATH_SELECTED sequence; CRYPTO_FRAGMENT in-flight bitmap
	// a carries the obligation's scope identity (full stream ID, slot
	// class, CID value or sequence, park request ID) and participates
	// in queue dedupe. For the monotonic credit kinds b is the pending
	// absolute limit; for CRYPTO_FRAGMENT a is instead the mutable
	// outstanding-fragment bitmap, exempt from identity comparison.
	a, b uint64
	c    uint32
	agen uint32 // allocation generation for loss-list references
	tok  [16]byte
	body []byte // owned payload for CRYPTO_FRAGMENT / NEW_TICKET
	live bool   // inflight in an unacked packet
	used bool
}

const (
	ctrlMaxData uint8 = iota
	ctrlMaxStreamData
	ctrlMaxSlots
	ctrlReset
	ctrlStopSending
	ctrlRetire
	ctrlRetireAck
	ctrlFinish
	ctrlHandshakeDone
	ctrlNewCID
	ctrlRetireCID
	ctrlNewToken
	ctrlNewTicket
	ctrlPathChallenge
	ctrlPathResponse
	ctrlPathIntent
	ctrlPathSelected
	ctrlPing
	ctrlKeyRequest
	ctrlCryptoFrag
	ctrlKeyCommit
	ctrlKeyConfirm
	ctrlParkRequest
	ctrlParkGrant
	ctrlParkWake
	ctrlParkAwake
	ctrlClose
)

// ClientTicket is a stored resumption ticket plus derived PSK.
type ClientTicket struct {
	Ticket [wire.TicketBytes]byte
	Meta   [wire.TicketMetaBytes]byte
	PSK    [32]byte
	ID     [16]byte
}

// handshake carries the establishment working state for one attempt.
type handshake struct {
	attemptID [wire.AttemptIDBytes]byte
	offer     [wire.MaxCipherOffers]uint16 // client cipher offer
	token     [wire.TokenBytes]byte
	hasToken  bool
	retrySeen bool
	// client
	clientMsg   []byte // immutable flight bytes
	clientFrags [][]byte
	serverMsg   []byte  // assembled SP||body
	srvFrags    []rxSeg // fragment reassembly
	srvTotal    uint32
	srvCnt      uint16
	srvHave     int
	srvHash     [32]byte // pinned message_hash of the server flight
	flightHash  [32]byte // client flight hash bound into the transcript
	// ephemeral key material (client)
	ikC   gc.X25519KeyPair
	spkC  gc.X25519KeyPair
	pqpkC gc.MLKEM768KeyPair
	// shared secrets
	sk [32]byte
	// server reassembly (per-attempt bounded slot)
	cliFrags []rxSeg
	cliTotal uint32
	cliCnt   uint16
	cliHave  int
	cliMsg   []byte
	// transcript results
	t0 [32]byte
	t1 [32]byte
	// early
	earlyPlain  []byte
	earlyCh0End int    // client: ch0 boundary for EK derivation
	earlyNoBnd  int    // client: no-binder boundary
	earlySID    uint64 // client: stream id carrying the early batch
	earlyStatus uint8  // negotiated early-data result
	earlyReject bool   // committed early data was refused
	// cached server flight for retransmission (server side)
	serverFlight [][]byte
	// selected ticket material
	ticketPSK [32]byte
	hasPSK    bool
	// retry bookkeeping
	retryDCID uint64
	// peer's FINISH MAC (server side, for HANDSHAKE_DONE)
	cliFinishMAC [32]byte
	// paced flight emission state (section 6.4)
	flight flightTx
	// duplicate-flight reemission gate (server): the cached server flight
	// answers a repeated client flight at most once per client-RTX round
	dupNextUS uint64
}

// flightTx paces one bootstrap flight's emission (spec section 6.4): an
// emission round releases at most flightBurst datagrams back to back and
// paces each further datagram by its own transmission time at
// 12000/INITIAL_RTT_US bytes per microsecond. The client repeats whole
// rounds on a doubling retransmission backoff; the server starts a new
// round per admitted duplicate client flight.
type flightTx struct {
	frags   [][]byte // immutable datagrams of the current flight
	idx     int      // next fragment to emit this round
	burst   int      // immediate datagrams remaining this round (<=2)
	nextUS  uint64   // paced send time; round deadline once idx==len(frags)
	roundUS uint64   // anchor of the current emission round
	rtxUS   uint64   // client: current retransmission interval (doubles)
}

// connCore is the section-22 connection object. All fields are owned by the
// connection's run goroutine except as documented.
type connCore struct {
	role  role
	state connState
	gen   uint32

	cfg   *Config
	local wire.Params
	peer  wire.Params

	sock Socket

	// CIDs: peerCID is the current destination CID; localCIDs are ours.
	peerCID   cidRec
	localCID  cidRec
	peerCIDs  [wire.MaxActiveCIDs]cidRec
	localCIDs [wire.MaxActiveCIDs]cidRec

	// crypto
	keys     keyState
	cipher   uint16
	cipherSt uint8
	root0    [32]byte
	t1       [32]byte
	finKey   [32]byte
	doneKey  [32]byte
	// retained master secrets (spec section 11.3): resumption feeds
	// ticket PSKs; exporter feeds application exporters.
	resumptionMaster [32]byte
	exporterMaster   [32]byte
	cliReset         [16]byte // our reset token for our handshake CID (client)

	// packet machinery
	nextPN    uint64
	txBuildID uint64 // bumped per packet build; dedupes per-packet OPENs
	replay    replayWin
	ledger    *sentLedger
	acker     ackState

	// flow control
	connTxUsed uint64 // reserved high-water sum across halves
	connRxUsed uint64
	connTxLim  uint64 // peer MAX_DATA
	connRxLim  uint64 // our advertised MAX_DATA
	connRxAdv  uint64 // bytes the application has consumed (credit base)

	// streams
	slots slotTable
	// DRR scheduler cursor (section 14.5): the flattened (class, slot)
	// scan position where the next emitStreamData traversal resumes.
	schedClass uint8
	schedIdx   uint16

	// control queue (bounded); ctrlAgen tags each slot allocation so
	// stale loss-list references cannot resolve a recycled item.
	ctrl     []ctrlItem
	ctrlAgen uint32
	// Section-16 control-only allowance: every emitted DATA packet earns
	// one credit and a non-exempt control-only packet spends four, so
	// such packets never exceed one per four emitted while application
	// traffic is eligible.
	ctrlCredit uint64

	// timers (monotonic microseconds)
	deadline [nTimerKinds]uint64
	timerGen [nTimerKinds]uint32
	armed    [nTimerKinds]bool

	// paths
	paths     [wire.MaxPaths]pathRec
	activePth int
	pathGen   uint32
	// migration intent bookkeeping (section 17.4/17.5)
	peerIntentSeq      uint64 // highest intent sequence the peer has sent
	peerIntentSel      bool   // that sequence was recorded with SELECT=1
	peerIntentCut      uint64 // its recorded cutover barrier (peer PN space)
	peerIntentFlags    uint32 // its recorded intent flags
	peerIntentPath     int    // ingress path record carrying the live intent
	peerIntentRec      bool   // a peer intent has been recorded
	peerCutoverFloor   uint64 // accepted peer cutover barrier (peer PN space)
	localIntentSeq     uint64 // our last used intent sequence (starts at 1)
	localIntentPath    int    // candidate carrying our live intent, -1 none
	localIntentCutover uint64 // our PN barrier recorded in the live SELECT
	localIntentDone    bool   // SELECTED result resolved for the live intent
	localIntentDL      uint64 // bounded deadline for SELECT result/retries
	// carrying-PN of the packet being committed (PATH_INTENT guard)
	lastPeerPN uint64
	// cached result of the last completed selection, regenerated for
	// duplicate SELECT intents (section 17.4)
	selSeq    uint64
	selStatus uint16
	selOK     bool
	// promotion hysteresis: routine elective promotions are separated by
	// at least max(one base PTO, 1s) (section 17.6)
	lastPromoUS uint64
	// server: a PARK_AWAKE obligation is pending emission on wakePath
	// (never coalesced with application frames, section 35.6)
	awakePend bool
	// client: PARK_AWAKE received while awaiting path revalidation readiness
	awakeSeen bool
	// largest PN ever seen in an ACK's first range (section 15.4 needs a
	// monotonic "newer largest" reference for counter validation)
	ackLargestPN uint64
	ackLargestOK bool
	// sender ECN disabled for the rest of the connection once a migration
	// or detected tuple change occurs (section 15.4)
	ecnKilled bool
	// cumulative committed-DATA ECN receive counters reported in ACKs;
	// connection-level so they continue across migration (section 15.4)
	rxECT [3]uint64

	// listener back-pointer for server conns
	listener *Listener
	// attemptHeld records that this conn owns a slot in the listener's
	// bounded attempt pool (spec sections 20.1/37.6); released exactly
	// once at handshake confirmation or death.
	attemptHeld bool
	// attemptTokenless marks an attempt admitted without an address token,
	// charged against the listener's tokenless share until release.
	attemptTokenless bool

	// durable local reset secret (client: random per conn; server: config)
	resetSecret [32]byte

	// key-refresh transaction (section 19)
	refresh refreshTx

	// CID issuance bookkeeping (section 18)
	peerCIDSeq    uint64
	peerCIDSeqSet bool
	localCIDSeq   uint64

	// resumption material (client side)
	resume      *ClientTicket
	newToken    [wire.TokenBytes]byte
	hasNewToken bool

	// handshake working state
	hs handshake

	// PTO probe budget for the current expiration (section 14.4)
	probeBudget int
	// last conn-level limit for which we emitted a blocked advisory
	blockedAt       uint64
	wantDataBlocked bool

	// lifecycle
	lastActivityUS uint64
	closeSentUS    uint64
	closeCode      closeCode
	closeRemote    bool
	closeSent      bool

	// anti-amplification is per-path; unvalidated budget enforced at TX.
	// auth failure counter (section 25 protection)
	authFails uint64

	// parking (section 35)
	parked      bool   // application send/admission gate closed
	parkSt      parkSt // lease component state
	parkID      uint64 // live lease request id
	parkDurMS   uint32 // server: granted duration for parkID (cached result)
	parkResult  int16  // server: cached result status for parkID (-1 none)
	parkLeaseUS uint64 // fixed lease expiry (never extended by duplicates)
	parkReqUS   uint64 // client: first request handoff (conservative lease base)
	parkSends   int    // transmissions of the pending request/wake/result
	parkBaseUS  uint64 // first handoff anchoring the 0/pto/3pto resend grid
	parkNextUS  uint64 // next scheduled resend of the pending park control
	parkDL      uint64 // client request/wake resolution deadline
	ctrlPTO     uint64 // lease_control_pto snapshot: max(100ms, base_pto)
	wakeSeq     uint64 // client: current wake attempt sequence
	wakeSends   int    // client: PARK_WAKE transmissions for wakeSeq
	wakeDL      uint64 // client: wake attempt resolution deadline
	peerWakeSeq uint64 // server: highest wake sequence applied for parkID
	wakePath    int    // server: path the pending PARK_AWAKE must emit on
	lastParkReq uint64 // highest peer PARK_REQUEST id seen
	lastParkDur uint32 // duration of that request (contradiction check)
	grantStatus uint16 // cached result for lastParkReq
	grantDur    uint32 // cached granted duration for lastParkReq
	parkReqSeen bool

	// locally initiated candidate-validation probe bucket (section
	// 17.3): capacity three 1200-byte datagrams, refill one per second.
	probeToks  float64
	probeLastU uint64
	// migration startup bandwidth bucket (section 17.3): 12000-byte
	// capacity, initially full, refill 12000 bytes/s; charged while a
	// freshly promoted path lacks its first valid RTT sample.
	migToks  uint64
	migLastU uint64

	// datagram app queues
	dgOut [][]byte
	dgIn  [][]byte

	// terminal error for the API surface
	termErr error
	// constructor-time failure (e.g. entropy source) reported once the
	// owner loop starts so the conn dies through the normal quiesce path
	initErr error
}

// Monotonic microsecond clock anchored at process start.
var (
	epoch0    = time.Now()
	globalNow atomic.Pointer[func() uint64]
)

// nowUS returns the monotonic clock in microseconds.
func nowUS() uint64 {
	if fn := globalNow.Load(); fn != nil {
		return (*fn)()
	}
	return uint64(time.Since(epoch0).Microseconds())
}

// SetGlobalNowUS sets a process-wide simulated monotonic microsecond time function.
// In production, pass nil to restore the system monotonic clock.
func SetGlobalNowUS(fn func() uint64) {
	if fn == nil {
		globalNow.Store(nil)
		return
	}
	globalNow.Store(&fn)
}

// nowUS returns the monotonic clock in microseconds for this connection.
func (c *Conn) nowUS() uint64 {
	if c != nil && c.core != nil && c.core.cfg != nil && c.core.cfg.NowUS != nil {
		return c.core.cfg.NowUS()
	}
	return nowUS()
}

// ---------------------------------------------------------------------------
// Mailbox / owner loop

// Conn is the public handle; core state lives on the run goroutine.
type Conn struct {
	core *connCore
	mbox chan connEvent
	done chan struct{} // closed on DEAD
	dead chan error    // receives terminal error once

	// app-facing notification (signaled by owner goroutine)
	readWake     chan struct{} // cap 1: some stream readable/closed
	acceptQ      chan uint64   // stream IDs available for accept
	acceptUniQ   chan uint64
	dgWake       chan struct{}
	writeWake    chan struct{} // send-side capacity may have changed
	secureReady  chan struct{} // SECURE_READY (client, server auth verified)
	confirmed    chan struct{} // CONFIRMED
	stateWake    chan struct{} // cap 1: lifecycle state changed
	termVal      error         // terminal error; valid after done closes
	stateSnap    atomic.Int32  // last published connState, for observers
	retrySeen    atomic.Bool   // RETRY was issued during establishment
	closeErr     error
	closeOnce    sync.Once
	wakeup       *time.Timer
	wakeKind     timerKind
	wakeGen      uint32
	wakeDeadline uint64
	wakePending  bool        // a not-yet-fired wakeup is armed for wakeDeadline
	wakeFired    atomic.Bool // wakeup fired since the last dispatch

	// deferred holds DATA datagrams whose epoch key is not yet installed;
	// flushDeferred replays them in arrival order on key install.
	deferred   []rxDgram
	deferFlush bool // flushDeferred in progress — nested calls skip
	// qsend is non-nil when the socket adapter offers an asynchronous
	// transmit queue (QueueSocket); send outcomes arrive as evSendRep.
	qsend QueueSocket

	// scratch buffers owned by the run goroutine
	txfb     []byte // wire packet assembly (header || ciphertext)
	framebuf []byte // plaintext frame staging — must not alias txfb
	txhdr    [wire.HeaderBytes]byte

	itemScratch [wire.MaxFrames]lossItem

	txBatch    txBatch
	txBatching bool
}

var pktPool = sync.Pool{
	New: func() any {
		return new([wire.MaxUDPPayload]byte)
	},
}

// GetPacketBuf borrows a packet buffer from the pool.
func GetPacketBuf() []byte {
	return pktPool.Get().(*[wire.MaxUDPPayload]byte)[:]
}

// PutPacketBuf returns a packet buffer to the pool.
func PutPacketBuf(b []byte) {
	if cap(b) >= wire.MaxUDPPayload {
		b = b[:cap(b)]
		pktPool.Put((*[wire.MaxUDPPayload]byte)(unsafe.Pointer(&b[0])))
	}
}

type txBatchItem struct {
	pn          uint64
	blockedEmit bool
	sp          *sentPkt // stable item copy and metadata
	inFlight    bool
	pktLen      int
}

type txBatch struct {
	active      bool
	pi          int
	local       netip.AddrPort
	remote      netip.AddrPort
	segmentSize uint16
	ecn         uint8
	buf         []byte
	items       []txBatchItem
}

// newConn allocates a connection object with generation gen.
func newConn(cfg *Config, sock Socket, gen uint32) *Conn {
	c := &Conn{
		core: &connCore{
			gen:             gen,
			cfg:             cfg,
			local:           cfg.Params,
			sock:            sock,
			ledger:          newSentLedger(cfg.MaxLiveSent),
			localIntentPath: -1,
		},
		mbox:     make(chan connEvent, 4096),
		done:     make(chan struct{}),
		dead:     make(chan error, 1),
		readWake: make(chan struct{}, 1),
		// Pending-accept queues hold every peer-opened stream the
		// application has not consumed yet. Capacity equals the
		// maximum open grant we can ever advertise, so a conforming
		// peer can never fill them and a stream ID is never dropped
		// after the open commits (section 12/36).
		acceptQ:     make(chan uint64, wire.MaxStreamSlots),
		acceptUniQ:  make(chan uint64, wire.MaxStreamSlots),
		dgWake:      make(chan struct{}, 1),
		writeWake:   make(chan struct{}, 1),
		secureReady: make(chan struct{}),
		confirmed:   make(chan struct{}),
		stateWake:   make(chan struct{}, 1),
	}
	c.core.initErr = c.core.initResetSecret(cfg)
	// The replay bitmap is exactly as wide as the receive_packet_history
	// we advertise. An out-of-range or non-power-of-two configured value
	// normalizes to the protocol minimum rather than being advertised
	// falsely (section 13.1).
	if v := c.core.local.ReceivePacketHistory; v < wire.MinReplayWindow ||
		v > wire.MaxReplayWindow || v&(v-1) != 0 {
		c.core.local.ReceivePacketHistory = wire.MinReplayWindow
	}
	c.core.replay.width = uint64(c.core.local.ReceivePacketHistory)
	c.core.localIntentPath = -1
	c.core.wakePath = -1
	c.core.ledger.paths = &c.core.paths
	c.core.acker.init(&c.core.local)
	c.txfb = make([]byte, wire.MaxUDPPayload)
	c.framebuf = make([]byte, wire.MaxUDPPayload)
	c.txBatch.items = make([]txBatchItem, 0, wire.MaxLocalBatchDatagrams)
	c.wakeup = time.AfterFunc(time.Hour, c.onWakeup)
	c.wakeup.Stop()
	return c
}

// post delivers an event; returns false if the mailbox is full.
func (c *Conn) post(ev connEvent) bool {
	select {
	case c.mbox <- ev:
		return true
	default:
		return false
	}
}

// postWait posts an app command and waits for its result.
func (c *Conn) postWait(cmd *appCmd) appResult {
	if cmd.res == nil {
		cmd.res = make(chan appResult, 1)
	}
	select {
	case c.mbox <- connEvent{kind: evApp, cmd: cmd}:
	case <-c.done:
		return appResult{err: errClosed}
	}
	select {
	case r := <-cmd.res:
		return r
	case <-c.done:
		// The owner may have answered and then died in the same loop
		// iteration; leaving that result buffered would hand it to the
		// next pooled user of cmd, possibly on another connection.
		select {
		case r := <-cmd.res:
			return r
		default:
			return appResult{err: errClosed}
		}
	}
}

// notify nudges a cap-1 wake channel.
func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// rearmWakeup recomputes the nearest armed deadline and (re)arms the
// single physical timer that posts an evTimer into the mailbox.
func (c *Conn) rearmWakeup(now uint64) {
	core := c.core
	var best uint64
	var kind timerKind
	found := false
	for k := timerKind(0); k < nTimerKinds; k++ {
		if !core.armed[k] {
			continue
		}
		if !found || core.deadline[k] < best {
			best, kind, found = core.deadline[k], k, true
		}
	}
	if !found {
		if c.wakeup != nil && c.wakePending {
			c.wakeup.Stop()
			c.wakePending = false
		}
		c.wakeDeadline = 0
		return
	}
	g := core.timerGen[kind]
	// An armed-but-unfired wakeup already covers its deadline; only a
	// newly earlier deadline needs a physical reset. A fired-but-not-
	// yet-dispatched wakeup is re-armed fresh.
	if c.wakePending && !c.wakeFired.Load() && c.wakeDeadline <= best {
		return
	}
	if c.wakeup != nil && c.wakePending {
		c.wakeup.Stop()
		c.wakePending = false
	}
	d := time.Duration(int64(best)-int64(now)) * time.Microsecond
	if d < 0 {
		d = 0
	}
	c.wakeKind, c.wakeGen, c.wakeDeadline = kind, g, best
	// wakeFired must clear before the timer can fire: a zero-duration
	// reset may invoke onWakeup concurrently, and clearing after the arm
	// would erase that firing and leave the loop asleep on an elapsed
	// deadline forever.
	c.wakeFired.Store(false)
	if c.wakeup == nil {
		c.wakeup = time.AfterFunc(d, c.onWakeup)
	} else {
		c.wakeup.Reset(d)
	}
	c.wakePending = true
}

func (c *Conn) onWakeup() {
	c.wakeFired.Store(true)
	c.post(connEvent{kind: evTimer})
}

// run is the owner goroutine: the section-23 event ordering.
func (c *Conn) run() {
	core := c.core
	core.lastActivityUS = nowUS()
	c.stateSnap.Store(int32(core.state))
	if core.initErr != nil {
		c.quiesce(core.initErr)
	}
	if qs, ok := core.sock.(QueueSocket); ok {
		c.qsend = qs
		qs.SetSendReporter(func(tag uint64, err error) {
			// A dropped report self-heals: the peer never saw the packet,
			// so ordinary loss detection retires it.
			_ = c.post(connEvent{kind: evSendRep, rep: tag, serr: err})
		})
	}

	prev := core.state
	var rxq []connEvent
	var appq []*appCmd
	var timq []connEvent
	var repq []connEvent
	add := func(e *connEvent) {
		switch e.kind {
		case evRX:
			if e.batch == nil {
				rxq = append(rxq, *e)
				break
			}
			for i := range e.batch.dgs {
				d := &e.batch.dgs[i]
				rxq = append(rxq, connEvent{kind: evRX, pkt: d.pkt,
					src: d.src, dst: d.dst, ecn: d.ecn, rxUS: d.rxUS,
					h: d.h, hasH: true})
			}
			e.batch.dgs = e.batch.dgs[:0]
			rxBatchPool.Put(e.batch)
			e.batch = nil
		case evApp:
			appq = append(appq, e.cmd)
		case evTimer:
			timq = append(timq, *e)
		case evSendRep:
			repq = append(repq, *e)
		}
	}
	for core.state != stDead {
		// Arm the physical wakeup only when the mailbox is empty and the
		// loop is about to block: while events keep arriving the in-loop
		// deadline scan fires expiries itself, so per-iteration timer
		// resets (each waking the netpoller) are wasted work.
		var ev connEvent
		select {
		case ev = <-c.mbox:
		default:
			c.rearmWakeup(nowUS())
			ev = <-c.mbox
		}
		rxq = rxq[:0]
		appq = appq[:0]
		timq = timq[:0]
		repq = repq[:0]
		add(&ev)
	drain:
		for i := 0; i < 256; i++ {
			select {
			case e := <-c.mbox:
				add(&e)
			default:
				break drain
			}
		}

		for i := range rxq {
			if !c.rxPacket(&rxq[i]) {
				PutPacketBuf(rxq[i].pkt)
			}
		}
		for i := range repq {
			c.sendReport(repq[i].rep, repq[i].serr)
		}
		for _, cmd := range appq {
			c.appCommand(cmd)
		}
		now := nowUS()
		for _, e := range timq {
			if e.timer != 0 || e.timerg != 0 {
				c.timerFired(e.timer, e.timerg, now)
			}
		}
		for k := timerKind(0); k < nTimerKinds; k++ {
			if core.armed[k] && core.deadline[k] <= now {
				c.timerFired(k, core.timerGen[k], now)
			}
		}
		if core.state != stDead {
			c.txSchedule()
		}
		if core.state != prev {
			prev = core.state
			notify(c.stateWake)
		}
		c.stateSnap.Store(int32(core.state))
	}
	c.stateSnap.Store(int32(stDead))
	notify(c.stateWake)
	if c.wakeup != nil {
		c.wakeup.Stop()
	}
	// Datagrams held for a never-installed epoch are released here.
	for i := range c.deferred {
		PutPacketBuf(c.deferred[i].pkt)
	}
	c.deferred = nil
	// Release the listener's routing entries so dead conns cannot
	// accumulate in the demux tables.
	if core.listener != nil {
		core.listener.removeConn(c)
	}
	err := core.termErr
	if err == nil {
		err = errClosed
	}
	// termVal is written before done closes so any observer reading it
	// after <-Done() sees the terminal error (idempotent, multi-reader).
	c.termVal = err
	close(c.done)
	c.dead <- err
}

// arm sets a named deadline in microseconds.
func (core *connCore) arm(k timerKind, atUS uint64) {
	core.deadline[k] = atUS
	core.armed[k] = true
	core.timerGen[k]++
}

// disarm clears a named deadline.
func (core *connCore) disarm(k timerKind) { core.armed[k] = false }

// timerFired handles an expiry; stale generations are ignored.
func (c *Conn) timerFired(k timerKind, g uint32, now uint64) {
	core := c.core
	if !core.armed[k] || core.timerGen[k] != g {
		return
	}
	core.armed[k] = false
	if now < core.deadline[k] {
		// Early/stale fire — rearm at the real deadline.
		core.armed[k] = true
		return
	}
	c.onDeadline(k, now)
}
