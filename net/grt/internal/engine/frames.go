package engine

import (
	"math"

	"gosuda.org/grt/internal/wire"
)

// Stream-direction helpers: a received STREAM/RESET operates on our receive
// half of that slot; MAX_STREAM_DATA/STOP_SENDING operate on our send half.

// recvLegal reports whether the peer may carry a send-half frame (STREAM,
// RESET_STREAM) for this handle: always for bidi; for uni only when the
// peer is the initiator (otherwise the half does not exist).
func (c *Conn) recvLegal(sid wire.StreamID) bool {
	if !sid.Uni() {
		return true
	}
	return sid.Initiator() == c.peerInitiator()
}

// sendLegal reports whether a frame may address our send half: for uni only
// when we are the initiator.
func (c *Conn) sendLegal(sid wire.StreamID) bool {
	if !sid.Uni() {
		return true
	}
	return sid.Initiator() == c.ourInitiator()
}

func (c *Conn) ourInitiator() uint8 {
	if c.core.role == roleClient {
		return wire.StreamInitiatorClient
	}
	return wire.StreamInitiatorServer
}

func (c *Conn) peerInitiator() uint8 { return c.ourInitiator() ^ 1 }

// openedInTx reports whether this transaction already opened sid.
func (pf *rxPreflight) openedInTx(sid uint64) bool {
	for i := 0; i < pf.nOpened; i++ {
		if pf.opened[i] == sid {
			return true
		}
	}
	return false
}

func (pf *rxPreflight) markOpened(sid uint64) {
	if pf.nOpened < len(pf.opened) {
		pf.opened[pf.nOpened] = sid
		pf.nOpened++
	}
}

// preflightOpen validates a peer STREAM_OPEN.
func (c *Conn) preflightOpen(sid wire.StreamID, pf *rxPreflight) closeCode {
	core := c.core
	if sid.Initiator() != c.peerInitiator() {
		return codeStreamState // only the peer may open its streams
	}
	class := sid.Class()
	if uint32(sid.Slot()) >= core.slots.localLimit[class] {
		return codeStreamState // beyond granted slots
	}
	s := core.slots.slotOf(class, sid.Slot())
	st, gen := slotEmpty, uint64(0)
	if s != nil {
		st, gen = s.state, s.gen
	}
	switch st {
	case slotActive:
		if sid.Generation() == gen {
			return codeNoError // idempotent repeat
		}
		if sid.Generation() < gen {
			return codeNoError // stale
		}
		return codeStreamState
	case slotEmpty:
		if sid.Generation() == gen {
			pf.markOpened(uint64(sid))
			return codeNoError
		}
		if sid.Generation() < gen {
			return codeNoError // stale
		}
		return codeStreamState // future generation
	case slotRetiredWaitOpen:
		// Only an OPEN at exactly generation+1 reopens the slot
		// (section 21.1); every same-or-older-generation frame is stale.
		if sid.Generation() == gen+1 {
			pf.markOpened(uint64(sid))
			return codeNoError
		}
		if sid.Generation() <= gen {
			return codeNoError
		}
		return codeStreamState // future generation
	case slotRetirePending, slotRetireSent:
		if sid.Generation() <= gen {
			return codeNoError // stale repeat of retiring gen
		}
		return codeStreamState
	case slotExhausted:
		return codeNoError // stale traffic ignored
	}
	return codeStreamState
}

