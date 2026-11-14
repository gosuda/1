package engine

import "gosuda.org/grt/internal/wire"

// applyFrames commits the validated transaction's semantic work in frame
// order (spec section 22 RX_COMMIT). ref is the decrypted-packet backing
// store stream payloads may borrow (nil when frames did not come from a
// refcounted buffer).
func (c *Conn) applyFrames(frames []wire.Frame, pf *rxPreflight, e *connEvent, now uint64, pi int, ref *ptRef) {
	for i := range frames {
		f := &frames[i]
		if c.core.state == stDead || c.core.state == stQuiescing {
			return
		}
		c.applyFrame(f, pf, e, now, pi, ref)
	}
}

func (c *Conn) applyFrame(f *wire.Frame, pf *rxPreflight, e *connEvent, now uint64, pi int, ref *ptRef) {
	core := c.core
	switch f.Type {
	case wire.FrameAck:
		a, _ := wire.ParseAckBody(f.Body)
		c.applyAck(&a, now, pi)
	case wire.FrameStreamOpen:
		sid64, _ := wire.ParseU64(f.Body)
		c.applyOpen(wire.StreamID(sid64))
	case wire.FrameStream:
		sid64, off, data, _ := wire.ParseStreamBody(f.Body)
		c.applyStreamData(wire.StreamID(sid64), off, data,
			f.Flags&wire.FrameFlagStreamFin != 0, ref)
	case wire.FrameResetStream:
		sid64, final, code, _ := wire.ParseResetStream(f.Body)
		c.applyReset(wire.StreamID(sid64), final, code)
	case wire.FrameStopSending:
		sid64, code, _ := wire.ParseStopSending(f.Body)
		c.applyStopSending(wire.StreamID(sid64), code)
	case wire.FrameMaxData:
		v, _ := wire.ParseU64(f.Body)
		if v > core.connTxLim {
			core.connTxLim = v
			notify(c.writeWake)
		}
	case wire.FrameMaxStreamData:
		sid64, lim, _ := wire.ParseMaxStreamData(f.Body)
		s := core.slots.slotOf(wire.StreamID(sid64).Class(), wire.StreamID(sid64).Slot())
		if s != nil && s.state == slotActive && s.send != nil &&
			wire.StreamID(sid64).Generation() == s.gen && lim > s.send.limit {
			s.send.limit = lim
			notify(c.writeWake)
		}
	case wire.FrameMaxSlots:
		class, lim, _ := wire.ParseMaxSlots(f.Body)
		if classOurs(class, core.role) && lim > core.slots.peerLimit[class] {
			maxBidi := uint32(core.peer.MaximumBidiSlots)
			maxUni := uint32(core.peer.MaximumUniSlots)
			cap_ := maxBidi
			if class&2 != 0 {
				cap_ = maxUni
			}
			if lim > cap_ {
				lim = cap_
			}
			core.slots.peerLimit[class] = lim
			notify(c.writeWake)
		}
	case wire.FrameStreamRetire:
		sid64, _ := wire.ParseU64(f.Body)
		c.applyRetire(wire.StreamID(sid64))
	case wire.FrameRetireAck:
		sid64, _ := wire.ParseU64(f.Body)
		c.applyRetireAck(wire.StreamID(sid64))
	case wire.FrameDatagram:
		_, data, _ := wire.ParseDatagram(f.Body)
		if len(core.dgIn) < 256 {
			core.dgIn = append(core.dgIn, append([]byte(nil), data...))
			notify(c.dgWake)
		}
	case wire.FrameClose:
		code, _, hash, _ := wire.ParseClose(f.Body)
		core.closeRemote = true
		core.termErr = &TransportError{Code: closeCode(code), Remote: true}
		copy(core.termErr.(*TransportError).Diag[16:], hash[:])
		c.enterDraining()
	case wire.FrameFinish:
		mac, tok, _ := wire.ParseFinish(f.Body)
		if c.applyFinish(mac, tok, pi) {
			pf.finishOK = true
		}
	case wire.FrameHandshakeDone:
		mac, _ := wire.ParseHandshakeDone(f.Body)
		if c.applyHandshakeDone(mac) {
			pf.handshakeDoneOK = true
		}
	case wire.FramePathChallenge:
		tok, _ := wire.Parse16(f.Body)
		// The response returns on the challenge's ingress path
		// (section 17.3), pinned so a later active-path change cannot
		// redirect it.
		c.queueCtrlPath(ctrlPathResponse, int16(pi), 0, 0, 0, 0, tok)
	case wire.FramePathResponse:
		tok, _ := wire.Parse16(f.Body)
		c.applyPathResponse(tok, pi, len(e.pkt))
	case wire.FramePathIntent:
		seq, cutover, flags, _ := wire.ParsePathIntent(f.Body)
		c.applyPathIntent(seq, cutover, flags, pi)
	case wire.FramePathSelected:
		seq, status, _ := wire.ParsePathSelected(f.Body)
		c.applyPathSelected(seq, status, pi)
	case wire.FrameNewCID:
		seq, retirePrior, cid, tok, _ := wire.ParseNewCID(f.Body)
		c.applyNewCID(seq, retirePrior, cid, tok)
	case wire.FrameRetireCID:
		seq, _ := wire.ParseU64(f.Body)
		c.applyRetireCID(seq)
	case wire.FrameNewToken:
		tok, _ := wire.ParseNewToken(f.Body)
		c.applyNewToken(tok)
	case wire.FrameNewTicket:
		meta, tkt, _ := wire.ParseNewTicket(f.Body)
		c.applyNewTicket(meta, tkt)
	case wire.FrameCryptoFragment:
		cf, _ := wire.ParseCryptoFrag(f.Body)
		c.applyCryptoFrag(&cf)
	case wire.FrameKeyCommit:
		epoch, id, mac, _ := wire.ParseKeyCommit(f.Body)
		c.applyKeyCommit(epoch, id, mac)
	case wire.FrameKeyConfirm:
		mac, _ := wire.Parse16(f.Body)
		_ = mac
		c.applyKeyConfirm(f.Body)
	case wire.FrameKeyRequest:
		v, _ := wire.ParseKeyRequest(f.Body)
		c.applyKeyRequest(v)
	case wire.FrameParkRequest:
		reqID, dur, _ := wire.ParseParkRequest(f.Body)
		c.applyParkRequest(reqID, dur)
	case wire.FrameParkGrant:
		reqID, dur, st, _ := wire.ParseParkGrant(f.Body)
		c.applyParkGrant(reqID, dur, st)
	case wire.FrameParkWake:
		reqID, wakeSeq, _ := wire.ParseParkWake(f.Body)
		c.applyParkWake(reqID, wakeSeq, pi)
	case wire.FrameParkAwake:
		reqID, wakeSeq, _ := wire.ParseParkWake(f.Body)
		c.applyParkAwake(reqID, wakeSeq)
	case wire.FramePing, wire.FramePadding, wire.FrameMTUProbe,
		wire.FrameDataBlocked, wire.FrameStreamBlocked, wire.FrameSlotsBlocked:
		// no semantic state
	}
}

