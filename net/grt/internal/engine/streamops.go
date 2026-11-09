package engine

import "gosuda.org/grt/internal/wire"

// applyReset commits a peer RESET_STREAM (section 12.6): fix/check final
// size, discard incomplete data, notify once.
func (c *Conn) applyReset(sid wire.StreamID, final uint64, code uint32) {
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.recv == nil || s.state != slotActive ||
		sid.Generation() != s.gen {
		return
	}
	r := s.recv
	if fsmRecvIsTerminal(r.state) {
		return
	}
	if !r.finalKnown {
		r.finalKnown, r.final = true, final
	}
	if final > r.high {
		c.core.connRxUsed += final - r.high
		r.high = final
	}
	fsmRecvSet(&r.state, recvReset)
	r.resetCode = code
	s.readTerm, s.readTermGen = termReset, s.gen
	if r.high > r.base {
		c.core.connRxAdv += r.high - r.base
		r.base = r.high
	}
	r.releaseAll() // discard incomplete retained data
	c.retireCheck(s, sid)
	notify(c.readWake)
}

// applyStopSending records the peer's request to terminate our send half.
func (c *Conn) applyStopSending(sid wire.StreamID, code uint32) {
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.send == nil || s.state != slotActive {
		return
	}
	if sid.Generation() != s.gen {
		return
	}
	sh := s.send
	if fsmSendIsTerminal(sh.state) {
		return // first effective request wins
	}
	sh.stopReq = true
	c.queueStreamReset(s, sid)
	notify(c.writeWake)
}

// queueStreamReset transitions a send half to SEND_RESET with a frozen
// final size and code (section 12.5).
func (c *Conn) queueStreamReset(s *streamSlot, sid wire.StreamID) {
	sh := s.send
	if sh == nil || fsmSendIsTerminal(sh.state) {
		return
	}
	// final = emitted FIN final size, else reserved high-water.
	sh.resetFinal = sh.txHigh
	if sh.finEmitted {
		sh.resetFinal = sh.final
	}
	if sh.resetCode == 0 {
		sh.resetCode = wire.ErrApplicationAbort
	}
	fsmSendSet(&sh.state, sendReset)
	sh.unsent = rangeSet{} // abandon unsent/retransmittable bytes
	sh.retx = rangeSet{}
	sh.unacked = rangeSet{}
	sh.buf = nil
	sh.head = 0
	c.queueCtrl(ctrlReset, sid.Slot(), uint64(sid), s.gen, sh.resetCode, [16]byte{})
	// The abandoned half is terminal: retirement may now proceed.
	c.retireCheck(s, sid)
}

// applyRetire handles peer STREAM_RETIRE (non-initiator side).
func (c *Conn) applyRetire(sid wire.StreamID) {
	core := c.core
	s := core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil {
		return
	}
	switch s.state {
	case slotActive:
		if sid.Generation() != s.gen {
			return
		}
		fsmSlotSet(&s.state, slotRetirePending)
		c.maybeRetireAck(s, sid)
	case slotRetirePending:
		// The ACK obligation is already queued; it emits once local
		// halves reach terminal (section 21.1).
	case slotRetiredWaitOpen:
		if sid.Generation() == s.gen {
			// Duplicate same-generation RETIRE: regenerate the same
			// RETIRE_ACK (identical one-shot items dedupe in the queue).
			c.queueCtrl(ctrlRetireAck, sid.Slot(), uint64(sid), s.gen, 0, [16]byte{})
		}
	}
}

// maybeRetireAck sends RETIRE_ACK once local halves are terminal. The
// retired peer-initiated slot frees its grant: offer a replacement via
// MAX_SLOTS (section 12.1).
func (c *Conn) maybeRetireAck(s *streamSlot, sid wire.StreamID) {
	if s.state != slotRetirePending {
		return
	}
	if !slotLocalDone(s) {
		return
	}
	fsmSlotSet(&s.state, slotRetiredWaitOpen)
	c.queueCtrl(ctrlRetireAck, sid.Slot(), uint64(sid), s.gen, 0, [16]byte{})
	core := c.core
	class := sid.Class()
	// Grants may never exceed the authenticated maximum we advertised
	// (section 12.1): the class's own MaximumBidi/UniSlots ceiling.
	maxBidi := uint32(core.local.MaximumBidiSlots)
	maxUni := uint32(core.local.MaximumUniSlots)
	cap_ := maxBidi
	if class&2 != 0 {
		cap_ = maxUni
	}
	if sid.Initiator() == c.peerInitiator() &&
		core.slots.localLimit[class] < cap_ {
		core.slots.localLimit[class]++
		c.queueCtrl(ctrlMaxSlots, 0, uint64(class),
			uint64(core.slots.localLimit[class]), 0, [16]byte{})
	}
}

