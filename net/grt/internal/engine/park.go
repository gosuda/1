package engine

import "gosuda.org/grt/internal/wire"

// Parking (spec section 35): a client-requested bounded retention lease.
// One lease and one wake sequence are live at a time; results are cached
// for duplicate suppression, never a history. All park frames are 16-byte
// bodies and ack-eliciting.

// parkNegotiated reports whether the PARK feature and a positive lease
// bound were negotiated at both ends.
func (c *Conn) parkNegotiated() bool {
	core := c.core
	return core.local.FeatureFlags&wire.FeaturePark != 0 &&
		core.peer.FeatureFlags&wire.FeaturePark != 0 &&
		core.local.MaxParkDurationMS != 0 && core.peer.MaxParkDurationMS != 0
}

// parkMaxMS is the effective lease ceiling: the smaller advertised value.
func (c *Conn) parkMaxMS() uint64 {
	return minU64(uint64(c.core.local.MaxParkDurationMS),
		uint64(c.core.peer.MaxParkDurationMS))
}

// appQuiescent reports the section-35.5 requester precondition: no active
// stream (including unread receive data), no unacknowledged application
// bytes, no pending datagrams, no unfinished retirements, no key refresh,
// and no elective candidate.
func (c *Conn) appQuiescent() bool {
	core := c.core
	if core.refresh.active || core.refresh.committed {
		return false
	}
	for i := range core.paths {
		if core.paths[i].role == pathCandidate {
			return false
		}
	}
	for class := 0; class < 4; class++ {
		for _, s := range core.slots.slots[class] {
			if s.state == slotActive || s.state == slotRetireSent {
				return false
			}
		}
	}
	if len(core.dgOut) > 0 {
		return false
	}
	for sp := core.ledger.liveHead; sp != nil; sp = sp.liveNext {
		for _, li := range sp.items {
			if li.kind == itemStream || li.kind == itemOpen ||
				li.kind == itemCtrl {
				return false
			}
		}
	}
	return true
}

// refreshReserveOK reports the section-35.6 control reserve: at least 128
// remaining DATA seals and 524288 remaining plaintext-byte allowance under
// the current directional key before granting/requesting a lease.
func (c *Conn) refreshReserveOK() bool {
	s := c.core.keys.current()
	if s == nil {
		return false
	}
	hardP := wire.RekeyHardPacketsXChaCha
	if s.tx.CipherID() == wire.CipherAES256GCM {
		hardP = wire.RekeyHardPacketsAESGCM
	}
	return hardP-s.sealCount >= 128 &&
		wire.RekeyHardBytes-s.sealBytes >= 524288
}

// leasePTO is lease_control_pto: max(100ms, current base_pto).
func (c *Conn) leasePTO() uint64 {
	return maxU64(100000, c.ptoUS())
}

// ---------------------------------------------------------------------------
// Client side

// appPark issues PARK_REQUEST (client only). The request retransmits on
// the bounded 0/pto/3pto schedule until a result or the request deadline.
func (c *Conn) appPark(cmd *appCmd) error {
	core := c.core
	if core.role != roleClient || core.state != stEstablished {
		return errClosed
	}
	if !c.parkNegotiated() {
		return errNoPark
	}
	dur := uint64(cmd.code)
	if dur < 1000 || dur > c.parkMaxMS() {
		return errMobileState
	}
	if core.parkSt != parkIdle {
		return errWouldBlock
	}
	if !c.appQuiescent() || !c.refreshReserveOK() {
		return errMobileState
	}
	now := nowUS()
	core.parkID++
	if core.parkID == 0 {
		core.parkID = 1
	}
	core.parkReqUS = now
	core.ctrlPTO = c.leasePTO()
	core.parkSt = parkReqWait
	core.parked = true // freeze application admission at request
	core.parkSends = 1
	core.parkBaseUS = now
	core.parkNextUS = now + core.ctrlPTO
	dl := now + 7*core.ctrlPTO
	if cap := now + c.maxPathValidationUS(); cap < dl {
		dl = cap
	}
	core.parkDL = dl
	c.queueCtrl(ctrlParkRequest, 0, core.parkID, dur, 0, [16]byte{})
	core.arm(tkParkCtrl, core.parkNextUS)
	return nil
}

// appWake begins a wake attempt for the parked or uncertain lease. A new
// nonzero wake_seq is chosen once per attempt (section 35.8).
func (c *Conn) appWake() error {
	core := c.core
	if core.role != roleClient {
		return errClosed
	}
	if core.parkSt != parkLive && core.parkSt != parkReqWait &&
		core.parkSt != parkWaking {
		return errClosed
	}
	c.beginWake()
	return nil
}

