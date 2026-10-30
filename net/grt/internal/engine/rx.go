package engine

import (
	"crypto/subtle"
	"net/netip"

	"gosuda.org/grt/internal/wire"
)

// rxPacket dispatches a received datagram by class. It reports true when
// the packet buffer's ownership moved to the deferred-epoch queue; false
// means the caller must release the buffer.
func (c *Conn) rxPacket(e *connEvent) bool {
	core := c.core
	if core.state == stDead || core.state == stQuiescing {
		return false
	}
	h := e.h
	if !e.hasH {
		var ok bool
		h, ok = wire.ParseHeader(e.pkt)
		if !ok {
			// A plausible DATA datagram that fails header parsing may be a
			// peer's stateless reset (section 18/37.5).
			c.maybeStatelessReset(e)
			return false
		}
	}
	switch h.Class {
	case wire.ClassData:
		return c.rxData(e, h)
	case wire.ClassRetry:
		c.rxRetry(e, h)
	case wire.ClassServerFlight:
		c.rxServerFlight(e, h)
	case wire.ClassClientFlight:
		c.rxClientFlightDup(e, h)
	}
	return false
}

// isLocalCID reports whether cid names one of our active local CIDs.
func (c *Conn) isLocalCID(cid uint64) bool {
	for _, r := range c.core.localCIDs {
		if r.valid && r.cid == cid {
			return true
		}
	}
	return false
}

// rxData is the section-22 DATA receive pipeline: header → key-slot →
// replay freshness → AEAD → frame parse → transactional preflight →
// commit. Returns true when the datagram was deferred for a not-yet-
// installed epoch (buffer ownership moved to the queue).
func (c *Conn) rxData(e *connEvent, h wire.Header) bool {
	core := c.core
	if !core.state.keyBearing() {
		return false
	}
	// CID routing check: the destination CID must name this connection.
	if h.DestCID != core.localCID.cid && !c.isLocalCID(h.DestCID) {
		return false // not ours; dispatcher handles reset candidates
	}
	slot := core.keys.find(h.Epoch)
	if slot == nil || slot.state == epochSlotEmpty {
		// A packet under a not-yet-installed epoch is most likely early
		// key-update traffic reordered ahead of the key install: hold it
		// briefly instead of forcing a retransmission round-trip.
		if len(c.deferred) < maxDeferredRX {
			c.deferred = append(c.deferred, rxDgram{
				pkt: e.pkt, src: e.src, dst: e.dst, ecn: e.ecn,
				h: h, rxUS: e.rxUS,
			})
			return true
		}
		c.maybeStatelessReset(e)
		return false // unknown epoch: drop without KDF work (section 4.5)
	}
	if !core.replay.fresh(h.PacketNumber) {
		return false // replay/stale: drop before AEAD work
	}
	payload := e.pkt[wire.HeaderBytes:]
	if len(payload) != int(h.PayloadLength) || len(payload) < wire.TagBytes {
		return false
	}
	if len(payload) > int(core.local.MaxUDPPayload) {
		return false
	}
	// Decrypt into a pooled buffer the stream segments may borrow: runs
	// landing unmerged keep the plaintext in place instead of copying.
	pbuf := GetPacketBuf()
	pt, err := slot.rx.Open(h.PacketNumber, e.pkt[:wire.HeaderBytes], payload, pbuf[:0])
	if err != nil {
		PutPacketBuf(pbuf)
		core.authFails++
		c.maybeStatelessReset(e)
		if core.authFails >= wire.InvalidAuthLimit {
			c.quiesce(&TransportError{Code: codeInternal})
		}
		return false
	}
	var frbuf [wire.MaxFrames]wire.Frame
	frames, eliciting, ferr := wire.ParseFrames(pt, frbuf[:0])
	if ferr != nil {
		PutPacketBuf(pbuf)
		c.closeWith(codeFrameEncoding, 0)
		return false
	}
	// Section 5: the visible ack-eliciting flag must equal the decrypted
	// frame classification; a mismatch is PROTOCOL_VIOLATION.
	if h.AckEliciting != eliciting {
		PutPacketBuf(pbuf)
		c.closeWith(codeProtocolViolation, 0)
		return false
	}
	ref := &ptRef{buf: pbuf}
	c.commitData(e, h, frames, eliciting, slot, ref)
	if ref.n == 0 && ref.buf != nil {
		// No segment borrowed the plaintext and the buffer was not
		// freed mid-commit — it is free again. (n > 0 means ownership
		// moved to recvHalf segs; buf == nil means releaseSeg already
		// returned it when the last borrower died inside this commit.)
		PutPacketBuf(ref.buf)
	}
	return false
}

