package engine

import (
	"crypto/subtle"
	"encoding/binary"
	"net/netip"
	"sync"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// Listener is the admission object (spec section 20.1): it owns the socket,
// demuxes DATA by destination CID, performs stateless Retry, and runs
// bounded client-flight reassembly slots.
type Listener struct {
	cfg  *Config
	sock PacketSocket

	mu       sync.RWMutex
	conns    map[uint64]*Conn   // local CID → conn
	attempts map[[16]byte]*Conn // admitted attempt → conn
	// Bounded shared attempt pool (spec sections 20.1/37.6): a slot is
	// reserved at admission and held until the attempt is confirmed or
	// dies, so token validation can never open unlimited expensive
	// PQXDH/signature work.
	attemptsInFlight int
	attemptsBound    int
	gen              uint32
	closed           bool
	accept           chan *Conn

	tokens *gc.TokenService

	// bounded per-attempt reassembly slots; tokenless counts the live
	// ALLOW_TOKENLESS spool entries (section 6)
	reasm     map[reasmKey]*reasmSlot
	tokenless int

	// Stateless-RETRY emission control (section 6.3): a listener-global
	// packet/byte token bucket plus a bounded per-source-address set, so
	// spoofed fragment-zero floods cannot turn the listener into an
	// unbounded Retry emitter.
	retryGlobal tokenBucket
	retrySrc    map[netip.Addr]*tokenBucket
	attemptSrc  map[netip.Addr]*tokenBucket

	// Section 37.7 abuse & admission metrics (guarded by mu).
	retriesSent      uint64
	retryBytesSent   uint64
	retryRateLimited uint64
	tokenFailures    uint64
	reassemblyDrops  uint64
	admissionDrops   uint64
	attemptsAdmitted uint64
}

// ListenerStats is an immutable snapshot of listener occupancy and
// Section 37.7 abuse/admission telemetry counters.
type ListenerStats struct {
	ActiveConnections   int
	AttemptsInFlight    int
	ReassemblyOccupancy int
	TokenlessOccupancy  int
	RetriesSent         uint64
	RetryBytesSent      uint64
	RetryRateLimited    uint64
	TokenFailures       uint64
	ReassemblyDrops     uint64
	AdmissionDrops      uint64
	AttemptsAdmitted    uint64
}

// tokenBucket is a joint packet/byte token bucket: each take() costs one
// packet and cost bytes, refilled at the given packet rate.
type tokenBucket struct {
	pkts   float64
	bytes  float64
	lastUS uint64
}

// take charges one packet of cost bytes against the bucket.
func (b *tokenBucket) take(now uint64, pktRate, pktBurst, cost float64) bool {
	if b.lastUS == 0 {
		b.pkts, b.bytes, b.lastUS = pktBurst, pktBurst*cost, now
	}
	if now > b.lastUS {
		dt := float64(now-b.lastUS) / 1e6
		b.pkts += dt * pktRate
		if b.pkts > pktBurst {
			b.pkts = pktBurst
		}
		b.bytes += dt * pktRate * cost
		if b.bytes > pktBurst*cost {
			b.bytes = pktBurst * cost
		}
		b.lastUS = now
	}
	if b.pkts < 1 || b.bytes < cost {
		return false
	}
	b.pkts--
	b.bytes -= cost
	return true
}

// Retry emission limits are local policy (spec section 6.3 requires the
// buckets; their rates are deployment-tunable). A legitimate client
// retransmits a fragment zero at most a handful of times per attempt.
const (
	retryGlobalPktRate  = 4096.0 // retry datagrams/s listener-wide
	retryGlobalPktBurst = 4096.0
	retrySrcPktRate     = 8.0 // retry datagrams/s per source address
	retrySrcPktBurst    = 8.0
	retrySrcMax         = 1 << 16 // bounded per-source bucket table

	// maxTokenlessSlots bounds concurrently held client flights admitted
	// under ALLOW_TOKENLESS (section 6.3): each is a real allocation the
	// listener carries without any validated token.
	maxTokenlessSlots = 16

	// defaultMaxAttempts is the manifest admission bound applied when
	// Config.MaxAttempts is unset (spec sections 20.1/37.6).
	defaultMaxAttempts = 256

	// Per-source handshake admission rate/burst defaults (section 37.6).
	defaultAttemptSrcRate  = 64.0
	defaultAttemptSrcBurst = 64.0
)

// PacketSocket is the read/write UDP adapter the listener needs.
type PacketSocket interface {
	Socket
	Recv(buf []byte) (n int, src, dst netip.AddrPort, ecn uint8, err error)
}

// reasmKey binds a reassembly slot to its exact attempt: source and
// destination tuples, attempt id, destination CID, and message hash
// (section 6). A fragment matching none of the pinned fields never
// feeds an existing attempt's byte budget.
type reasmKey struct {
	src, dst  netip.AddrPort
	attemptID [wire.AttemptIDBytes]byte
	dcid      uint64
	hash      [32]byte
}

// reasmSlot is one bounded in-flight client-flight reassembly (S_COLLECT).
type reasmSlot struct {
	frags     [][]byte
	total     uint32
	cnt       uint16
	have      int
	hash      [32]byte
	dcid      uint64
	token     [wire.TokenBytes]byte
	createdUS uint64
	// received bootstrap datagrams on this exact attempt/source — the
	// anti-amplification allowance for a tokenless reply (section 6)
	rxBytes uint64
	rxPkts  uint64
}

// NewListener creates a listener on the given socket.
func NewListener(cfg *Config, sock PacketSocket, domain []byte) *Listener {
	var nowf func() uint64
	if cfg.NowUnix != nil {
		nowf = func() uint64 { return uint64(cfg.NowUnix()) }
	}
	bound := cfg.MaxAttempts
	if bound <= 0 {
		bound = defaultMaxAttempts
	}
	return &Listener{
		cfg:           cfg,
		sock:          sock,
		conns:         make(map[uint64]*Conn),
		attempts:      make(map[[16]byte]*Conn),
		attemptsBound: bound,
		accept:        make(chan *Conn, 64),
		reasm:         make(map[reasmKey]*reasmSlot),
		retrySrc:      make(map[netip.Addr]*tokenBucket),
		attemptSrc:    make(map[netip.Addr]*tokenBucket),
		tokens:        gc.NewTokenService(nowf, domain),
	}
}

// Stats returns an atomic snapshot of the listener's current occupancy
// and Section 37.7 abuse/admission counters.
func (l *Listener) Stats() ListenerStats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	// Count unique live connections across CID aliases.
	seen := make(map[*Conn]struct{}, len(l.conns))
	for _, cn := range l.conns {
		seen[cn] = struct{}{}
	}
	return ListenerStats{
		ActiveConnections:   len(seen),
		AttemptsInFlight:    l.attemptsInFlight,
		ReassemblyOccupancy: len(l.reasm),
		TokenlessOccupancy:  l.tokenless,
		RetriesSent:         l.retriesSent,
		RetryBytesSent:      l.retryBytesSent,
		RetryRateLimited:    l.retryRateLimited,
		TokenFailures:       l.tokenFailures,
		ReassemblyDrops:     l.reassemblyDrops,
		AdmissionDrops:      l.admissionDrops,
		AttemptsAdmitted:    l.attemptsAdmitted,
	}
}