// preflightStreamData validates a STREAM frame's offsets and records its
// tentative flow increment.
func (c *Conn) preflightStreamData(sid wire.StreamID, off, n uint64, fin bool, pf *rxPreflight) closeCode {
	core := c.core
	if !c.recvLegal(sid) {
		return codeStreamState
	}
	sid64 := uint64(sid)
	s := core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil {
		if pf.openedInTx(sid64) {
			// Just-opened stream: check against the advertised window.
			return pf.chargeStreamEnd(sid64, off+n, fin,
				uint64(core.local.InitialStreamWindow), nil)
		}
		return codeStreamState
	}
	if s.state == slotActive && sid.Generation() < s.gen {
		return codeNoError // stale generation: ignore
	}
	open := s.state == slotActive && sid.Generation() == s.gen && s.recv != nil
	if !open && pf.openedInTx(sid64) {
		// Legal only if this transaction opened it (frame-order semantics).
		open = true
	}
	if !open {
		// Stale frames are ignored: an older generation on any slot, or
		// the retiring/retired generation once the slot left ACTIVE.
		stale := s.state == slotExhausted || sid.Generation() < s.gen ||
			(s.state != slotEmpty && s.state != slotActive &&
				sid.Generation() <= s.gen)
		if stale {
			return codeNoError
		}
		return codeStreamState // missing OPEN / future generation
	}
	// Tentative half: use the live-generation recv or a synthetic limit
	// check for a stream opened inside this packet. A half from a retired
	// generation must not leak into the new generation's checks.
	var r *recvHalf
	if s.state == slotActive && sid.Generation() == s.gen {
		r = s.recv
	}
	limit := uint64(core.local.InitialStreamWindow)
	if r != nil {
		limit = r.limit
	}
	return pf.chargeStreamEnd(sid64, off+n, fin, limit, r)
}

// chargeStreamEnd validates a STREAM frame's end and FIN marker against the
// tentative receive state for sid — the committed high-water/final on r
// (nil for a same-packet open) plus same-packet staging — then charges the
// unique high-water increase once. Frame-order semantics (section 22)
// require a second frame for the same stream to measure its increment
// above the earlier frame's end, so retransmitted or overlapping
// same-packet bytes are never charged twice. A completed stream still
// enforces final-size semantics but a stale duplicate is not recharged.
func (pf *rxPreflight) chargeStreamEnd(sid uint64, end uint64, fin bool,
	limit uint64, r *recvHalf) closeCode {
	var high, final uint64
	var finalOK, done bool
	if r != nil {
		high, final = r.high, r.final
		finalOK, done = r.finalKnown, r.state == recvDone
	}
	effHigh := pf.stagedHigh(sid, high)
	if f, ok := pf.stagedFinal(sid); ok {
		final, finalOK = f, true
	}
	// RFC9000 §2.2: end may not exceed the declared final; FIN data must
	// equal the declared final on every repeat and may not sit below
	// already-received bytes — including bytes staged earlier in this
	// packet.
	if finalOK && end > final {
		return codeFinalSize
	}
	if fin {
		finMismatch := finalOK && final != end
		if finMismatch || end < effHigh {
			return codeFinalSize
		}
	}
	if end > limit {
		return codeFlowControl
	}
	if done {
		return codeNoError
	}
	if end > effHigh {
		pf.connInc += end - effHigh
	}
	pf.stageEnd(sid, end, fin)
	return codeNoError
}

// preflightReset validates RESET_STREAM final-size semantics.
func (c *Conn) preflightReset(sid wire.StreamID, final uint64, pf *rxPreflight) closeCode {
	core := c.core
	if !c.recvLegal(sid) {
		return codeStreamState
	}
	sid64 := uint64(sid)
	s := core.slots.slotOf(sid.Class(), sid.Slot())
	noRecv := s == nil || s.state != slotActive
	if !noRecv {
		noRecv = s.recv == nil
	}
	if noRecv {
		if s != nil && (sid.Generation() < s.gen ||
			(s.state != slotEmpty && sid.Generation() <= s.gen)) {
			return codeNoError // stale
		}
		if pf.openedInTx(sid64) {
			// RESET for a just-opened stream: charge the gap.
			return pf.chargeResetEnd(sid64, final,
				uint64(core.local.InitialStreamWindow), nil)
		}
		return codeStreamState
	}
	if sid.Generation() != s.gen {
		if sid.Generation() < s.gen {
			return codeNoError
		}
		return codeStreamState
	}
	r := s.recv
	return pf.chargeResetEnd(sid64, final, r.limit, r)
}