// beginWake starts a wake attempt: new wake_seq, fresh path revalidation,
// PARK_WAKE on the bounded schedule (section 35.6/35.8).
func (c *Conn) beginWake() {
	core := c.core
	now := nowUS()
	core.wakeSeq++
	if core.wakeSeq == 0 {
		core.wakeSeq = 1
	}
	core.wakeSends = 1
	core.parkSt = parkWaking
	// The attempt snapshots the same pto formula and anchors a fresh
	// 0/pto/3pto resend grid (section 35.8).
	core.ctrlPTO = c.leasePTO()
	core.parkBaseUS = now
	// Wake deadline: min(7*pto horizon, negotiated validation cap, and the
	// known lease expiry or the uncertain-result recovery deadline).
	dl := now + 7*core.ctrlPTO
	if cap := now + c.maxPathValidationUS(); cap < dl {
		dl = cap
	}
	if core.parkLeaseUS != 0 {
		if core.parkLeaseUS < dl {
			dl = core.parkLeaseUS
		}
	} else if idle := core.lastActivityUS + c.idleUS(); idle < dl {
		dl = idle
	}
	core.wakeDL = dl
	core.parkNextUS = now + core.ctrlPTO
	// The stored path is stale after parking even when the tuple text is
	// unchanged: revalidate with a fresh generation and no carried-over
	// challenge or budget (section 35.6).
	p := &core.paths[core.activePth]
	if p.role == pathActive && !p.validating {
		p.gen = c.nextPathGen()
		p.stale = true
		c.startValidation(p, core.activePth, now, true)
	}
	c.queueCtrl(ctrlParkWake, 0, core.parkID, core.wakeSeq, 0, [16]byte{})
	core.arm(tkParkCtrl, core.parkNextUS)
}

// applyParkGrant handles the grant/refusal of our request (client side).
func (c *Conn) applyParkGrant(reqID uint64, durMS uint32, status uint16) {
	core := c.core
	if reqID != core.parkID || core.parkSt != parkReqWait {
		// A late grant in WAKING cannot reverse the wake (section 35.6).
		return
	}
	if status != 0 || durMS == 0 {
		// REFUSED/BUSY are final for this request ID; retain ordinary
		// idle semantics and reopen the gate.
		core.parkSt = parkIdle
		core.parked = false
		core.disarm(tkParkCtrl)
		notify(c.writeWake)
		return
	}
	if uint64(durMS) > c.parkMaxMS() {
		c.closeWith(codeMobileState, wire.FrameParkGrant)
		return
	}
	core.parkSt = parkLive
	// Conservative expiry from first request handoff, never the later
	// grant-arrival time (section 35.5).
	core.parkLeaseUS = core.parkReqUS + uint64(durMS)*1000
	core.disarm(tkParkCtrl)
	core.arm(tkIdle, core.parkLeaseUS)
}

// applyParkAwake completes our wake (client side): a matching result is
// accepted only before the conservative/recovery deadline and after the
// local path is READY/ACTIVE again (section 35.6).
func (c *Conn) applyParkAwake(reqID, wakeSeq uint64) {
	core := c.core
	if core.parkSt != parkWaking || reqID != core.parkID ||
		wakeSeq != core.wakeSeq {
		return
	}
	now := nowUS()
	if core.wakeDL != 0 && now > core.wakeDL {
		return
	}
	core.awakeSeen = true
	p := &core.paths[core.activePth]
	if (p.pstate != psActive && p.pstate != psReady) || p.stale {
		return // wait for path readiness; the deadline still applies
	}
	core.awakeSeen = false
	core.parked = false
	core.parkSt = parkIdle
	core.parkLeaseUS = 0
	core.disarm(tkParkCtrl)
	core.disarm(tkIdle)
	core.lastActivityUS = now
	notify(c.writeWake)
}

// ---------------------------------------------------------------------------
// Server side