// ---------------------------------------------------------------------------
// ACK processing (section 13.3/14)

// preflightAck validates ACK ranges against the emission ledger before
// commit. An ACK covering RESERVED/BURNED/never-emitted numbers in the
// retained window is ACK_RANGE_ERROR; a high ≥ nextPN is always an error.
// Range members below the verification floor are unverifiable and ignored
// per section 13.3, including inside a straddling range.
func (c *Conn) preflightAck(a *wire.AckFrame) closeCode {
	core := c.core
	floor := core.ledger.floor()
	ceil := floor + compactHistory
	for i := 0; i < a.Count; i++ {
		lo, hi := a.Ranges[i][0], a.Ranges[i][1]
		if hi >= core.nextPN {
			return codeAckRange
		}
		if hi < floor {
			continue
		}
		if lo < floor {
			lo = floor // below-floor members are unverifiable, not errors
		}
		if hi >= ceil {
			hi = ceil - 1
		}
		for pn := lo; pn <= hi; pn++ {
			// A live entry reads EMITTED in the history, so the status
			// word alone decides verifiability via the generated O(1)
			// SentPacket FSM bitmask classifier.
			if !fsmSentIsAckVerifiable(core.ledger.status(pn)) {
				return codeAckRange
			}
		}
	}
	return codeNoError
}