// Accept returns the next server connection that completed S_BUILD.
func (l *Listener) Accept() *Conn { return <-l.accept }

// AcceptCh exposes the accept queue for select-based callers.
func (l *Listener) AcceptCh() <-chan *Conn { return l.accept }

// Addr returns the listener's bound local endpoint.
func (l *Listener) Addr() netip.AddrPort { return l.sock.LocalAddr() }

// Close stops admission: new client flights are dropped and pending
// reassembly slots are discarded. Established connections are unaffected.
func (l *Listener) Close() {
	l.mu.Lock()
	l.closed = true
	clear(l.reasm)
	l.tokenless = 0
	l.mu.Unlock()
	l.tokens.Zeroize()
}

// ---------------------------------------------------------------------------
// Datagram ingress

// Datagram is one inbound UDP datagram for the batched receive entry
// points. Data is a pooled packet buffer borrowed for the call.
type Datagram struct {
	Data     []byte
	Src, Dst netip.AddrPort
	ECN      uint8
}

// OnDatagram is called by the socket read loop for each received datagram.
func (l *Listener) OnDatagram(pkt []byte, src, dst netip.AddrPort, ecn uint8) {
	h, ok := wire.ParseHeader(pkt)
	if !ok {
		PutPacketBuf(pkt)
		return
	}
	switch h.Class {
	case wire.ClassData:
		l.mu.RLock()
		cn := l.conns[h.DestCID]
		l.mu.RUnlock()
		if cn != nil {
			if !cn.post(connEvent{kind: evRX, pkt: pkt, src: src, dst: dst, ecn: ecn,
				h: h, hasH: true, rxUS: nowUS()}) {
				PutPacketBuf(pkt)
			}
		} else {
			PutPacketBuf(pkt)
		}
	case wire.ClassClientFlight:
		l.mu.RLock()
		closed := l.closed
		l.mu.RUnlock()
		if !closed {
			l.onClientFlight(pkt, h, src, dst)
		} else {
			PutPacketBuf(pkt)
		}
	case wire.ClassRetry, wire.ClassServerFlight:
		PutPacketBuf(pkt)
	}
}