// applyParkRequest handles a peer's PARK_REQUEST (server side, section
// 35.9). Results are regenerated for duplicates; contradictory nonstale
// encodings are MOBILE_STATE_ERROR.
func (c *Conn) applyParkRequest(reqID uint64, durMS uint32) {
	core := c.core
	if reqID == 0 {
		c.closeWith(codeMobileState, wire.FrameParkRequest)
		return
	}
	// Stale completed/lower IDs are ignored; they cannot extend a lease.
	if core.parkReqSeen && reqID < core.lastParkReq {
		return
	}
	if core.parkReqSeen && reqID == core.lastParkReq {
		// Duplicate of the recorded request: identical fields required.
		if durMS != core.lastParkDur {
			c.closeWith(codeMobileState, wire.FrameParkRequest)
			return
		}
		// Regenerate the cached result under the bounded response policy.
		if core.parkSends < 3 {
			core.parkSends++
			c.queueCtrl(ctrlParkGrant, 0, reqID,
				uint64(core.grantDur), uint32(core.grantStatus), [16]byte{})
		}
		return
	}
	// A fresh request while a lease/wake for an older ID is live is a
	// contradiction (at most one lease is live).
	if core.parkReqSeen && reqID > core.lastParkReq &&
		(core.parkSt == parkLive || core.parkSt == parkWaking ||
			core.parkSt == parkAwakePend) {
		c.closeWith(codeMobileState, wire.FrameParkRequest)
		return
	}
	core.parkReqSeen = true
	core.lastParkReq = reqID
	core.lastParkDur = durMS

	status := uint16(1) // REFUSED
	grant := uint32(0)
	maxD := c.parkMaxMS()
	if !c.parkNegotiated() || core.state != stEstablished {
		status = 1
	} else if uint64(durMS) < 1000 || uint64(durMS) > maxD {
		// Out-of-range request duration is an authenticated error.
		c.closeWith(codeMobileState, wire.FrameParkRequest)
		return
	} else if !c.appQuiescent() || !c.refreshReserveOK() {
		status = 2 // BUSY: retention capacity or quiescence unmet
	} else {
		// Grant: freeze our own admission and fix the lease expiry from
		// this first grant handoff (section 35.5). The active route is
		// stale after parking even when the wake tuple text matches it:
		// PARK_AWAKE waits for a fresh routability proof (section 35.6).
		status, grant = 0, durMS
		core.parked = true
		core.parkSt = parkLive
		core.parkID = reqID
		core.parkDurMS = durMS
		core.parkLeaseUS = nowUS() + uint64(durMS)*1000
		core.peerWakeSeq = 0
		core.parkSends = 1
		core.ctrlPTO = c.leasePTO()
		core.parkBaseUS = nowUS()
		core.parkNextUS = nowUS() + c.leasePTO()
		core.paths[core.activePth].stale = true
		core.arm(tkIdle, core.parkLeaseUS)
		core.arm(tkParkCtrl, core.parkNextUS)
	}
	core.grantStatus = status
	core.grantDur = grant
	c.queueCtrl(ctrlParkGrant, 0, reqID, uint64(grant), uint32(status), [16]byte{})
}

// applyParkWake handles the peer's wake (server side, section 35.6/35.9).
// A nonstale wake on a granted lease moves to WAKE_PENDING; a nonstale
// wake without a lease normalizes the uncertain request without creating
// a lease. PARK_AWAKE is emitted only once the ingress path is READY.
func (c *Conn) applyParkWake(reqID, wakeSeq uint64, pi int) {
	core := c.core
	if reqID == 0 || wakeSeq == 0 {
		c.closeWith(codeMobileState, wire.FrameParkWake)
		return
	}
	now := nowUS()
	switch {
	case core.parkSt == parkLive || core.parkSt == parkWaking ||
		core.parkSt == parkAwakePend:
		if reqID != core.parkID {
			if reqID > core.parkID {
				c.closeWith(codeMobileState, wire.FrameParkWake)
			}
			return // lower stale request ID cannot cancel a newer lease
		}
		if wakeSeq < core.peerWakeSeq {
			return // stale wake
		}
		if wakeSeq == core.peerWakeSeq {
			// Duplicate wake: regenerate the same result under quotas.
			if core.parkSt == parkAwakePend {
				c.queueCtrlPath(ctrlParkAwake, int16(core.wakePath),
					0, reqID, wakeSeq, 0, [16]byte{})
			}
			return
		}
		core.peerWakeSeq = wakeSeq
		if core.parkSt != parkAwakePend {
			core.parkSt = parkWaking
		}
		core.wakePath = pi
		// Revalidate the wake ingress tuple before emitting AWAKE; the
		// lease (or ordinary idle) deadline bounds the WAKE_PENDING wait.
		p := &core.paths[pi]
		if !p.validating &&
			(p.role == pathCandidate || (p.role == pathActive && p.stale)) {
			c.startValidation(p, pi, now, false)
		}
		c.wakeAdvance(now)
	default:
		// No live lease: a nonstale request ID may be recorded as
		// completed and answered with PARK_AWAKE after the same guards;
		// it creates no lease or expiry extension.
		if core.parkReqSeen && reqID <= core.lastParkReq {
			return // stale
		}
		if core.state != stEstablished || !c.parkNegotiated() {
			return
		}
		core.parkID = reqID
		core.peerWakeSeq = wakeSeq
		core.parkSt = parkWaking
		core.parked = true
		core.wakePath = pi
		p := &core.paths[pi]
		if !p.validating &&
			(p.role == pathCandidate || (p.role == pathActive && p.stale)) {
			c.startValidation(p, pi, now, false)
		}
		c.wakeAdvance(now)
	}
}