// maxDeferredRX bounds the unknown-epoch hold queue (section 4.5 allows
// dropping such packets; buffering them is a courtesy, not state that an
// adversary may grow without limit).
const maxDeferredRX = 64

// flushDeferred replays held datagrams whose epoch has since installed,
// in arrival order. Called by the key-install paths.
func (c *Conn) flushDeferred() {
	if len(c.deferred) == 0 || c.deferFlush {
		// Nested flush (a replayed packet installing yet another epoch)
		// skips: the outer pass re-checks each held packet's epoch and
		// replays whatever the nested install just enabled.
		return
	}
	c.deferFlush = true
	defer func() { c.deferFlush = false }()
	rest := c.deferred[:0]
	for i := range c.deferred {
		d := c.deferred[i]
		if slot := c.core.keys.find(d.h.Epoch); slot != nil && slot.state != epochSlotEmpty {
			e := connEvent{kind: evRX, pkt: d.pkt, src: d.src, dst: d.dst,
				ecn: d.ecn, rxUS: d.rxUS, h: d.h, hasH: true}
			if !c.rxPacket(&e) {
				PutPacketBuf(d.pkt)
			}
		} else {
			rest = append(rest, d)
		}
	}
	c.deferred = rest
}

// matchPath returns the index of the path record whose full tuple
// (remote, local) matches the datagram's (src, dst), or -1. A change in
// either endpoint names a different path (spec section 17.1).
//
// A path record whose local address is still unspecified (a wildcard-
// bound socket whose canonical local endpoint has not been observed)
// matches a datagram arriving on the bound port and pins the observed
// destination as its local endpoint (section 6.2). Exact matches are
// resolved first so a wildcard record can never shadow a concrete tuple.
func (c *Conn) matchPath(src, dst netip.AddrPort) int {
	wild := -1
	for i := range c.core.paths {
		p := &c.core.paths[i]
		if p.role == pathFree || p.remote != src {
			continue
		}
		if p.local == dst {
			return i
		}
		if wild < 0 && p.local.Addr().IsUnspecified() &&
			p.local.Port() == dst.Port() {
			wild = i
		}
	}
	if wild >= 0 {
		c.core.paths[wild].local = dst
		return wild
	}
	return -1
}