// OnDatagrams is the batched form of OnDatagram for platforms that
// receive several datagrams per call (e.g. recvmmsg, a channel sweep).
// Demux still runs per datagram, but each destination connection takes
// a single mailbox admission for the whole burst.
func (l *Listener) OnDatagrams(dgrams []Datagram) {
	batches := make(map[*Conn]*rxBatch, 4)
	rxUS := nowUS()
	for i := range dgrams {
		d := &dgrams[i]
		h, ok := wire.ParseHeader(d.Data)
		if !ok {
			PutPacketBuf(d.Data)
			continue
		}
		switch h.Class {
		case wire.ClassData:
			l.mu.RLock()
			cn := l.conns[h.DestCID]
			l.mu.RUnlock()
			if cn == nil {
				PutPacketBuf(d.Data)
				continue
			}
			b := batches[cn]
			if b == nil {
				b = rxBatchPool.Get().(*rxBatch)
				b.dgs = b.dgs[:0]
				batches[cn] = b
			}
			b.dgs = append(b.dgs, rxDgram{
				pkt: d.Data, src: d.Src, dst: d.Dst, ecn: d.ECN,
				h: h, rxUS: rxUS,
			})
		case wire.ClassClientFlight:
			l.mu.RLock()
			closed := l.closed
			l.mu.RUnlock()
			if !closed {
				l.onClientFlight(d.Data, h, d.Src, d.Dst)
			} else {
				PutPacketBuf(d.Data)
			}
		case wire.ClassRetry, wire.ClassServerFlight:
			PutPacketBuf(d.Data)
		}
	}
	for cn, b := range batches {
		if !cn.post(connEvent{kind: evRX, batch: b}) {
			for i := range b.dgs {
				PutPacketBuf(b.dgs[i].pkt)
			}
			b.dgs = b.dgs[:0]
			rxBatchPool.Put(b)
		}
	}
}

