package engine

import (
	"encoding/binary"

	"gosuda.org/grt/internal/wire"
)

// The TX pipeline follows spec section 22: RESERVE (PN + budgets) → build →
// seal → SUBMIT → EMIT_COMMIT → ledger.track. The portable socket adapter
// fuses SUBMIT/ACCEPT: Send returning nil is the emission commit.

// txBudget tracks one packet's reservations during construction.
type txBudget struct {
	items       []lossItem
	elicit      bool
	ackSent     bool   // an ACK frame was appended; acker resets on commit
	streamBytes uint64 // new conn-level increments this packet reserved
	probe       bool   // carries PATH_CHALLENGE/PATH_RESPONSE: pad to base size
	appData     bool   // carries a STREAM_OPEN/STREAM/DATAGRAM frame
	blockedEmit bool   // a DATA_BLOCKED frame was appended (flag consumed)
	flowLimited bool   // stream/conn credit stopped emission with work left
}

// txSchedule is the section-23 step-9 TX pass.
func (c *Conn) txSchedule() {
	core := c.core
	now := nowUS()

	switch core.state {
	case stDead, stQuiescing, stDraining:
		return
	case stClosing:
		c.txClose(now)
		return
	case stCBuild, stCWaitReply:
		c.txClientFlight(now)
		return
	case stSFinishing:
		// The server flight emits under its own paced/amplified envelope
		// (section 6.4) while HANDSHAKE_DONE/ACKs continue on DATA.
		c.txServerFlight(now)
	}
	if !core.state.keyBearing() {
		return
	}
	slot := core.keys.current()
	_, hasGSO := core.sock.(GSOSocket)
	c.txBatching = hasGSO && slot != nil && c.qsend == nil
	// Elicit packets until out of work or budget.
	for i := 0; i < wire.MaxLocalBatchDatagrams; i++ {
		if !c.txDataPacket(now) {
			break
		}
	}
	if c.txBatching {
		c.flushTxBatch(slot)
		c.txBatching = false
	}
	// Path-pinned progress on non-active paths: pending challenges,
	// responses, and intents addressed to a candidate/OLD path emit
	// there, never on the default route (section 17.3).
	for pi := range core.paths {
		if pi == core.activePth || core.paths[pi].role == pathFree {
			continue
		}
		for c.hasPathWork(pi) && c.txPathPacket(now, pi) {
		}
	}
	// A PARK_AWAKE obligation is a dedicated packet on the recorded wake
	// path (section 35.6): no coalesced application frames.
	if core.awakePend && core.wakePath >= 0 {
		c.txAwake(now)
	}
	// PMTU probing on the active path (section 17.2).
	if core.state == stEstablished {
		c.txMTUProbe(now)
	}
	c.updateLossTimer()
	// ACK deadline arming.
	if core.acker.dueValid && !core.armed[tkAckDelay] {
		core.arm(tkAckDelay, core.acker.dueUS)
	}
}

// txClose emits a CLOSE-only packet at most once per base PTO.
func (c *Conn) txClose(now uint64) {
	core := c.core
	if core.closeSent && now < core.closeSentUS+c.ptoUS() {
		return
	}
	slot := core.keys.current()
	if slot == nil {
		c.quiesce(nil)
		return
	}
	fb := wire.NewFrameBuilder(c.frameScratch())
	var body [24]byte
	wire.BuildClose(body[:], uint32(core.closeCode), 0, c.diagHash())
	fb.Add(wire.FrameClose, 0, len(body), func(d []byte) { copy(d, body[:]) })
	c.emit(slot, core.activePth, fb, nil, now, 0)
	core.closeSent, core.closeSentUS = true, now
}

// diagHash is the local diagnostic hash carried by CLOSE (16 bytes).
func (c *Conn) diagHash() (h [16]byte) {
	if te, ok := c.core.termErr.(*TransportError); ok {
		copy(h[:], te.Diag[16:])
	}
	return
}