// commitData runs the transactional preflight and commit for an
// authenticated DATA packet. ref owns the decrypted backing store the
// frames point into; stream payloads may borrow it via recvHalf segs.
func (c *Conn) commitData(e *connEvent, h wire.Header, frames []wire.Frame, eliciting bool, slot *epochSlot, ref *ptRef) {
	core := c.core
	// The OnDatagram arrival stamp doubles as the processing stamp;
	// RTT sampling is more accurate against it than a re-read clock.
	now := e.rxUS
	if now == 0 {
		now = nowUS()
	}

	// Path resolution: match the ingress tuple to a path record. A
	// peer-discovered candidate is minted only at the commit point
	// below — a dropped transaction must not create path state, and
	// section 6 forbids processing migration before handshake
	// confirmation.
	pi := c.matchPath(e.src, e.dst)

	// Staged-epoch admission (section 19.4): a packet under an installed
	// but non-current epoch is legal only while a refresh transaction is
	// open for that epoch. Server side it must carry a valid KEY_COMMIT;
	// client side (during COMMITTING) a valid KEY_CONFIRM. Anything else
	// is dropped without ACK or replay commit.
	if !c.stagedGate(slot, frames) {
		return
	}

	// --- Preflight: validate the complete frame set before any commit ---
	var pf rxPreflight
	if code := c.preflight(frames, &pf, eliciting); code != codeNoError {
		c.closeWith(code, pf.offending)
		return
	}
	if core.state == stSFinishing && !pf.hasFinish {
		return // noncommitting drop: no FINISH, no transaction (section 20.3)
	}

	// --- Commit ---
	if pi < 0 {
		if !c.createCandidate(e) {
			return // no candidate capacity: drop whole transaction
		}
		pi = c.matchPath(e.src, e.dst)
		if pi < 0 {
			return
		}
	}
	p := &core.paths[pi]
	largest, hadLargest := core.replay.largestObserved()
	core.replay.commit(h.PacketNumber)
	p.rxBytes += uint64(len(e.pkt))
	p.rxPkts++
	if h.PacketNumber > p.peerPN || !p.peerSet {
		p.peerPN, p.peerSet = h.PacketNumber, true
	}
	if eliciting {
		// Committed-DATA ECN counters are connection-level and continue
		// across migration (section 15.4).
		switch e.ecn {
		case 2:
			core.rxECT[0]++
		case 3:
			core.rxECT[2]++
		case 1:
			core.rxECT[1]++
		}
		// Section 13.2 immediate triggers: a newly observed gap, a newly
		// filled gap or new out-of-order range (any commit other than
		// largest+1), handshake/key progress, a path challenge requiring
		// response, or a newly committed CE-marked eliciting packet.
		imm := pf.immediateAck || e.ecn == 3 ||
			(hadLargest && h.PacketNumber != largest+1)
		// The deadline uses our own advertised max_ack_delay — the bound
		// we promised the peer, not the peer's. The ingress path's RTT
		// estimate drives the adaptive ratio's idle reset (section 13.2).
		core.acker.onEliciting(now, p.srtt, uint64(core.local.MaxAckDelayUS), imm)
	}
	core.lastActivityUS = now
	if pf.nonProbing {
		// Fresh non-probing evidence on a candidate (section 17.4).
		if h.PacketNumber > p.evidencePN {
			p.evidencePN = h.PacketNumber
		}
	}

	// Apply committed semantic work.
	core.lastPeerPN = h.PacketNumber
	c.applyFrames(frames, &pf, e, now, pi, ref)

	if pf.nonProbing && p.role == pathCandidate && p.pstate == psReady {
		c.checkImplicitPromotion(pi)
	}
	if core.state == stSFinishing && pf.finishOK {
		core.state = stEstablished
		c.releaseAttempt()
		// A valid FINISH proves the client received our flight: the
		// cached response and the client's flight/reassembly buffers are
		// released at their confirmation fence, and any later bootstrap
		// duplicates are dropped (section 9.4/20.3).
		core.hs.serverFlight = nil
		core.hs.flight.frags = nil
		core.disarm(tkFlight)
		core.hs.cliMsg = nil
		core.hs.cliFrags = nil
		c.signalConfirmed()
		c.postEstablish()
	}
	if core.state == stCFinishing && pf.handshakeDoneOK {
		core.state = stEstablished
		// Same confirmation fence: the client flight and its emitter
		// state are no longer needed once the handshake is done.
		core.hs.clientMsg = nil
		core.hs.clientFrags = nil
		core.hs.flight.frags = nil
		core.disarm(tkFlight)
		c.signalConfirmed()
		c.postEstablish()
	}
}