// onClientFlight handles an inbound CLIENT_FLIGHT fragment (section 6).
func (l *Listener) onClientFlight(pkt []byte, h wire.Header, src, dst netip.AddrPort) {
	f, ok := wire.ParseFragment(pkt[wire.HeaderBytes:])
	if !ok {
		return
	}
	l.mu.Lock()
	if cn := l.attempts[f.AttemptID]; cn != nil {
		l.mu.Unlock()
		// Admitted attempt: forward duplicate fragments for cached
		// server-flight reemission (section 20).
		cn.post(connEvent{kind: evRX, pkt: pkt, src: src, dst: dst, rxUS: nowUS()})
		return
	}
	key := reasmKey{src: src, dst: dst, attemptID: f.AttemptID,
		dcid: h.DestCID, hash: f.MessageHash}
	slot := l.reasm[key]
	if slot == nil {
		// Bounded but lazy: opportunistically expire slots abandoned
		// past the handshake deadline so stalled attempts cannot hold
		// memory forever.
		now := nowUS()
		for k, s := range l.reasm {
			if now-s.createdUS > wire.HandshakeDeadlineUS {
				if s.token == ([wire.TokenBytes]byte{}) {
					l.tokenless--
				}
				delete(l.reasm, k)
			}
		}
		if l.cfg.AdmissionMode == AdmissionShedding {
			l.admissionDrops++
			l.mu.Unlock()
			return // SHEDDING: every new attempt is silently dropped
		}
		zeroTok := f.Token == ([wire.TokenBytes]byte{})
		if zeroTok && l.cfg.AdmissionMode != AdmissionTokenless {
			l.mu.Unlock()
			if f.Index == 0 {
				l.sendRetry(&f, h, src, dst)
			}
			return
		}
		if !zeroTok {
			// Token-bearing: validate before any per-attempt allocation.
			// Kind-1 (REUSE) validates from this fragment alone; kind-0
			// binds flight hash + outer retry DCID now and original DCID
			// at frag 0.
			tok, tokOK := wire.DecodeToken(f.Token[:])
			if !tokOK {
				l.tokenFailures++
				l.mu.Unlock()
				return
			}
			if tok.Kind == wire.TokenKindRetry {
				if !l.tokens.ValidateToken(f.Token, src, dst, f.MessageHash, h.DestCID, tok.Binding) {
					l.tokenFailures++
					l.mu.Unlock()
					return
				}
			} else if !l.tokens.ValidateToken(f.Token, src, dst, [32]byte{}, 0, 0) {
				// An invalid REUSE token is not a drop: the safe fallback
				// is a stateless RETRY on fragment zero (section 6.3).
				l.tokenFailures++
				l.mu.Unlock()
				if f.Index == 0 {
					l.sendRetry(&f, h, src, dst)
				}
				return
			}
		}
		if len(l.reasm) >= 64 || (zeroTok && l.tokenless >= maxTokenlessSlots) {
			l.reassemblyDrops++
			l.mu.Unlock()
			return
		}
		slot = &reasmSlot{
			frags:     make([][]byte, f.Count),
			total:     f.TotalLength,
			cnt:       f.Count,
			hash:      f.MessageHash,
			dcid:      h.DestCID,
			token:     f.Token,
			createdUS: nowUS(),
		}
		l.reasm[key] = slot
		if zeroTok {
			l.tokenless++
		}
	}
	l.mu.Unlock()

	// Consistency: fixed fields must match across fragments.
	if slot.total != f.TotalLength || slot.cnt != f.Count ||
		slot.hash != f.MessageHash || slot.token != f.Token {
		return
	}
	// For a RETRY-token attempt, fragment zero's original DCID (inside the
	// client message) must equal the token binding; checked at completion
	// inside startServerAttempt.
	l.mu.Lock()
	// Every received bootstrap datagram on this exact attempt/source adds
	// anti-amplification credit for the (possibly tokenless) reply.
	slot.rxBytes += uint64(len(pkt))
	slot.rxPkts++
	if slot.frags[f.Index] != nil {
		l.mu.Unlock()
		return
	}
	slot.frags[f.Index] = append([]byte(nil), f.Data...)
	slot.have++
	complete := slot.have == int(slot.cnt)
	if complete {
		if slot.token == ([wire.TokenBytes]byte{}) {
			l.tokenless--
		}
		delete(l.reasm, key)
	}
	l.mu.Unlock()
	if !complete {
		return
	}
	msg := make([]byte, 0, slot.total)
	for _, fr := range slot.frags {
		msg = append(msg, fr...)
	}
	l.startServerAttempt(msg, &f, slot, src, dst)
}

// sendRetry emits one stateless RETRY for a tokenless fragment zero
// (section 7.1 fixed-prefix checks; no crypto beyond the token HMAC).
func (l *Listener) sendRetry(f *wire.Fragment, h wire.Header, src, dst netip.AddrPort) {
	d := f.Data
	if len(d) < 90 || d[0] != wire.MsgClient || d[1] != wire.Version {
		return
	}
	if d[4] < 1 || d[4] > 2 || d[5] != 0 {
		return
	}
	if subtle.ConstantTimeCompare(d[12:28], f.AttemptID[:]) != 1 {
		return
	}
	origDCID := h.DestCID
	clientCID := binary.BigEndian.Uint64(d[60:68])
	if clientCID == 0 || origDCID == 0 {
		return
	}
	l.emitRetry(f.AttemptID, f.MessageHash, origDCID, clientCID, src, dst)
}