// applyAck processes a validated ACK frame: per-PN resolution, same-path
// RTT sample, loss detection, congestion growth, ECN validation. pi is the
// ACK's ingress path — section 14.2 attributes RTT samples only to the
// path the acknowledged packet was sent on.
func (c *Conn) applyAck(a *wire.AckFrame, now uint64, pi int) {
	core := c.core
	largestAcked := a.Ranges[0][1]
	var (
		hasLargest       bool
		largestEliciting bool
		largestPath      uint8
		largestSentUS    uint64
	)
	var newOrd [wire.MaxPaths]uint64
	var hasNewOrd [wire.MaxPaths]bool
	// Walk live entries in packet-number order up to the largest covered
	// number: live entries are the only numbers an ACK can newly resolve,
	// including retained below-floor entries the compact history cannot
	// verify (sections 13.3/36.5). ackOne unlinks each resolved entry, so
	// the successor is captured first.
	for sp := core.ledger.liveHead; sp != nil && sp.pn <= largestAcked; {
		next := sp.liveNext
		if ackCovers(a, sp.pn) {
			path := sp.path
			ord := sp.ordinal
			if sp.pn == largestAcked {
				hasLargest = true
				largestEliciting = sp.eliciting
				largestPath = path
				largestSentUS = sp.sentUS
			}
			c.ackOne(sp, now)
			if !hasNewOrd[path] || ord > newOrd[path] {
				newOrd[path], hasNewOrd[path] = ord, true
			}
		}
		sp = next
	}
	// RTT sample: only for a newly acked largest, eliciting packet, and
	// only when the ACK arrived on that packet's own path (section 14.2).
	if hasLargest && largestEliciting && int(largestPath) == pi {
		p := &core.paths[largestPath]
		p.rttSample(now, largestSentUS, uint64(a.AckDelayUS),
			uint64(core.peer.MaxAckDelayUS))
		p.ptoCount = 0 // newly acked eliciting packet resets PTO count
		p.migSampled = true
	}
	// Loss detection per path (section 14.3) using the ordinals this ACK
	// newly resolved.
	for pj := range core.paths {
		if !hasNewOrd[pj] && !core.paths[pj].hadAck {
			continue
		}
		c.detectLoss(pj, newOrd[pj], now)
	}
	// ECN feedback validation (section 15.4).
	c.applyECN(a, pi, now)
}

// ackCovers reports whether pn is inside any ACK range. Ranges are
// validated non-overlapping, non-touching, and sorted by descending high.
func ackCovers(a *wire.AckFrame, pn uint64) bool {
	if a.Count == 1 {
		return pn >= a.Ranges[0][0] && pn <= a.Ranges[0][1]
	}
	for i := 0; i < a.Count; i++ {
		if pn >= a.Ranges[i][0] && pn <= a.Ranges[i][1] {
			return true
		}
	}
	return false
}

// ackOne resolves one newly acknowledged packet.
func (c *Conn) ackOne(p *sentPkt, now uint64) {
	core := c.core
	for _, it := range p.items {
		c.itemAcked(&it)
	}
	path := &core.paths[p.path]
	if p.isMTU {
		// An explicit authenticated ACK of the probe carrier confirms
		// the complete datagram size (section 17.2). A confirmed target
		// unlocks the next one immediately — the max(1s,10·srtt) spacing
		// constrains only failed-target retries.
		path.mtuProbeOut = false
		path.mtuNextUS = 0
		if p.mtuProbe == path.mtuProbeID && path.mtuTarget > path.m {
			path.m = path.mtuTarget
			path.cwnd = maxU64(path.cwnd, 2*path.m)
		}
		path.mtuTarget, path.mtuTries = 0, 0
	} else if p.eliciting {
		path.baseAcked = true
	}
	if p.inFlight {
		// Charged packets release exactly once (section 14.1).
		if path.inFlight >= uint64(p.bytes) {
			path.inFlight -= uint64(p.bytes)
		} else {
			path.inFlight = 0
		}
	}
	// Section 15.1: growth applies to newly acknowledged in-flight bytes
	// sent after the recovery boundary while the sender was neither
	// application- nor flow-control-limited.
	eligible := p.eliciting && !p.limited && p.ordinal > path.recBoundary
	if path.inRecovery && p.ordinal > path.recBoundary {
		path.inRecovery = false // exit recovery
	}
	c.pcBreak(path, p)
	path.onAcked(uint64(p.bytes), eligible)
	if p.ectMark == 2 {
		path.ackedECT0++
	}
	if !path.hadAck || p.ordinal > path.maxAckOrd {
		path.maxAckOrd, path.hadAck = p.ordinal, true
	}
	core.ledger.release(p.pn, sentAcked)
}