// stagedGate enforces the section-19.4 staged-epoch admission rule.
// Returns false when the packet must be dropped without acknowledgment.
func (c *Conn) stagedGate(slot *epochSlot, frames []wire.Frame) bool {
	core := c.core
	if slot.state != epochSlotInstalled || core.keys.isCurrent(slot) {
		return true // CURRENT and PREVIOUS epochs carry ordinary traffic
	}
	rt := &core.refresh
	if !rt.active || slot.epoch != rt.epoch {
		return false // staged epoch outside a matching transaction
	}
	if core.role == roleServer {
		// Pre-commit STAGED packets must carry the valid KEY_COMMIT
		// (epoch/ID mismatches inside a commit are KEY_UPDATE_ERROR).
		for i := range frames {
			if frames[i].Type != wire.FrameKeyCommit {
				continue
			}
			epoch, id, mac, ok := wire.ParseKeyCommit(frames[i].Body)
			if !ok {
				return false
			}
			if epoch != rt.epoch || id != rt.id {
				c.closeWith(codeKeyUpdate, wire.FrameKeyCommit)
				return false
			}
			if subtle.ConstantTimeCompare(mac[:], rt.commitMAC[:]) != 1 {
				return false // not a valid commit: drop without ACK
			}
			return true
		}
		return false
	}
	// Client during COMMITTING accepts a staged-epoch packet only with a
	// valid KEY_CONFIRM.
	if !rt.commitSent || rt.confirmed {
		return false
	}
	for i := range frames {
		if frames[i].Type != wire.FrameKeyConfirm {
			continue
		}
		epoch, id, mac, ok := wire.ParseKeyCommit(frames[i].Body)
		if !ok {
			return false
		}
		if epoch != rt.epoch || id != rt.id {
			c.closeWith(codeKeyUpdate, wire.FrameKeyConfirm)
			return false
		}
		if subtle.ConstantTimeCompare(mac[:], rt.confirmMAC[:]) != 1 {
			return false
		}
		return true
	}
	return false
}

// rxPreflight collects tentative validation results for a transaction.
type rxPreflight struct {
	connInc         uint64 // total connection high-water increment
	offending       uint16 // frame type for errors
	hasFinish       bool
	finishOK        bool
	handshakeDoneOK bool
	immediateAck    bool
	nonProbing      bool // a non-probing frame for section-17.4 evidence
	// streams opened inside this packet (tentative); a packet can carry
	// up to MaxFrames OPENs, so the set must cover every one of them
	opened  [wire.MaxFrames]uint64
	nOpened int
	// per-stream tentative receive high-water inside this transaction:
	// charging must follow frame order, so a second frame for the same
	// stream measures its increment above the earlier frame's end, not
	// the committed high-water. fin records a declared final size staged
	// by a same-packet FIN or RESET_STREAM.
	incs [wire.MaxFrames]struct {
		sid uint64
		hi  uint64
		fin bool
	}
	nIncs int
	// ACK ranges validated against the ledger
	ackOK bool
}

// probingOnlyType reports whether a frame type is "probing" for the
// section-17.4 implicit-promotion evidence rule: ACK, PADDING,
// PATH_CHALLENGE/RESPONSE, probe-only PATH_INTENT, NEW_CID, RETIRE_CID,
// NEW_TOKEN, and mobile lease control are probing; everything else —
// PING included — is non-probing evidence.
func probingOnlyType(t uint16) bool {
	switch t {
	case wire.FrameAck, wire.FramePadding,
		wire.FramePathChallenge, wire.FramePathResponse,
		wire.FrameNewCID, wire.FrameRetireCID, wire.FrameNewToken,
		wire.FrameParkRequest, wire.FrameParkGrant,
		wire.FrameParkWake, wire.FrameParkAwake:
		return true
	}
	return false
}

// stagedHigh returns sid's tentative receive high-water inside this
// transaction: the largest end staged so far in frame order, or the
// committed high-water when no same-packet frame touched the stream.
func (pf *rxPreflight) stagedHigh(sid uint64, committed uint64) uint64 {
	for i := 0; i < pf.nIncs; i++ {
		if pf.incs[i].sid == sid {
			return pf.incs[i].hi
		}
	}
	return committed
}