// chargeResetEnd validates a RESET_STREAM's declared final size against the
// tentative receive state for sid — the committed high-water/final on r
// (nil for a same-packet open) plus same-packet staging — and charges the
// unique high-water increase once. A same-packet STREAM or FIN already
// staged the stream's high-water or final — the reset must agree with it,
// and only the gap above the staged end is charged.
func (pf *rxPreflight) chargeResetEnd(sid uint64, final uint64,
	limit uint64, r *recvHalf) closeCode {
	var high, prior uint64
	var priorOK bool
	if r != nil {
		high, prior, priorOK = r.high, r.final, r.finalKnown
	}
	effHigh := pf.stagedHigh(sid, high)
	if f, ok := pf.stagedFinal(sid); ok {
		prior, priorOK = f, true
	}
	if priorOK && prior != final {
		return codeFinalSize
	}
	if final < effHigh {
		return codeFinalSize // final below high-water
	}
	if final > limit {
		return codeFlowControl
	}
	pf.connInc += final - effHigh
	pf.stageEnd(sid, final, true)
	return codeNoError
}

// preflightStopSending validates STOP_SENDING targets an existing send half.
func (c *Conn) preflightStopSending(sid wire.StreamID) closeCode {
	if !c.sendLegal(sid) {
		return codeStreamState
	}
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil {
		return codeNoError // unopened: request to a dead half is a no-op
	}
	if sid.Generation() > s.gen {
		return codeStreamState // future generation
	}
	return codeNoError // live or stale generation: both no-ops here
}

// preflightMaxStreamData validates generation/existence for a credit frame.
func (c *Conn) preflightMaxStreamData(sid wire.StreamID, pf *rxPreflight) closeCode {
	if !c.sendLegal(sid) {
		return codeStreamState
	}
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil {
		// The same packet may have opened it earlier in the transaction
		// (section 12.4); the slot table is only grown at commit.
		if pf.openedInTx(uint64(sid)) {
			return codeNoError
		}
		return codeStreamState
	}
	if s.state == slotActive && sid.Generation() == s.gen && s.send != nil {
		return codeNoError
	}
	if pf.openedInTx(uint64(sid)) {
		return codeNoError // same packet opened it
	}
	// A future generation is an error. Credit for a generation at or below
	// the retained one is stale and ignored — except on EMPTY at the
	// expected generation, which names a not-yet-open stream: the peer
	// cannot know it exists, so that is an error (section 21.1).
	if sid.Generation() > s.gen {
		return codeStreamState
	}
	if s.state == slotEmpty && sid.Generation() == s.gen {
		return codeStreamState
	}
	return codeNoError
}

// preflightRetire validates STREAM_RETIRE from the stream initiator.
func (c *Conn) preflightRetire(sid wire.StreamID) closeCode {
	core := c.core
	if sid.Initiator() != c.peerInitiator() {
		return codeStreamState // only the initiator retires
	}
	s := core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil {
		return codeStreamState
	}
	switch s.state {
	case slotActive:
		if sid.Generation() != s.gen {
			if sid.Generation() < s.gen {
				return codeNoError
			}
			return codeStreamState
		}
		return codeNoError
	case slotRetirePending, slotRetiredWaitOpen:
		if sid.Generation() <= s.gen {
			return codeNoError // duplicate retire → regen RETIRE_ACK
		}
		return codeStreamState
	case slotExhausted:
		return codeNoError
	}
	return codeStreamState
}

// preflightRetireAck validates RETIRE_ACK (we are the initiator).
func (c *Conn) preflightRetireAck(sid wire.StreamID) closeCode {
	if sid.Initiator() != c.ourInitiator() {
		return codeStreamState
	}
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil {
		return codeStreamState
	}
	if s.state == slotRetireSent && sid.Generation() == s.gen {
		return codeNoError
	}
	if sid.Generation() < s.gen || s.state == slotExhausted {
		return codeNoError // stale
	}
	return codeStreamState
}

// ---------------------------------------------------------------------------
// Commit-time application of stream frames.