// txDataPacket builds and emits one DATA packet; returns false when no
// permitted work remains.
func (c *Conn) txDataPacket(now uint64) bool {
	core := c.core
	pi := core.activePth
	p := &core.paths[pi]

	// Client COMMITTING obligation (section 19.4): a staged-epoch packet
	// carries only KEY_COMMIT plus ACK/PING/PADDING and is emitted on its
	// own before ordinary current-epoch traffic.
	if c.wantStagedCommit(now) {
		return c.txStagedCommit(now)
	}

	slot := core.keys.current()
	if slot == nil {
		return false
	}
	// AEAD hard-limit admission (section 4.5).
	if !sealAdmits(slot, 1, uint64(p.m)) {
		// At the hard limit: only refresh/control progress may continue.
		if !c.refreshInFlight() {
			c.initiateKeyRefresh()
		}
		if !c.refreshInFlight() {
			c.quiesce(&TransportError{Code: codeAEADLimit})
			return false
		}
	}

	// Whether any non-ACK work is pending decides if this packet can be
	// ack-eliciting; ACK/padding-only packets bypass the cwnd and pacing
	// gates but never the anti-amplification gate.
	mayElicit := c.txPending(pi)
	ackDue := core.armed[tkAckDelay] && now >= core.acker.dueUS
	wantAck := core.acker.wantAck(now) || ackDue
	if !mayElicit && !wantAck {
		return false
	}

	// Gates are evaluated BEFORE any frames are built: the frame emitters
	// consume send-half/queue bookkeeping as they append, so a packet that
	// gets gated out must not have been built at all. The size used for
	// admission is the pessimistic path bound.
	estSize := uint64(p.m)
	ptoProbe := false
	if mayElicit {
		allow := p.inFlight+estSize <= p.cwnd
		if !allow && core.probeBudget > 0 {
			allow = true // explicit PTO probe allowance
			ptoProbe = true
		}
		if !allow || !p.paceAllow(now, estSize) || !c.migAllow(now, p, estSize) {
			if !allow {
				c.maybeBlocked()
			}
			if !p.paceAllow(now, estSize) {
				core.arm(tkPacer, now+p.paceWaitUS(now, estSize))
			} else if !c.migAllow(now, p, estSize) {
				core.arm(tkPacer, now+1000)
			}
			if wantAck {
				mayElicit = false
			} else {
				return false
			}
		}
	}
	if !p.ampAllow(estSize) {
		return false
	}
	if mayElicit && core.ledger.liveFull() {
		if wantAck {
			mayElicit = false
		} else {
			return false
		}
	}
	if core.nextPN > wire.MaxPacketNumber {
		c.quiesce(&TransportError{Code: codeInternal})
		return false
	}

	var tb txBudget
	tb.items = c.itemScratch[:0]
	core.txBuildID++
	fb := wire.NewFrameBuilder(c.frameScratch())
	maxPlain := int(p.m) - wire.HeaderBytes - wire.TagBytes
	if maxPlain < 64 {
		return false
	}

	// --- Priority 1: handshake/key/path progress ---
	c.emitProgressFrames(fb, &tb, maxPlain, pi)

	// --- Priority 2: ready ACK ---
	if wantAck {
		c.emitAck(fb, &tb, now)
	}

	// --- Priority 3: reliable credit/control ---
	if mayElicit {
		c.emitCtrl(fb, &tb, maxPlain, pi)
	}

	// A parked connection suspends application emission; control and ACK
	// progress still flow (section 35).
	appOK := !core.parked && mayElicit

	// --- Priority 4/5: stream retransmit then new data ---
	if appOK {
		c.emitStreamData(fb, &tb, maxPlain)

		// --- Priority 6: datagrams ---
		c.emitDatagrams(fb, &tb, maxPlain)
	}

	// Frame emission is the bookkeeping commit point: an empty builder
	// means nothing was mutated.
	if fb.Len() == 0 {
		return false
	}
	// Section-16 control allowance: a packet carrying no application
	// traffic while application work is eligible may emit only when it
	// serves an excepted purpose — an ACK deadline, handshake
	// completion, path validation, key-commit progress, a PTO probe, or
	// CLOSE — or when one control-only packet per four emitted DATA
	// packets is available. A denied packet's bookkeeping is restored so
	// its items ride the next emitted packet instead.
	limited := false
	if !tb.appData && c.appWorkPending() && !ptoProbe &&
		!c.ctrlOnlyExempt(&tb, ackDue) {
		if core.ctrlCredit < 4 {
			c.txAbort(&tb)
			return false
		}
		limited = true
	}
	// Probe-carrying packets pad to base size so a challenge/response
	// exchange supplies full-sized evidence in both directions
	// (section 17.3); the amp gate above already admitted p.m bytes.
	if tb.probe {
		fb.Pad(wire.BaseUDPPayload - wire.HeaderBytes - wire.TagBytes)
	}
	if c.emit(slot, pi, fb, &tb, now, 0) {
		if limited {
			core.ctrlCredit -= 4
		}
		return true
	}
	return false
}