// stagedFinal returns sid's declared final size staged inside this
// transaction by an earlier FIN or RESET_STREAM, if any.
func (pf *rxPreflight) stagedFinal(sid uint64) (uint64, bool) {
	for i := 0; i < pf.nIncs; i++ {
		if pf.incs[i].sid == sid && pf.incs[i].fin {
			return pf.incs[i].hi, true
		}
	}
	return 0, false
}

// stageEnd raises sid's tentative high-water to end; a FIN or RESET
// additionally pins the declared final size. Capacity equals MaxFrames —
// the maximum chargeable frames in one packet — so it cannot overflow.
func (pf *rxPreflight) stageEnd(sid uint64, end uint64, fin bool) {
	for i := 0; i < pf.nIncs; i++ {
		e := &pf.incs[i]
		if e.sid != sid {
			continue
		}
		if end > e.hi {
			e.hi = end
		}
		if fin {
			e.fin = true
		}
		return
	}
	if pf.nIncs >= len(pf.incs) {
		return // unreachable: a packet carries at most MaxFrames frames
	}
	e := &pf.incs[pf.nIncs]
	e.sid, e.hi, e.fin = sid, end, fin
	pf.nIncs++
}

// preflight validates every frame semantically before commit. It writes
// staged increments into pf and returns a close code (0 = ok).
func (c *Conn) preflight(frames []wire.Frame, pf *rxPreflight, eliciting bool) closeCode {
	core := c.core
	// Packet-composition rules (section 11): a CLOSE packet contains exactly
	// one CLOSE and no PADDING; an MTU_PROBE packet may contain only
	// MTU_PROBE, PING, and PADDING; PARK_AWAKE is never coalesced with
	// application frames; a PADDING-bearing packet must be ack-eliciting.
	var hasClose, hasMTU, hasAwake, hasPad bool
	for i := range frames {
		switch frames[i].Type {
		case wire.FrameClose:
			hasClose = true
		case wire.FrameMTUProbe:
			hasMTU = true
		case wire.FrameParkAwake:
			hasAwake = true
		case wire.FramePadding:
			hasPad = true
		}
	}
	if hasClose && len(frames) != 1 {
		return codeProtocolViolation
	}
	if hasMTU {
		for i := range frames {
			switch frames[i].Type {
			case wire.FrameMTUProbe, wire.FramePing, wire.FramePadding:
			default:
				return codeProtocolViolation
			}
		}
	}
	if hasPad && !eliciting {
		return codeProtocolViolation
	}
	if hasAwake {
		for i := range frames {
			if appFrameType(frames[i].Type) {
				return codeProtocolViolation
			}
		}
	}
	var seen [wire.SingletonCount]bool
	for i := range frames {
		f := &frames[i]
		if idx, ok := wire.SingletonIndex(f.Type); ok {
			if seen[idx] {
				pf.offending = f.Type
				return codeProtocolViolation
			}
			seen[idx] = true
		}
		if !probingOnlyType(f.Type) {
			// A probe-only PATH_INTENT (SELECT=0) stays probing; parsed
			// flags decide.
			if f.Type != wire.FramePathIntent {
				pf.nonProbing = true
			}
		}
		if code := c.preflightFrame(f, pf); code != codeNoError {
			pf.offending = f.Type
			return code
		}
	}
	// Connection flow check: aggregate unique increments (section 12.4).
	if core.connRxUsed+pf.connInc > core.connRxLim {
		return codeFlowControl
	}
	return codeNoError
}

// appFrameType reports whether a frame carries application semantics and
// therefore may not share a packet with PARK_AWAKE (section 35).
func appFrameType(t uint16) bool {
	switch t {
	case wire.FrameStreamOpen, wire.FrameStream, wire.FrameResetStream,
		wire.FrameStopSending, wire.FrameMaxData, wire.FrameMaxStreamData,
		wire.FrameMaxSlots, wire.FrameDataBlocked, wire.FrameStreamBlocked,
		wire.FrameSlotsBlocked, wire.FrameStreamRetire, wire.FrameRetireAck,
		wire.FrameDatagram, wire.FrameNewTicket:
		return true
	}
	return false
}