// applyOpen creates halves for a newly accepted OPEN (non-initiator side).
// The generation rules of section 21.1 are enforced again here: an OPEN
// only initializes an EMPTY slot at its expected generation or a
// RETIRED_WAIT_OPEN slot at exactly generation+1. Stale-generation OPENs
// never recreate a retired stream.
func (c *Conn) applyOpen(sid wire.StreamID) {
	core := c.core
	s := core.slots.ensureSlot(sid.Class(), sid.Slot())
	if s == nil {
		return
	}
	switch s.state {
	case slotActive:
		if s.gen == sid.Generation() {
			return // idempotent repeat
		}
		return
	case slotEmpty:
		if sid.Generation() != s.gen {
			return
		}
	case slotRetiredWaitOpen:
		if sid.Generation() != s.gen+1 {
			return
		}
	default:
		return
	}
	fsmSlotSet(&s.state, slotActive)
	s.gen = sid.Generation()
	s.readTerm = termNone // a new generation retires the old terminal
	s.uni = sid.Uni()
	s.initiator = sid.Initiator()
	s.openAcked = false
	s.send = nil
	s.recv = &recvHalf{state: recvOpen, segs: make([]rxSeg, 0, 16), limit: uint64(core.local.InitialStreamWindow)}
	if !s.uni {
		// Leave send.buf nil until the application actually writes on the
		// stream so peer STREAM_OPEN frames cannot force send-buffer
		// allocations.
		s.send = &sendHalf{state: sendOpen, limit: uint64(core.peer.InitialStreamWindow)}
	}
	// Section 36: accepting an OPEN reserves a small receive grant. Keep at
	// least one stream window of connection credit ahead of consumed base.
	if core.connRxLim-core.connRxAdv < s.recv.limit {
		core.connRxLim = core.connRxAdv + s.recv.limit
		c.queueCtrl(ctrlMaxData, 0, 0, core.connRxLim, 0, [16]byte{})
	}
	// Notify accept queue.
	if s.uni {
		select {
		case c.acceptUniQ <- uint64(sid):
		default:
		}
	} else {
		select {
		case c.acceptQ <- uint64(sid):
		default:
		}
	}
	notify(c.readWake) // a newly opened stream is a readability event
}

// applyStreamData commits stream bytes into the recv half. Stale-generation
// frames the preflight admitted for ACK purposes are ignored here so old
// bytes can never contaminate a reused generation.
func (c *Conn) applyStreamData(sid wire.StreamID, off uint64, data []byte, fin bool, ref *ptRef) {
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.recv == nil || s.state != slotActive ||
		sid.Generation() != s.gen {
		return
	}
	r := s.recv
	if r.state == recvDone || r.state == recvReset {
		return
	}
	end := off + uint64(len(data))
	if len(data) > 0 && end > r.base {
		if !r.insert(off, data, ref) {
			c.closeWith(codeStreamData, wire.FrameStream)
			return
		}
	}
	if end > r.high {
		c.core.connRxUsed += end - r.high
		r.high = end
	}
	if fin {
		r.finalKnown = true
		r.final = end
		if r.state == recvOpen {
			fsmRecvSet(&r.state, recvFin)
		}
	}
	if r.state == recvFin && r.finalKnown && r.base >= r.final && r.allConsumed() {
		// recvDone: appConsume reports the persistent errEOF terminal
		// from here on (section 12.3).
		fsmRecvSet(&r.state, recvDone)
		s.readTerm, s.readTermGen = termEOF, s.gen
		c.retireCheck(s, sid)
	}
	notify(c.readWake)
}

// allConsumed reports whether every received byte was delivered to the app.
func (r *recvHalf) allConsumed() bool {
	return r.pending == 0 && r.base >= r.high
}