// appWorkPending reports whether application traffic is eligible to
// send: queued stream bytes, retransmissions, pending FINs, or outbound
// datagrams. Parked application work is not eligible (section 35).
func (c *Conn) appWorkPending() bool {
	core := c.core
	if core.parked {
		return false
	}
	for class := uint8(0); class < 4; class++ {
		for _, s := range core.slots.slots[class] {
			if s.state != slotActive || s.send == nil {
				continue
			}
			sh := s.send
			if !sh.unsent.empty() || !sh.retx.empty() ||
				(sh.finPending && !sh.finEmitted) {
				return true
			}
		}
	}
	return len(core.dgOut) > 0
}

// ctrlOnlyExempt reports whether a packet built without application
// traffic serves an excepted section-16 purpose: an ACK deadline,
// handshake completion (FINISH/HANDSHAKE_DONE), path validation or
// selection, key-refresh progress, or CLOSE.
func (c *Conn) ctrlOnlyExempt(tb *txBudget, ackDue bool) bool {
	if tb.ackSent {
		return true
	}
	for i := range tb.items {
		li := &tb.items[i]
		if li.kind != itemCtrl || int(li.ctrlID) >= len(c.core.ctrl) {
			continue
		}
		it := &c.core.ctrl[li.ctrlID]
		if !it.used {
			continue
		}
		switch it.kind {
		case ctrlFinish, ctrlHandshakeDone,
			ctrlPathChallenge, ctrlPathResponse,
			ctrlPathIntent, ctrlPathSelected,
			ctrlKeyCommit, ctrlKeyConfirm, ctrlKeyRequest, ctrlCryptoFrag,
			ctrlParkRequest, ctrlParkGrant, ctrlParkWake, ctrlParkAwake,
			ctrlClose:
			return true
		}
	}
	return false
}

// txAbort restores logical work consumed by a packet that will not
// emit: queued items requeue as on loss and consumed advisory flags
// return.
func (c *Conn) txAbort(tb *txBudget) {
	if tb == nil {
		return
	}
	for i := range tb.items {
		c.itemLost(&tb.items[i])
	}
	if tb.blockedEmit {
		c.core.wantDataBlocked = true
	}
}

// wantStagedCommit reports whether the client owes a KEY_COMMIT packet
// under the STAGED epoch now (first send or paced retransmission).
func (c *Conn) wantStagedCommit(now uint64) bool {
	rt := &c.core.refresh
	return c.core.role == roleClient && rt.active && rt.commitSent &&
		!rt.confirmed && now >= rt.commitNextUS && c.commitItem() >= 0
}

// commitItem returns the pending KEY_COMMIT item index for the live
// transaction, or -1.
func (c *Conn) commitItem() int {
	rt := &c.core.refresh
	for i := range c.core.ctrl {
		it := &c.core.ctrl[i]
		if it.used && it.kind == ctrlKeyCommit && it.tok == rt.id {
			return i
		}
	}
	return -1
}