// preflightFrame validates one frame against (tentative) state.
func (c *Conn) preflightFrame(f *wire.Frame, pf *rxPreflight) closeCode {
	core := c.core
	switch f.Type {
	case wire.FrameStreamOpen:
		sid64, ok := wire.ParseU64(f.Body)
		if !ok || !wire.StreamID(sid64).Valid() {
			return codeFrameEncoding
		}
		sid := wire.StreamID(sid64)
		code := c.preflightOpen(sid, pf)
		if code != codeNoError {
			return code
		}
	case wire.FrameStream:
		sid64, off, data, ok := wire.ParseStreamBody(f.Body)
		if !ok || !wire.StreamID(sid64).Valid() {
			return codeFrameEncoding
		}
		fin := f.Flags&wire.FrameFlagStreamFin != 0
		if len(data) == 0 && !fin {
			return codeFrameEncoding
		}
		if off > wire.MaxDataValue || off+uint64(len(data)) > wire.MaxDataValue {
			return codeFinalSize
		}
		return c.preflightStreamData(wire.StreamID(sid64), off, uint64(len(data)), fin, pf)
	case wire.FrameAck:
		a, ok := wire.ParseAckBody(f.Body)
		if !ok {
			return codeFrameEncoding
		}
		return c.preflightAck(&a)
	case wire.FrameClose:
		_, _, _, ok := wire.ParseClose(f.Body)
		if !ok {
			return codeFrameEncoding
		}
		// Packet-composition rule checked in preflight.
	case wire.FrameFinish:
		if _, _, ok := wire.ParseFinish(f.Body); !ok {
			return codeFrameEncoding
		}
		pf.hasFinish = true
		pf.immediateAck = true // handshake progress
	case wire.FrameHandshakeDone:
		if _, ok := wire.ParseHandshakeDone(f.Body); !ok {
			return codeFrameEncoding
		}
		pf.immediateAck = true // handshake progress
	case wire.FramePing:
		// PING is eliciting but not an immediate-ack trigger; it must not
		// clear a trigger an earlier frame in the same packet set.
	case wire.FramePadding:
		// eliciting marker only
	case wire.FrameResetStream:
		sid64, final, _, ok := wire.ParseResetStream(f.Body)
		if !ok || !wire.StreamID(sid64).Valid() || final > wire.MaxDataValue {
			return codeFrameEncoding
		}
		return c.preflightReset(wire.StreamID(sid64), final, pf)
	case wire.FrameStopSending:
		sid64, _, ok := wire.ParseStopSending(f.Body)
		if !ok || !wire.StreamID(sid64).Valid() {
			return codeFrameEncoding
		}
		return c.preflightStopSending(wire.StreamID(sid64))
	case wire.FrameMaxData:
		v, ok := wire.ParseU64(f.Body)
		if !ok || v > wire.MaxDataValue {
			return codeFrameEncoding
		}
	case wire.FrameMaxStreamData:
		sid64, lim, ok := wire.ParseMaxStreamData(f.Body)
		if !ok || lim > wire.MaxDataValue || !wire.StreamID(sid64).Valid() {
			return codeFrameEncoding
		}
		return c.preflightMaxStreamData(wire.StreamID(sid64), pf)
	case wire.FrameMaxSlots:
		class, lim, ok := wire.ParseMaxSlots(f.Body)
		if !ok || class > 3 || lim > wire.MaxStreamSlots {
			return codeFrameEncoding
		}
		// Only classes the local endpoint initiated may be granted.
		if !classOurs(class, core.role) {
			return codeStreamState
		}
	case wire.FrameStreamRetire:
		sid64, ok := wire.ParseU64(f.Body)
		if !ok || !wire.StreamID(sid64).Valid() {
			return codeFrameEncoding
		}
		return c.preflightRetire(wire.StreamID(sid64))
	case wire.FrameRetireAck:
		sid64, ok := wire.ParseU64(f.Body)
		if !ok || !wire.StreamID(sid64).Valid() {
			return codeFrameEncoding
		}
		return c.preflightRetireAck(wire.StreamID(sid64))
	case wire.FrameDatagram:
		ctx, _, ok := wire.ParseDatagram(f.Body)
		if !ok || ctx != 0 {
			return codeFrameEncoding
		}
		if core.peer.FeatureFlags&wire.FeatureDatagram == 0 {
			return codeProtocolViolation
		}
	case wire.FrameNewCID:
		seq, _, cid, _, ok := wire.ParseNewCID(f.Body)
		if !ok || cid == 0 {
			return codeFrameEncoding
		}
		_ = seq
	case wire.FrameRetireCID:
		seq, ok := wire.ParseU64(f.Body)
		if !ok {
			return codeFrameEncoding
		}
		_ = seq
	case wire.FramePathChallenge:
		if _, ok := wire.Parse16(f.Body); !ok {
			return codeFrameEncoding
		}
		pf.immediateAck = true // path challenge requires response
	case wire.FramePathResponse:
		if _, ok := wire.Parse16(f.Body); !ok {
			return codeFrameEncoding
		}
	case wire.FramePathIntent:
		_, _, flags, ok := wire.ParsePathIntent(f.Body)
		if !ok {
			return codeFrameEncoding
		}
		if flags&1 != 0 {
			pf.nonProbing = true // SELECT is non-probing evidence
		}
	case wire.FramePathSelected:
		_, _, ok := wire.ParsePathSelected(f.Body)
		if !ok {
			return codeFrameEncoding
		}
	case wire.FrameCryptoFragment:
		cf, ok := wire.ParseCryptoFrag(f.Body)
		if !ok {
			return codeFrameEncoding
		}
		pf.immediateAck = true // key progress
		return c.preflightCryptoFrag(&cf)
	case wire.FrameKeyCommit:
		_, _, _, ok := wire.ParseKeyCommit(f.Body)
		if !ok {
			return codeFrameEncoding
		}
		pf.immediateAck = true // key progress
	case wire.FrameKeyConfirm:
		if _, _, _, ok := wire.ParseKeyCommit(f.Body); !ok {
			return codeFrameEncoding
		}
		pf.immediateAck = true // key progress
	case wire.FrameKeyRequest:
		if _, ok := wire.ParseKeyRequest(f.Body); !ok {
			return codeFrameEncoding
		}
	case wire.FrameMTUProbe:
		// no semantic state
	case wire.FrameNewTicket:
		if _, _, ok := wire.ParseNewTicket(f.Body); !ok {
			return codeFrameEncoding
		}
	case wire.FrameNewToken:
		if _, ok := wire.ParseNewToken(f.Body); !ok {
			return codeFrameEncoding
		}
	case wire.FrameDataBlocked, wire.FrameStreamBlocked, wire.FrameSlotsBlocked:
		// advisory
	case wire.FrameParkRequest:
		if _, _, ok := wire.ParseParkRequest(f.Body); !ok {
			return codeFrameEncoding
		}
	case wire.FrameParkGrant:
		if _, _, _, ok := wire.ParseParkGrant(f.Body); !ok {
			return codeFrameEncoding
		}
	case wire.FrameParkWake:
		if _, _, ok := wire.ParseParkWake(f.Body); !ok {
			return codeFrameEncoding
		}
	case wire.FrameParkAwake:
		if _, _, ok := wire.ParseParkWake(f.Body); !ok {
			return codeFrameEncoding
		}
	default:
		// ParseFrames already enforced critical/skippable policy.
	}
	return codeNoError
}

// classOurs reports whether a MAX_SLOTS class is one the peer may grant us:
// the class's initiator bit must be ours.
func classOurs(class uint8, r role) bool {
	init := class & 1
	return (r == roleClient && init == wire.StreamInitiatorClient) ||
		(r == roleServer && init == wire.StreamInitiatorServer)
}