// detectLoss runs section-14.3 packet/time-threshold loss on one path.
// newOrd is the largest ordinal this ACK newly resolved on the path.
func (c *Conn) detectLoss(pi int, newOrd uint64, now uint64) {
	core := c.core
	p := &core.paths[pi]
	thr := maxU64(50, (9*maxU64(p.srtt, p.latestRTT)+7)/8)
	var lost []*sentPkt
	// Live entries sit on the path's send-ordered list; both predicates
	// are monotone along it, so the first non-qualifying eliciting entry
	// ends the scan.
	for sp := p.liveHead; sp != nil; sp = sp.pathNext {
		if !sp.eliciting {
			continue
		}
		// Packet threshold: newly acked ordinal ≥ sp.ordinal+3.
		if newOrd >= sp.ordinal+3 {
			lost = append(lost, sp)
			continue
		}
		// Time threshold requires a later same-path ack.
		if p.hadAck && p.maxAckOrd > sp.ordinal && now >= sp.sentUS+thr {
			lost = append(lost, sp)
			continue
		}
		break
	}
	if len(lost) == 0 {
		return
	}
	var hiOrd uint64
	for _, sp := range lost {
		if sp.ordinal > hiOrd {
			hiOrd = sp.ordinal
		}
		c.declareLost(sp)
	}
	if !p.inRecovery || hiOrd > p.recBoundary {
		p.congestionEvent()
	}
	if c.persistentCongestion(pi, p) {
		p.persistentCongestion()
	}
}

// declareAbandoned administratively detaches one packet retired with its
// path or epoch (sections 17.5/19.5): unresolved logical items requeue,
// counted flight bytes release once, and the packet is marked ABANDONED —
// it is neither a congestion signal nor loss evidence.
func (c *Conn) declareAbandoned(sp *sentPkt) {
	core := c.core
	path := &core.paths[sp.path]
	if sp.inFlight && path.inFlight >= uint64(sp.bytes) {
		path.inFlight -= uint64(sp.bytes)
	}
	if sp.isMTU {
		path.mtuProbeOut = false
	}
	for _, it := range sp.items {
		c.itemLost(&it)
	}
	core.ledger.release(sp.pn, sentAbandoned)
}

// declareLost resolves one lost packet: requeue logical items, free
// in-flight bytes once.
func (c *Conn) declareLost(sp *sentPkt) {
	core := c.core
	path := &core.paths[sp.path]
	if sp.inFlight && path.inFlight >= uint64(sp.bytes) {
		path.inFlight -= uint64(sp.bytes)
	}
	if sp.isMTU {
		// A lost probe is evidence against the current target only when
		// base-size traffic still succeeds on the same path; otherwise it
		// is ordinary congestion (section 17.2). A target is retired
		// after three spaced failures.
		path.mtuProbeOut = false
		if path.baseAcked {
			path.mtuTries++
			if path.mtuTries >= 3 {
				hi := (path.mtuTarget - 1) &^ 7
				if hi > path.m {
					path.mtuHi = hi
				} else {
					path.mtuHi = path.m // no headroom left above base
				}
				path.mtuTarget, path.mtuTries = 0, 0
			}
		}
		path.mtuNextUS = nowUS() + maxU64(1e6, 10*path.srtt)
	}
	c.pcRun(path, sp)
	for _, it := range sp.items {
		c.itemLost(&it)
	}
	core.ledger.release(sp.pn, sentLost)
}

// itemAcked resolves a retransmittable reference on ACK.
func (c *Conn) itemAcked(it *lossItem) {
	switch it.kind {
	case itemStream:
		c.ackStreamRange(wire.StreamID(it.sid), it.lo, it.hi, it.flags)
	case itemCtrl:
		c.ackCtrlItem(it)
	case itemOpen:
		sid := wire.StreamID(it.sid)
		s := c.core.slots.slotOf(sid.Class(), sid.Slot())
		if s != nil && s.gen == sid.Generation() {
			s.openAcked = true
		}
	}
}

// itemLost requeues a retransmittable reference on loss.
func (c *Conn) itemLost(it *lossItem) {
	switch it.kind {
	case itemStream:
		c.lossStreamRange(wire.StreamID(it.sid), it.lo, it.hi, it.flags)
	case itemCtrl:
		c.lossCtrlItem(it)
	}
}