// txStagedCommit emits one STAGED-epoch packet carrying KEY_COMMIT and at
// most ACK/PING/PADDING (section 19.4). Application traffic never rides
// the staged epoch.
func (c *Conn) txStagedCommit(now uint64) bool {
	core := c.core
	staged := core.keys.staged()
	if staged == nil {
		return false
	}
	id := c.commitItem()
	if id < 0 {
		return false
	}
	pi := core.activePth
	p := &core.paths[pi]
	estSize := uint64(p.m)
	allow := p.inFlight+estSize <= p.cwnd
	if !allow && core.probeBudget > 0 {
		allow = true
	}
	if !allow || !p.paceAllow(now, estSize) || !p.ampAllow(estSize) ||
		core.ledger.liveFull() {
		return false
	}
	var tb txBudget
	tb.items = c.itemScratch[:0]
	core.txBuildID++
	fb := wire.NewFrameBuilder(c.frameScratch())
	c.emitCtrlItem(fb, &tb, uint32(id))
	if core.acker.wantAck(now) {
		c.emitAck(fb, &tb, now)
	}
	if fb.Len() == 0 {
		return false
	}
	// Next paced retransmission if this one is not confirmed.
	rt := &core.refresh
	rt.commitNextUS = now + 4*c.ptoUS()
	c.armRefreshPace()
	return c.emit(staged, pi, fb, &tb, now, 0)
}

// armRefreshPace schedules the refresh timer for the earlier of the
// transaction deadline and the next commit retransmission.
func (c *Conn) armRefreshPace() {
	rt := &c.core.refresh
	d := rt.deadline
	if rt.commitSent && !rt.confirmed && rt.commitNextUS < d {
		d = rt.commitNextUS
	}
	c.core.arm(tkRefresh, d)
}

// hasPathWork reports whether a pending control item is pinned to path pi.
func (c *Conn) hasPathWork(pi int) bool {
	for i := range c.core.ctrl {
		it := &c.core.ctrl[i]
		if it.used && !it.live && int(it.path) == pi {
			return true
		}
	}
	return false
}

// txPathPacket emits one bounded packet on non-active path pi carrying
// only path-pinned progress items plus a due ACK (section 17.3: before
// READY only validation/ACK/key-progress frames leave on a candidate).
// Challenge/response packets pad toward 1200 when allowance permits.
func (c *Conn) txPathPacket(now uint64, pi int) bool {
	core := c.core
	p := &core.paths[pi]
	slot := core.keys.current()
	if slot == nil {
		return false
	}
	// Budget: anti-amplification until validated, plus pacing. Probing
	// datagrams are padded toward base size when allowance permits. A
	// locally initiated probe is authorized by the persistent probe
	// bucket spent in onPathDeadline, not by ingress credit (section
	// 17.6) — a fresh candidate has none.
	want := uint64(wire.BaseUDPPayload)
	probeOK := p.localInit && !p.validated
	if !p.ampAllow(want) {
		if !probeOK {
			want = 96 // minimal challenge/response datagram under tight budget
		}
	}
	if !p.ampAllow(want) && !probeOK {
		return false
	}
	var tb txBudget
	tb.items = c.itemScratch[:0]
	core.txBuildID++
	fb := wire.NewFrameBuilder(c.frameScratch())
	for i := range core.ctrl {
		it := &core.ctrl[i]
		if !it.used || it.live || int(it.path) != pi {
			continue
		}
		c.emitCtrlItem(fb, &tb, uint32(i))
	}
	if fb.Len() == 0 {
		return false
	}
	// Pad challenge/response datagrams to base size when the budget
	// already admitted it (section 17.3).
	if want >= wire.BaseUDPPayload {
		target := int(wire.BaseUDPPayload) - wire.HeaderBytes - wire.TagBytes
		fb.Pad(target)
	}
	return c.emit(slot, pi, fb, &tb, now, 0)
}

// txAwake emits the dedicated PARK_AWAKE packet on the recorded wake path
// (section 35.6: never coalesced with application frames).
func (c *Conn) txAwake(now uint64) {
	core := c.core
	pi := core.wakePath
	if pi < 0 || pi >= len(core.paths) || core.paths[pi].role == pathFree {
		pi = core.activePth
	}
	slot := core.keys.current()
	if slot == nil {
		return
	}
	p := &core.paths[pi]
	if !p.ampAllow(96) {
		return
	}
	for i := range core.ctrl {
		it := &core.ctrl[i]
		if !it.used || it.kind != ctrlParkAwake {
			continue
		}
		var tb txBudget
		tb.items = c.itemScratch[:0]
		core.txBuildID++
		fb := wire.NewFrameBuilder(c.frameScratch())
		it.path = int16(pi)
		c.emitCtrlItem(fb, &tb, uint32(i))
		if fb.Len() == 0 {
			return
		}
		if c.emit(slot, pi, fb, &tb, now, 0) {
			core.awakePend = false
		}
		return
	}
	core.awakePend = false
}