// insert buffers data at off, checking overlap consistency (section 12.3).
// It returns false on a byte mismatch with retained coverage.
// When ref is non-nil and the run lands unmerged, the segment references
// data in place instead of copying it — the caller's decrypted-packet
// buffer stays alive until the run is consumed.
func (r *recvHalf) insert(off uint64, data []byte, ref *ptRef) bool {
	end := off + uint64(len(data))
	if off < r.base {
		d := r.base - off
		if d >= uint64(len(data)) {
			return true
		}
		data = data[d:]
		off = r.base
	}
	// Overlapping retained bytes must match.
	for i := range r.segs {
		s := &r.segs[i]
		lo, hi := maxU64(off, s.off), minU64(end, segEnd(s))
		for x := lo; x < hi; x++ {
			j := x - s.off
			if segHas(s, j) && s.data[j] != data[x-off] {
				return false
			}
		}
	}
	// The merge set — every retained run touching or abutting the new
	// range — is contiguous because runs are sorted, pairwise disjoint,
	// and never abutting.
	i := 0
	for i < len(r.segs) && segEnd(&r.segs[i]) < off {
		i++
	}
	lo, hi := off, end
	j := i
	for j < len(r.segs) && r.segs[j].off <= hi {
		lo = minU64(lo, r.segs[j].off)
		hi = maxU64(hi, segEnd(&r.segs[j]))
		j++
	}
	if i < j {
		// Referenced runs that only abut the new range stay unmerged:
		// merging would copy the whole shared packet buffer each one
		// borrows. Adjacent foreign runs keep the table sorted and
		// disjoint, and mergeClosest still bounds it under pressure.
		ins := i
		allForeignAbut := true
		for k := i; k < j; k++ {
			s := &r.segs[k]
			if s.ref == nil || (segEnd(s) != off && s.off != end) {
				allForeignAbut = false
				break
			}
			if segEnd(s) == off {
				ins = k + 1
			}
		}
		if !allForeignAbut {
			r.mergeRuns(i, j, lo, hi, off, data)
			return true
		}
		i = ins
	}
	if len(r.segs) >= maxRxSegs {
		// The merged pair may span the new range as a hole, so the merge
		// set must be recomputed; depth is bounded by the freed slot.
		r.mergeClosest()
		return r.insert(off, data, ref)
	}
	var ns rxSeg
	if ref != nil && ref.buf != nil {
		// Zero-copy: the run borrows the packet's plaintext buffer.
		// buf == nil means the buffer was freed mid-commit (a segment
		// carrying this ref was already released) — data still reads
		// correctly but must be copied, not borrowed.
		ns = rxSeg{off: off, data: data, full: true, ref: ref}
		ref.n++
	} else {
		var dataBuf []byte
		if cap(r.staging) >= 64*1024 {
			dataBuf = append(r.staging[:0], data...)
			r.staging = nil
		} else {
			dataBuf = make([]byte, len(data), 64*1024)
			copy(dataBuf, data)
		}
		ns = rxSeg{
			off:  off,
			data: dataBuf,
			full: true,
		}
	}
	r.pending += uint64(len(data))
	r.segs = append(r.segs, rxSeg{})
	copy(r.segs[i+1:], r.segs[i:])
	r.segs[i] = ns
	return true
}

// mergeRuns folds segs[i:j] and the new run [off,end) into one run
// covering [lo,hi). Member runs always tile the new range contiguously,
// so the merged run is hole-free iff every member was.
func (r *recvHalf) mergeRuns(i, j int, lo, hi, off uint64, data []byte) {
	end := off + uint64(len(data))
	if j == i+1 {
		s := &r.segs[i]
		// A referenced run never grows by append: its tail room belongs
		// to the shared packet buffer, where another segment's bytes
		// may live.
		if s.ref == nil && s.full && lo == s.off && off >= s.off && off <= segEnd(s) && end >= segEnd(s) {
			tail := data[segEnd(s)-off:]
			if len(tail) > 0 {
				s.data = append(s.data, tail...)
				r.pending += uint64(len(tail))
			}
			return
		}
	}

	span := int(hi - lo)
	m := rxSeg{off: lo, data: make([]byte, span), valid: make([]byte, (span+7)/8)}
	m.full = true
	for k := i; k < j; k++ {
		s := &r.segs[k]
		d := int(s.off - lo)
		copy(m.data[d:], s.data)
		copyValid(m.valid, d, s)
		if !s.full {
			m.full = false
		}
		r.releaseSeg(s)
	}
	nd := int(off - lo)
	copy(m.data[nd:], data)
	var newBits uint64
	for b := 0; b < len(data); b++ {
		if m.valid[(nd+b)>>3]>>(uint64(nd+b)&7)&1 == 0 {
			newBits++
		}
	}
	setBits(m.valid, nd, nd+len(data))
	r.pending += newBits
	r.segs[i] = m
	if j > i+1 {
		copy(r.segs[i+1:], r.segs[j:])
		r.segs = r.segs[:len(r.segs)-(j-i-1)]
	}
}