// emitRetry issues and sends one stateless RETRY, admitted only while
// the global and per-source token buckets allow it (section 6.3).
func (l *Listener) emitRetry(attemptID [wire.AttemptIDBytes]byte,
	hash [32]byte, origDCID, clientCID uint64, src, dst netip.AddrPort) {
	l.mu.Lock()
	now := nowUS()
	if !l.retryGlobal.take(now, retryGlobalPktRate, retryGlobalPktBurst,
		float64(wire.RetryUDPBytes)) {
		l.retryRateLimited++
		l.mu.Unlock()
		return
	}
	sb := l.retrySrc[src.Addr()]
	if sb == nil {
		if len(l.retrySrc) >= retrySrcMax {
			// Spoofed-source flood: the table is bounded and the global
			// bucket still limits emission — refuse the new source rather
			// than resetting buckets that are throttling known sources.
			l.retryRateLimited++
			l.mu.Unlock()
			return
		}
		sb = &tokenBucket{}
		l.retrySrc[src.Addr()] = sb
	}
	ok := sb.take(now, retrySrcPktRate, retrySrcPktBurst,
		float64(wire.RetryUDPBytes))
	if !ok {
		l.retryRateLimited++
		l.mu.Unlock()
		return
	}
	l.retriesSent++
	l.retryBytesSent += wire.RetryUDPBytes
	l.mu.Unlock()
	retryDCID, err := randCID()
	if err != nil {
		return
	}
	tok, ok := l.tokens.IssueRetry(origDCID, retryDCID, hash, src, dst)
	if !ok {
		return
	}
	pkt := wire.BuildRetryDatagram(attemptID, hash, origDCID,
		retryDCID, tok, clientCID)
	_ = l.sock.Send(pkt, src, 0)
}

// startServerAttempt creates the server connection for a complete client
// flight and posts it for S_BUILD processing. Admission first reserves a
// slot in the bounded shared attempt pool (spec sections 20.1/37.6): a
// validated token authorizes the attempt but does not exempt it from the
// bound, so a token flood cannot open unlimited PQXDH/signature work.
// Attempts beyond the bound are dropped — the client's retransmission
// re-enters admission once capacity frees.
func (l *Listener) startServerAttempt(msg []byte, f *wire.Fragment, slot *reasmSlot, src, dst netip.AddrPort) {
	l.mu.Lock()
	if _, dup := l.attempts[f.AttemptID]; dup {
		l.mu.Unlock()
		return // concurrent reassembly completion: attempt already owned
	}
	if l.attemptsInFlight >= l.attemptsBound {
		l.admissionDrops++
		l.mu.Unlock()
		return
	}
	if l.cfg.MaxConnections > 0 && len(l.conns) >= l.cfg.MaxConnections {
		l.admissionDrops++
		l.mu.Unlock()
		return
	}
	srcRate := l.cfg.SourceAttemptRate
	if srcRate <= 0 {
		srcRate = defaultAttemptSrcRate
	}
	srcBurst := l.cfg.SourceAttemptBurst
	if srcBurst <= 0 {
		srcBurst = defaultAttemptSrcBurst
	}
	ab := l.attemptSrc[src.Addr()]
	if ab == nil && len(l.attemptSrc) < retrySrcMax {
		ab = &tokenBucket{}
		l.attemptSrc[src.Addr()] = ab
	}
	if ab == nil || !ab.take(nowUS(), srcRate, srcBurst, 1) {
		l.admissionDrops++
		l.mu.Unlock()
		return
	}
	l.attemptsAdmitted++
	l.attemptsInFlight++
	l.gen++
	gen := l.gen
	l.attempts[f.AttemptID] = nil // placeholder; set after conn build
	l.mu.Unlock()

	sock := &routeSocket{ps: l.sock, local: dst}
	cn := newConn(l.cfg, sock, gen)
	cn.core.attemptHeld = true
	cn.core.role = roleServer
	cn.core.state = stSBuild
	cn.core.hs.cliMsg = msg
	cn.core.hs.attemptID = f.AttemptID
	cn.core.hs.flightHash = f.MessageHash
	cn.core.hs.token = slot.token
	cn.core.hs.hasToken = slot.token != ([wire.TokenBytes]byte{})
	cn.core.hs.retryDCID = slot.dcid
	cn.core.listener = l
	cn.core.paths[0].remote = src
	cn.core.paths[0].local = dst
	cn.core.paths[0].role = pathActive
	cn.core.paths[0].pstate = psActive
	// The admission budget is the datagrams actually received on this
	// attempt/source; a validated token marks the route proven while a
	// tokenless attempt remains amplification-capped (section 6).
	cn.core.paths[0].rxBytes = slot.rxBytes
	cn.core.paths[0].rxPkts = slot.rxPkts
	cn.core.paths[0].validated = slot.token != ([wire.TokenBytes]byte{})
	cn.core.paths[0].initCC()
	cn.core.activePth = 0

	l.mu.Lock()
	l.attempts[f.AttemptID] = cn
	l.mu.Unlock()

	cn.core.arm(tkHandshake, nowUS()+wire.HandshakeDeadlineUS)
	go cn.run()
	cn.post(connEvent{kind: evApp, cmd: &appCmd{kind: acSrvBuild,
		res: make(chan appResult, 1)}})
}