// txPending reports whether any ack-eliciting work is waiting for a
// packet on path pi: pending control items routable there, handshake-
// progress obligations, stream open/data/FIN work, or queued datagrams.
// Application work does not count while the connection is parked.
func (c *Conn) txPending(pi int) bool {
	core := c.core
	for i := range core.ctrl {
		it := &core.ctrl[i]
		if it.used && !it.live && c.ctrlForPath(uint32(i), pi) {
			return true
		}
	}
	// FINISH and HANDSHAKE_DONE ride every packet until ACKed, even when
	// their item was already emitted once.
	prog := ctrlFinish
	if core.role == roleServer {
		prog = ctrlHandshakeDone
	}
	if _, ok := c.ctrlIDOf(prog, 0, 0); ok {
		return true
	}
	// A pending server KEY_CONFIRM obligation rides every new-epoch
	// packet (section 19.4).
	if core.role == roleServer && core.refresh.committed &&
		!core.refresh.confirmAcked {
		return true
	}
	if core.parked {
		return false
	}
	return c.appWorkPending()
}

// txMTUProbe emits a path-MTU probe on the active path (section 17.2).
// Probes carry only MTU_PROBE/PING/PADDING, are charged in-flight, and
// targets follow a binary search on eight-byte boundaries bounded by the
// peer's receive maximum. One probe is outstanding at a time.
func (c *Conn) txMTUProbe(now uint64) {
	core := c.core
	pi := core.activePth
	p := &core.paths[pi]
	if p.mtuProbeOut || now < p.mtuNextUS {
		return
	}
	hi := p.mtuHi
	if hi == 0 {
		hi = minU64(uint64(wire.MaxUDPPayload), uint64(core.peer.MaxUDPPayload))
	}
	if hi <= p.m {
		return // no headroom above the confirmed size
	}
	target := p.mtuTarget
	if target == 0 {
		// Greatest multiple of eight at or below the search midpoint.
		target = (p.m + hi) / 2 &^ 7
		if target <= p.m {
			target = hi &^ 7
		}
		if target <= p.m {
			return
		}
	}
	slot := core.keys.current()
	if slot == nil || core.ledger.liveFull() {
		return
	}
	// The pacing bucket caps at two datagrams of credit (section 15.3);
	// a probe larger than the confirmed M can never fit it. A probe is a
	// single rare datagram, so it spends the whole bucket instead: the
	// burst bound still holds because one probe is outstanding at a time.
	paceCost := minU64(target, 2*p.m)
	if !p.ampAllow(target) || !p.paceAllow(now, paceCost) {
		core.arm(tkPacer, now+p.paceWaitUS(now, paceCost))
		return
	}
	p.mtuProbeID++
	var tb txBudget
	tb.items = c.itemScratch[:0]
	fb := wire.NewFrameBuilder(c.frameScratch())
	var body [8]byte
	binary.BigEndian.PutUint64(body[:], p.mtuProbeID)
	if !fb.Add(wire.FrameMTUProbe, 0, 8, func(d []byte) { copy(d, body[:]) }) {
		return
	}
	// The probe datagram is padded to the tested size (section 17.2);
	// PADDING is legal here because MTU_PROBE is itself ack-eliciting.
	plain := int(target) - wire.HeaderBytes - wire.TagBytes
	if plain > 0 {
		fb.Pad(plain)
	}
	if c.emit(slot, pi, fb, &tb, now, p.mtuProbeID) {
		p.mtuProbeOut = true
		p.mtuTarget = target
		// No send-time spacing: section 17.2's max(1s,10·srtt) grid
		// applies to retries of a FAILED target (set in declareLost),
		// not to the first attempt or the next target after a success.
	} else {
		p.mtuProbeID--
	}
}