// wakeAdvance emits PARK_AWAKE once the wake path is READY and the
// server's receive-capacity reservation is held (section 35.6/35.9).
func (c *Conn) wakeAdvance(now uint64) {
	core := c.core
	if core.parkSt != parkWaking || core.wakePath < 0 {
		return
	}
	p := &core.paths[core.wakePath]
	if (p.pstate != psReady && p.pstate != psActive) || p.stale {
		return // ROUTE not ready/proven: stay WAKE_PENDING under the fixed deadline
	}
	// Receive gate opens before the result is emitted; the send gate stays
	// closed until an awake-carrying packet is acknowledged.
	core.parkSt = parkAwakePend
	core.awakePend = true
	core.parkSends = 1
	core.parkBaseUS = now
	core.parkNextUS = now + c.leasePTO()
	core.arm(tkParkCtrl, core.parkNextUS)
	c.queueCtrlPath(ctrlParkAwake, int16(core.wakePath),
		0, core.parkID, core.peerWakeSeq, 0, [16]byte{})
}

// parkAwakeAcked opens the server's application send gate when an
// awake-carrying packet is acknowledged (section 35.9 AWAKE_SENT→COMPLETE).
func (c *Conn) parkAwakeAcked() {
	core := c.core
	if core.parkSt != parkAwakePend {
		return
	}
	core.parkSt = parkIdle
	core.parked = false
	core.parkLeaseUS = 0
	core.awakePend = false
	core.disarm(tkParkCtrl)
	core.disarm(tkIdle)
	core.lastActivityUS = nowUS()
	notify(c.writeWake)
}

// onParkCtrl drives the bounded 0/pto/3pto resend schedule for pending
// park controls and their fixed deadlines (section 35.8). parkBaseUS
// anchors the grid at the first handoff; a transaction's deadline stays
// armed even after its three sends are spent so the uncertain-result
// normalization remains reachable.
func (c *Conn) onParkCtrl(now uint64) {
	core := c.core
	sends := &core.parkSends
	dl := uint64(0)
	switch core.parkSt {
	case parkReqWait:
		dl = core.parkDL
		if now >= dl {
			// Result uncertain: keep the gate closed and normalize with
			// PARK_WAKE under the same request ID.
			c.beginWake()
			return
		}
		if *sends < 3 && now >= core.parkNextUS {
			*sends++
			c.reviveCtrlKind(ctrlParkRequest)
		}
	case parkWaking:
		if core.role == roleClient {
			sends = &core.wakeSends
			dl = core.wakeDL
			if dl != 0 && now >= dl {
				// Wake failed: gates stay closed; lease expiry or a new
				// authorized attempt decides. No timeout return to online.
				core.parkSt = parkLive
				return
			}
			if *sends < 3 && now >= core.parkNextUS {
				*sends++
				c.reviveCtrlKind(ctrlParkWake)
			}
		} else {
			// Server WAKE_PENDING has no separate attempt deadline: the
			// lease (or ordinary idle) timer bounds the wait, and a
			// duplicate wake re-enters through applyParkWake. Reverting
			// to LIVE here would abandon the awake obligation and, in
			// the no-lease case, emit phantom grants (section 35.9).
			c.wakeAdvance(now)
			core.disarm(tkParkCtrl)
			return
		}
	case parkAwakePend:
		// Server may re-emit the awake result at pto/3pto while unacked.
		if *sends < 3 && now >= core.parkNextUS {
			*sends++
			core.awakePend = true
			c.reviveCtrlKind(ctrlParkAwake)
		}
	case parkLive:
		if core.role != roleServer {
			core.disarm(tkParkCtrl) // a parked client has nothing pending
			return
		}
		// Server unsolicited grant re-sends within the bounded policy.
		if *sends < 3 && now >= core.parkNextUS {
			*sends++
			c.queueCtrl(ctrlParkGrant, 0, core.parkID,
				uint64(core.grantDur), uint32(core.grantStatus), [16]byte{})
		}
	default:
		core.disarm(tkParkCtrl)
		return
	}
	var next uint64
	if *sends < 3 {
		// Grid anchored at the first handoff: the k-th resend lands at
		// parkBaseUS + (2^k - 1)*pto → t0, t0+pto, t0+3pto.
		core.parkNextUS = core.parkBaseUS + ((1<<uint(*sends))-1)*core.ctrlPTO
		next = core.parkNextUS
	}
	if dl != 0 && (next == 0 || dl < next) {
		next = dl
	}
	if next == 0 {
		core.disarm(tkParkCtrl)
		return
	}
	core.arm(tkParkCtrl, next)
}