// retireCheck re-evaluates both retirement directions after a local half
// became terminal: an initiator slot may now owe RETIRE, a peer slot may
// now owe RETIRE_ACK (section 21.1). Each helper gates on slot state.
func (c *Conn) retireCheck(s *streamSlot, sid wire.StreamID) {
	c.maybeRetire(s, sid)
	c.maybeRetireAck(s, sid)
}

// slotLocalDone reports whether both local halves of the slot are terminal.
// A reset or application-stopped half can never advance again and so
// counts as done for retirement purposes (section 21.1).
func slotLocalDone(s *streamSlot) bool {
	var sendSt sendState
	if s.send != nil {
		sendSt = s.send.state
	}
	var recvSt recvState
	var recvStopped bool
	if s.recv != nil {
		recvSt = s.recv.state
		recvStopped = s.recv.stopped
	}
	return fsmEvalStreamHalvesDone(s.send == nil, sendSt, s.recv == nil, recvSt, recvStopped)
}

// applyRetireAck handles RETIRE_ACK on the initiator side.
func (c *Conn) applyRetireAck(sid wire.StreamID) {
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.state != slotRetireSent || sid.Generation() != s.gen {
		return
	}
	// The retire obligation resolves at RETIRE_ACK (section 21.1): a lost
	// STREAM_RETIRE packet must not re-emit for the resolved generation.
	if id, ok := c.ctrlIDOf(ctrlRetire, uint64(sid), sid.Generation()); ok {
		c.core.ctrl[id].used = false
	}
	// Release old payload state; bump expected generation.
	if s.recv != nil {
		s.recv.releaseAll()
	}
	s.send, s.recv = nil, nil
	if s.gen >= wire.MaxGeneration {
		fsmSlotSet(&s.state, slotExhausted)
		return
	}
	s.gen++
	fsmSlotSet(&s.state, slotEmpty)
	s.openAcked = false
	notify(c.writeWake)
}

// ---------------------------------------------------------------------------
// Stream ACK/loss resolution (send half)

// ackStreamRange marks [lo,hi) acknowledged; flags&FIN resolves the FIN.
func (c *Conn) ackStreamRange(sid wire.StreamID, lo, hi uint64, flags uint16) {
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.send == nil || sid.Generation() != s.gen {
		return
	}
	sh := s.send
	sh.unacked.remove(lo, hi)
	sh.unsent.remove(lo, hi)
	sh.retx.remove(lo, hi)
	if flags&wire.FrameFlagStreamFin != 0 {
		sh.finPending = false
		sh.finEmitted = true
	}
	sh.advanceBase()
	notify(c.writeWake)
	if sh.state == sendFin && !sh.finPending && sh.unacked.empty() {
		fsmSendSet(&sh.state, sendDone)
		sh.buf = nil
		sh.head = 0
		c.retireCheck(s, sid)
	}
}

// lossStreamRange requeues a lost byte range for retransmission.
func (c *Conn) lossStreamRange(sid wire.StreamID, lo, hi uint64, flags uint16) {
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.send == nil || sid.Generation() != s.gen {
		return
	}
	sh := s.send
	if !fsmSendIsRetransmittable(sh.state) {
		return // abandoned bytes are not retransmitted
	}
	sh.retx.add(lo, hi)
	if flags&wire.FrameFlagStreamFin != 0 {
		sh.finPending = true
		sh.finEmitted = false
	}
}

// maybeRetire initiates slot retirement when local halves are done.
func (c *Conn) maybeRetire(s *streamSlot, sid wire.StreamID) {
	if s.state != slotActive {
		return
	}
	if s.initiator != c.ourInitiator() {
		return // only the initiator retires
	}
	if !slotLocalDone(s) {
		return
	}
	fsmSlotSet(&s.state, slotRetireSent)
	c.queueCtrl(ctrlRetire, sid.Slot(), uint64(sid), s.gen, 0, [16]byte{})
}