// emit seals fb into a wire packet on path pi and publishes it. tb may be
// nil; its items requeue when construction succeeded but emission did not
// commit. probeID nonzero tags the packet as an MTU probe carrier.
func (c *Conn) emit(slot *epochSlot, pi int, fb *wire.FrameBuilder, tb *txBudget, now uint64, probeID uint64) bool {
	core := c.core
	p := &core.paths[pi]
	var items []lossItem
	if tb != nil {
		items = tb.items
	}
	// Minimum plaintext: a packet under MinDataPlaintext is padded with an
	// authenticated PADDING frame. PADDING requires an ack-eliciting
	// sibling (section 11), so a sub-minimum non-eliciting packet first
	// gains a PING. A failed PING insert must not ship a PADDING-only
	// packet.
	if fb.Len() < wire.MinDataPlaintext {
		if !fb.Eliciting() && !fb.Add(wire.FramePing, 0, 0, nil) {
			c.txAbort(tb)
			return false
		}
		if !fb.Pad(wire.MinDataPlaintext) {
			c.txAbort(tb)
			return false
		}
	}
	// Section-4.5 hard limit is airtight at the seal boundary: no caller
	// path may cross it, including refresh progress on the staged slot.
	if !sealAdmits(slot, 1, uint64(fb.Len())+wire.HeaderBytes+wire.TagBytes) {
		c.txAbort(tb)
		return false
	}

	pn := core.nextPN
	core.nextPN++
	core.ledger.reserve(pn)
	p.ordinal++
	slot.sealCount++
	slot.sealBytes += uint64(fb.Len())

	wire.BuildHeader(c.txhdr[:], wire.ClassData, core.peerCID.cid, slot.epoch, pn,
		uint16(fb.Len()+wire.TagBytes), fb.Eliciting())
	var pkt []byte
	if c.qsend != nil {
		// QueueSocket takes ownership of the datagram — build it on a
		// pooled buffer rather than the reusable scratch.
		pkt = GetPacketBuf()[:0]
	} else {
		pkt = c.txfb[:0:wire.MaxUDPPayload]
	}
	pkt = append(pkt, c.txhdr[:]...)
	pkt = slot.tx.Seal(pn, c.txhdr[:], fb.Bytes(), pkt)
	eliciting := fb.Eliciting()
	ect := uint8(0)
	if eliciting {
		if ect = c.ecnMark(p); ect != 0 {
			p.sentECT0++
		}
	}
	var serr error
	if c.txBatching {
		if c.txBatch.active {
			incompatible := c.txBatch.pi != pi ||
				c.txBatch.local != p.local ||
				c.txBatch.remote != p.remote ||
				c.txBatch.segmentSize != uint16(p.m) ||
				c.txBatch.ecn != ect ||
				len(c.txBatch.items) >= wire.MaxLocalBatchDatagrams ||
				len(c.txBatch.buf)+len(pkt) > wire.SocketGSOMaxBytes ||
				(len(c.txBatch.items) > 0 && c.txBatch.items[len(c.txBatch.items)-1].pktLen < int(c.txBatch.segmentSize))
			if incompatible {
				c.flushTxBatch(slot)
			}
		}
		if !c.txBatch.active {
			c.txBatch.active = true
			c.txBatch.pi = pi
			c.txBatch.local = p.local
			c.txBatch.remote = p.remote
			c.txBatch.segmentSize = uint16(p.m)
			c.txBatch.ecn = ect
			if c.txBatch.buf == nil {
				c.txBatch.buf = make([]byte, 0, wire.SocketGSOMaxBytes)
			}
		}
		c.txBatch.buf = append(c.txBatch.buf, pkt...)
		p.txBytes += uint64(len(pkt))
		p.txPkts++
		if core.ctrlCredit < 64 {
			core.ctrlCredit++
		}
		padded := fb.Padded()
		if eliciting {
			p.lastElicitUS = now
			if core.probeBudget > 0 {
				core.probeBudget--
			}
		}
		if tb != nil && tb.ackSent {
			core.acker.reset()
			core.disarm(tkAckDelay)
		}
		for i := range items {
			if items[i].kind == itemCtrl && int(items[i].ctrlID) < len(core.ctrl) {
				it := &core.ctrl[items[i].ctrlID]
				if it.used && it.kind == ctrlPathChallenge && int(it.path) == pi {
					p.challengeBytes = len(pkt)
				}
			}
		}
		sp := newSentPkt(pn, now, uint16(len(pkt)), uint8(pi), p.ordinal, slot.epoch, eliciting, padded, ect, items, probeID)
		if eliciting || padded {
			sp.inFlight = true
			p.inFlight += uint64(len(pkt))
		}
		flowLimited := tb != nil && tb.flowLimited
		appLimited := p.inFlight < p.cwnd && !c.appWorkPending()
		if flowLimited || appLimited {
			sp.limited = true
		}
		blocked := tb != nil && tb.blockedEmit
		c.txBatch.items = append(c.txBatch.items, txBatchItem{
			pn:          pn,
			blockedEmit: blocked,
			sp:          sp,
			inFlight:    sp.inFlight,
			pktLen:      len(pkt),
		})
		if len(pkt) < int(c.txBatch.segmentSize) {
			c.flushTxBatch(slot)
		}
		return true
	}

	if c.qsend != nil {
		// Ownership of pkt moves to the adapter's send queue; the outcome
		// arrives as evSendRep and rolls back on failure (sendReport).
		c.qsend.QueueSend(pkt, p.remote, ect, pn)
	} else if ss, ok := core.sock.(SourceSocket); ok {
		serr = ss.SendFrom(pkt, p.local, p.remote, ect)
	} else {
		serr = core.sock.Send(pkt, p.remote, ect)
	}
	if serr != nil {
		core.ledger.set(pn, sentBurned)
		// Flow reservations stay charged; items revert to pending.
		c.txAbort(tb)
		return false
	}
	// EMIT_COMMIT
	p.txBytes += uint64(len(pkt))
	p.txPkts++
	if core.ctrlCredit < 64 {
		core.ctrlCredit++
	}
	core.ledger.set(pn, sentEmitted)
	padded := fb.Padded()
	if eliciting {
		p.lastElicitUS = now
		if core.probeBudget > 0 {
			core.probeBudget--
		}
	}
	if tb != nil && tb.ackSent {
		core.acker.reset()
		core.disarm(tkAckDelay)
	}
	// Challenge bookkeeping: the emitted datagram's wire size is the
	// matching challenge's size evidence (section 17.3). The send-time
	// anchor stays at the transaction's first send — rewriting it on a
	// retransmission would drift the retry grid past the deadline.
	for i := range items {
		if items[i].kind == itemCtrl && int(items[i].ctrlID) < len(core.ctrl) {
			it := &core.ctrl[items[i].ctrlID]
			if it.used && it.kind == ctrlPathChallenge && int(it.path) == pi {
				p.challengeBytes = len(pkt)
			}
		}
	}
	// Track every emitted packet so ACK validation stays exact. Packets
	// that are ack-eliciting or carry PADDING count in-flight (section
	// 14.1); pure ACK/CLOSE packets without PADDING do not.
	sp := newSentPkt(pn, now, uint16(len(pkt)), uint8(pi), p.ordinal, slot.epoch, eliciting, padded, ect, items, probeID)
	if eliciting || padded {
		sp.inFlight = true
		p.inFlight += uint64(len(pkt))
	}
	// Section 15.1: a packet sent while the sender was application- or
	// flow-control-limited must not grow cwnd when acknowledged. Build
	// records credit-blocked emission; app-limited means the pipe stays
	// underfull with no eligible work left after this send — the scan
	// runs only on that rare underfull-pipe outcome.
	flowLimited := tb != nil && tb.flowLimited
	appLimited := p.inFlight < p.cwnd && !c.appWorkPending()
	if flowLimited || appLimited {
		sp.limited = true
	}
	sp.blocked = tb != nil && tb.blockedEmit
	core.ledger.track(sp)
	return true
}