// mergeClosest merges the adjacent run pair with the smallest gap when
// the table is full; the gap becomes a hole in the merged run.
func (r *recvHalf) mergeClosest() {
	best, gap := -1, uint64(math.MaxUint64)
	for k := 0; k+1 < len(r.segs); k++ {
		g := r.segs[k+1].off - segEnd(&r.segs[k])
		if g < gap {
			best, gap = k, g
		}
	}
	if best < 0 {
		return
	}
	a, b := &r.segs[best], &r.segs[best+1]
	lo, hi := a.off, segEnd(b)
	span := int(hi - lo)
	m := rxSeg{off: lo, data: make([]byte, span), valid: make([]byte, (span+7)/8)}
	copy(m.data[int(a.off-lo):], a.data)
	copy(m.data[int(b.off-lo):], b.data)
	copyValid(m.valid, int(a.off-lo), a)
	copyValid(m.valid, int(b.off-lo), b)
	r.releaseSeg(a)
	r.releaseSeg(b)
	r.segs[best] = m
	copy(r.segs[best+1:], r.segs[best+2:])
	r.segs[len(r.segs)-1] = rxSeg{}
	r.segs = r.segs[:len(r.segs)-1]
}

// read delivers contiguous received bytes to the app, advancing base.
func (r *recvHalf) read(dst []byte) int {
	n := 0
	for n < len(dst) && len(r.segs) > 0 {
		s := &r.segs[0]
		if s.off > r.base {
			break
		}
		d := r.base - s.off
		if d >= uint64(len(s.data)) {
			if s.ref == nil && cap(s.data) >= 64*1024 && cap(r.staging) < cap(s.data) {
				r.staging = s.data[:0]
			}
			r.releaseSeg(s)
			copy(r.segs, r.segs[1:])
			r.segs[len(r.segs)-1] = rxSeg{}
			r.segs = r.segs[:len(r.segs)-1]
			continue
		}
		run := segRun(s, d)
		if run == 0 {
			break // hole at base
		}
		m := copy(dst[n:], s.data[d:d+run])
		n += m
		r.base += uint64(m)
		r.pending -= uint64(m)
	}
	if len(r.segs) > 0 {
		s := &r.segs[0]
		if s.off < r.base {
			d := r.base - s.off
			if d >= uint64(len(s.data)) {
				if len(r.segs) == 1 && s.ref == nil && s.full && cap(s.data) >= 64*1024 {
					s.data = s.data[:0]
					s.off = r.base
				} else {
					if s.ref == nil && cap(s.data) >= 64*1024 && cap(r.staging) < cap(s.data) {
						r.staging = s.data[:0]
					}
					r.releaseSeg(s)
					copy(r.segs, r.segs[1:])
					r.segs[len(r.segs)-1] = rxSeg{}
					r.segs = r.segs[:len(r.segs)-1]
				}
			} else if s.ref != nil {
				// Referenced runs are never rewritten in place; slicing
				// the consumed prefix is enough.
				s.data = s.data[d:]
				s.off = r.base
			} else if s.full && (d >= 16384 || d*2 >= uint64(len(s.data))) {
				rem := copy(s.data, s.data[d:])
				s.data = s.data[:rem]
				s.off = r.base
			}
		}
	}
	return n
}

// readable reports whether the app can read or a terminal state is pending.
func (r *recvHalf) readable() bool {
	for i := range r.segs {
		s := &r.segs[i]
		if s.off > r.base {
			break
		}
		d := r.base - s.off
		if d >= uint64(len(s.data)) {
			continue
		}
		if segHas(s, d) {
			return true
		}
		break
	}
	return r.state == recvDone || r.state == recvReset ||
		(r.state == recvFin && r.base >= r.final)
}