// registerConn installs a conn under its local CIDs for DATA demux.
func (l *Listener) registerConn(cn *Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cn.core.localCID.valid {
		l.conns[cn.core.localCID.cid] = cn
	}
	for i := range cn.core.localCIDs {
		if cn.core.localCIDs[i].valid {
			l.conns[cn.core.localCIDs[i].cid] = cn
		}
	}
}

// removeConn drops all routing entries for a dead conn: local CIDs, any
// attempt-table reference, and is idempotent under listener Close.
func (l *Listener) removeConn(cn *Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cn.core.localCID.valid {
		delete(l.conns, cn.core.localCID.cid)
	}
	for i := range cn.core.localCIDs {
		if cn.core.localCIDs[i].valid {
			delete(l.conns, cn.core.localCIDs[i].cid)
		}
	}
	for id, c := range l.attempts {
		if c == cn {
			delete(l.attempts, id)
		}
	}
}

// routeSocket sends each datagram to the destination the engine asks for
// (per-path remotes), preserving the listener's bound local address.
type routeSocket struct {
	ps    PacketSocket
	local netip.AddrPort
}

func (r *routeSocket) Send(payload []byte, dst netip.AddrPort, ecn uint8) error {
	return r.ps.Send(payload, dst, ecn)
}

// SendFrom forwards source selection when the underlying socket supports
// it; otherwise only the conn's bound local address may send.
func (r *routeSocket) SendFrom(payload []byte, src, dst netip.AddrPort, ecn uint8) error {
	if ss, ok := r.ps.(SourceSocket); ok {
		return ss.SendFrom(payload, src, dst, ecn)
	}
	if src == r.local {
		return r.ps.Send(payload, dst, ecn)
	}
	return errWouldBlock
}

func (r *routeSocket) SendGSO(payload []byte, segmentSize uint16, dst netip.AddrPort, ecn uint8) error {
	return r.SendFromGSO(payload, segmentSize, r.local, dst, ecn)
}

func (r *routeSocket) SendFromGSO(payload []byte, segmentSize uint16, src, dst netip.AddrPort, ecn uint8) error {
	if sgs, ok := r.ps.(SourceGSOSocket); ok {
		return sgs.SendFromGSO(payload, segmentSize, src, dst, ecn)
	}
	if gs, ok := r.ps.(GSOSocket); ok && src == r.local {
		return gs.SendGSO(payload, segmentSize, dst, ecn)
	}
	s := int(segmentSize)
	if s <= 0 {
		return r.SendFrom(payload, src, dst, ecn)
	}
	for len(payload) > 0 {
		chunk := payload
		if len(chunk) > s {
			chunk = chunk[:s]
		}
		if err := r.SendFrom(chunk, src, dst, ecn); err != nil {
			return err
		}
		payload = payload[len(chunk):]
	}
	return nil
}

func (r *routeSocket) LocalAddr() netip.AddrPort { return r.local }

var (
	_ GSOSocket       = (*routeSocket)(nil)
	_ SourceGSOSocket = (*routeSocket)(nil)
)