// sendReport completes an asynchronous QueueSend outcome (evSendRep). A
// failure is rolled back like the synchronous Send error path — the PN
// burns and its payload items revert to pending — except the packet was
// already tracked as emitted, so the live bookkeeping is undone here.
func (c *Conn) sendReport(pn uint64, err error) {
	core := c.core
	if err == nil || core.state == stDead {
		return
	}
	sp := core.ledger.get(pn)
	if sp == nil || core.ledger.status(pn) != sentEmitted {
		return // already resolved by ACK, loss, or retransmission
	}
	items := sp.items
	inFlight := sp.inFlight
	size := uint64(sp.bytes)
	pi := int(sp.path)
	blocked := sp.blocked
	for i := range items {
		c.itemLost(&items[i])
	}
	core.ledger.release(pn, sentBurned)
	p := &core.paths[pi]
	if p.txBytes >= size {
		p.txBytes -= size
	}
	if p.txPkts > 0 {
		p.txPkts--
	}
	if inFlight {
		if p.inFlight >= size {
			p.inFlight -= size
		} else {
			p.inFlight = 0
		}
	}
	if blocked {
		core.wantDataBlocked = true
	}
}

func (c *Conn) flushTxBatch(slot *epochSlot) {
	if !c.txBatch.active || len(c.txBatch.items) == 0 {
		c.txBatch.active = false
		c.txBatch.items = c.txBatch.items[:0]
		c.txBatch.buf = c.txBatch.buf[:0]
		return
	}
	core := c.core
	b := &c.txBatch
	pi := b.pi
	p := &core.paths[pi]

	var serr error
	if len(b.items) == 1 {
		if ss, ok := core.sock.(SourceSocket); ok {
			serr = ss.SendFrom(b.buf, b.local, b.remote, b.ecn)
		} else {
			serr = core.sock.Send(b.buf, b.remote, b.ecn)
		}
	} else {
		if sgs, ok := core.sock.(SourceGSOSocket); ok {
			serr = sgs.SendFromGSO(b.buf, b.segmentSize, b.local, b.remote, b.ecn)
		} else if gs, ok := core.sock.(GSOSocket); ok {
			serr = gs.SendGSO(b.buf, b.segmentSize, b.remote, b.ecn)
		} else if ss, ok := core.sock.(SourceSocket); ok {
			serr = ss.SendFrom(b.buf, b.local, b.remote, b.ecn)
		} else {
			serr = core.sock.Send(b.buf, b.remote, b.ecn)
		}
	}

	if serr != nil {
		for i := range b.items {
			it := &b.items[i]
			core.ledger.set(it.pn, sentBurned)
			// The packet's stable item copy lives on it.sp — the build
			// scratch has since been reused by later packets.
			for j := range it.sp.items {
				c.itemLost(&it.sp.items[j])
			}
			putSentPkt(it.sp)
			if it.blockedEmit {
				core.wantDataBlocked = true
			}
			p.txBytes -= uint64(it.pktLen)
			p.txPkts--
			if it.inFlight {
				if p.inFlight >= uint64(it.pktLen) {
					p.inFlight -= uint64(it.pktLen)
				} else {
					p.inFlight = 0
				}
			}
		}
	} else {
		for i := range b.items {
			it := &b.items[i]
			core.ledger.set(it.pn, sentEmitted)
			core.ledger.track(it.sp)
		}
	}

	b.active = false
	b.items = b.items[:0]
	b.buf = b.buf[:0]
}

// frameScratch returns the per-conn plaintext build buffer.
func (c *Conn) frameScratch() []byte {
	return c.framebuf[:0]
}

// sealAdmits checks the section-4.5 hard admission predicate.
func sealAdmits(s *epochSlot, n, bytes uint64) bool {
	hardC := wire.RekeyHardPacketsXChaCha
	hardB := wire.RekeyHardBytes
	if s.tx.CipherID() == wire.CipherAES256GCM {
		hardC = wire.RekeyHardPacketsAESGCM
	}
	return s.sealCount+n <= hardC && s.sealBytes+bytes <= hardB
}

// maybeBlocked emits a DATA_BLOCKED once per observed limit when the
// connection credit gate blocks eligible work.
func (c *Conn) maybeBlocked() {
	core := c.core
	if core.blockedAt == core.connTxLim {
		return
	}
	core.blockedAt = core.connTxLim
	c.queueCtrl(ctrlPing, 0, 0, 0, 0, [16]byte{})
	// DATA_BLOCKED is advisory; emit on the next packet.
	core.wantDataBlocked = true
}
