package engine

import (
	"cmp"
	"crypto/sha256"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// closeWith begins an authenticated close with a protocol error (section
// 20.2): CLOSING when a safe sending key remains, else QUIESCING.
func (c *Conn) closeWith(code closeCode, offending uint16) {
	core := c.core
	if core.state.terminating() {
		return
	}
	core.termErr = &TransportError{Code: code}
	diag := sha256.Sum256([]byte(codeName(code)))
	copy(core.termErr.(*TransportError).Diag[:], diag[:])
	if core.state.keyBearing() {
		core.state = stClosing
		core.closeCode = code
		core.arm(tkClose, nowUS()+3*c.ptoUS())
	} else {
		c.quiesce(core.termErr)
	}
}

// enterDraining follows a valid peer CLOSE or stateless reset.
func (c *Conn) enterDraining() {
	core := c.core
	if core.state.terminating() {
		return
	}
	core.state = stDraining
	core.arm(tkClose, nowUS()+3*c.ptoUS())
	notify(c.readWake)
	notify(c.writeWake)
	notify(c.dgWake)
}

// quiesce stops emission and fences work (QUIESCING → DEAD).
func (c *Conn) quiesce(err error) {
	core := c.core
	if core.state == stDead {
		return
	}
	if err != nil && core.termErr == nil {
		core.termErr = err
	}
	core.state = stQuiescing
	// The portable profile has no outstanding I/O fences: the synchronous
	// socket adapter completes publication inside the owner call. Zeroize
	// secrets, release the admission-pool slot, and die.
	c.zeroize()
	c.releaseAttempt()
	core.state = stDead
}

// releaseAttempt returns the conn's admission-pool slot to the listener
// exactly once (spec sections 20.1/37.6): an attempt holds its slot from
// admission until handshake confirmation or death, whichever comes
// first. The attempts-table entry itself stays until conn death so late
// duplicate flights still route here for cheap suppression instead of
// re-entering admission as phantom attempts.
func (c *Conn) releaseAttempt() {
	core := c.core
	if core.listener == nil || !core.attemptHeld {
		return
	}
	core.attemptHeld = false
	l := core.listener
	l.mu.Lock()
	l.attemptsInFlight--
	l.mu.Unlock()
}

// zeroize erases key material and secret caches per policy (spec
// section 23.3): every secret the connection itself owns is erased at
// the DEAD boundary — epoch AEADs and roots, handshake residue, the
// transcript secrets behind FINISH/HANDSHAKE_DONE, staged refresh
// material, reset secrets and tokens, and retained resumption state.
// Config-owned secrets (ServerCert.IdentityPriv, TicketKey, the stored
// client ticket) are caller property and stay untouched.
func (c *Conn) zeroize() {
	core := c.core
	for i := range core.keys.slots {
		s := &core.keys.slots[i]
		if s.state == epochSlotEmpty {
			continue
		}
		s.tx.Zeroize()
		s.rx.Zeroize()
		s.state = epochSlotEmpty
	}
	for i := range core.keys.root {
		for j := range core.keys.root[i] {
			core.keys.root[i][j] = 0
		}
	}
	core.root0 = [32]byte{}
	core.t1 = [32]byte{}
	core.finKey = [32]byte{}
	core.doneKey = [32]byte{}
	core.exporterMaster = [32]byte{}
	core.resumptionMaster = [32]byte{}
	core.resetSecret = [32]byte{}
	core.cliReset = [16]byte{}
	core.newToken = [wire.TokenBytes]byte{}
	core.hasNewToken = false
	// Reset tokens carried on CID records are shared secrets against
	// off-path reset forgery; drop them with the rest.
	core.peerCID.token = [16]byte{}
	core.localCID.token = [16]byte{}
	for i := range core.peerCIDs {
		core.peerCIDs[i].token = [16]byte{}
	}
	for i := range core.localCIDs {
		core.localCIDs[i].token = [16]byte{}
	}
	// Handshake residue: one-time keypairs may still be live on a
	// mid-handshake death (the success path erases them separately).
	core.hs.sk = [32]byte{}
	core.hs.ikC.Zeroize()
	core.hs.spkC.Zeroize()
	core.hs.pqpkC.Zeroize()
	core.hs.t0 = [32]byte{}
	core.hs.t1 = [32]byte{}
	core.hs.ticketPSK = [32]byte{}
	core.hs.hasPSK = false
	core.hs.cliFinishMAC = [32]byte{}
	core.hs.token = [wire.TokenBytes]byte{}
	gc.Zeroize(core.hs.earlyPlain)
	core.hs.earlyPlain = nil
	// Public transcript bytes are not secret but are released so a dead
	// conn retains no bootstrap buffers.
	core.hs.clientMsg = nil
	core.hs.clientFrags = nil
	core.hs.serverMsg = nil
	core.hs.srvFrags = nil
	core.hs.cliMsg = nil
	core.hs.cliFrags = nil
	core.hs.serverFlight = nil
	core.hs.flight.frags = nil
	// Staged refresh secrets (a mid-transaction death must not leave
	// RT/proof MACs or fresh private keys behind); the whole struct
	// resets so buffers and reassembly state release as well.
	core.refresh = refreshTx{}
}

// ptoUS returns the current base PTO of the active path.
func (c *Conn) ptoUS() uint64 {
	p := &c.core.paths[c.core.activePth]
	return p.basePTO(uint64(c.core.peer.MaxAckDelayUS))
}

// onDeadline dispatches a named timer expiry (section 23 timer order).
func (c *Conn) onDeadline(k timerKind, now uint64) {
	core := c.core
	switch k {
	case tkLoss:
		c.onLossDeadline(now)
	case tkIdle:
		c.onIdleDeadline(now)
	case tkHandshake:
		c.onHandshakeDeadline(now)
	case tkAckDelay:
		// ACK deadline reached: txSchedule emits the due ACK.
	case tkClose:
		c.onCloseDeadline()
	case tkPacer:
		// pacing rearm: txSchedule handles
	case tkFlight:
		// paced flight emission step: txSchedule handles
	case tkPathValid:
		c.onPathDeadline(now)
	case tkPathRetention:
		c.onRetentionDeadline(now)
	case tkRefresh:
		c.onRefreshDeadline(now)
	case tkKeepalive:
		if core.state == stEstablished {
			c.queueCtrl(ctrlPing, 0, 0, 0, 0, [16]byte{})
		}
	case tkParkCtrl:
		c.onParkCtrl(now)
	case tkGrace:
		c.onGraceDeadline(now)
	}
}

// onLossDeadline fires the loss/PTO timer (section 14.3/14.4).
func (c *Conn) onLossDeadline(now uint64) {
	core := c.core
	// Time-threshold loss evaluation on each path. The path's live list
	// is send-ordered, so entries past the acked-ordinal bound or the
	// first not-yet-expired candidate end the scan.
	for pi := range core.paths {
		p := &core.paths[pi]
		if p.role == pathFree || !p.hadAck {
			continue
		}
		thr := maxU64(50, (9*maxU64(p.srtt, p.latestRTT)+7)/8)
		var lost []*sentPkt
		for sp := p.liveHead; sp != nil && p.maxAckOrd > sp.ordinal; sp = sp.pathNext {
			if !sp.eliciting {
				continue
			}
			if now < sp.sentUS+thr {
				break
			}
			lost = append(lost, sp)
		}
		var hiOrd uint64
		for _, sp := range lost {
			if sp.ordinal > hiOrd {
				hiOrd = sp.ordinal
			}
			c.declareLost(sp)
		}
		if len(lost) > 0 && (!p.inRecovery || hiOrd > p.recBoundary) {
			p.congestionEvent()
		}
		if len(lost) > 0 && c.persistentCongestion(pi, p) {
			p.persistentCongestion()
		}
	}
	// PTO: arm a probe obligation on the active path.
	p := &core.paths[core.activePth]
	if c.hasOutstanding() {
		p.ptoCount++
		core.probeBudget = 2
		c.queueCtrl(ctrlPing, 0, 0, 0, 0, [16]byte{})
	}
}

// pcRun extends the per-path persistent-congestion run for a newly lost
// ack-eliciting packet (spec section 15.2). Loss declarations can arrive
// out of send order, so the run tracks minimum and maximum ordinal/send
// time rather than first/last seen.
func (c *Conn) pcRun(p *pathRec, sp *sentPkt) {
	if !sp.eliciting {
		return
	}
	if p.pcN == 0 {
		p.pcLoOrd, p.pcHiOrd = sp.ordinal, sp.ordinal
		p.pcLoSent, p.pcHiSent = sp.sentUS, sp.sentUS
	} else {
		p.pcLoOrd = minU64(p.pcLoOrd, sp.ordinal)
		p.pcHiOrd = maxU64(p.pcHiOrd, sp.ordinal)
		p.pcLoSent = minU64(p.pcLoSent, sp.sentUS)
		p.pcHiSent = maxU64(p.pcHiSent, sp.sentUS)
	}
	p.pcN++
}

// pcBreak resets the run when an ack-eliciting packet is acknowledged:
// an acknowledged transmission inside or past the run's start breaks the
// interval. An acknowledgment below the run leaves it intact.
func (c *Conn) pcBreak(p *pathRec, sp *sentPkt) {
	if sp.eliciting && p.pcN > 0 && sp.ordinal > p.pcLoOrd {
		p.pcN = 0
	}
}

// persistentCongestion reports whether the lost run spans more than three
// base PTOs between its earliest and latest loss with no acknowledged
// ack-eliciting packet inside (spec section 15.2). Every ack-eliciting
// transmission inside the interval must also be declared lost: a merely
// outstanding one disqualifies the interval.
func (c *Conn) persistentCongestion(pi int, p *pathRec) bool {
	if p.pcN < 2 || p.pcHiSent-p.pcLoSent <= 3*p.basePTO(
		uint64(c.core.peer.MaxAckDelayUS)) {
		return false
	}
	for sp := p.liveHead; sp != nil && sp.ordinal < p.pcHiOrd; sp = sp.pathNext {
		if sp.eliciting && sp.ordinal > p.pcLoOrd {
			return false
		}
	}
	// The interval is consumed by this decision; a later loss starts a
	// fresh run rather than redeclaring the same congestion event.
	p.pcN = 0
	return true
}

// hasOutstanding reports whether ack-eliciting traffic is unresolved.
func (c *Conn) hasOutstanding() bool {
	return c.core.ledger.outstanding()
}

// idleUS is the effective idle deadline interval: the negotiated idle
// timeout, never earlier than three current base PTOs (section 20.5).
func (c *Conn) idleUS() uint64 {
	core := c.core
	idle := uint64(core.local.IdleTimeoutMS) * 1000
	if peerIdle := uint64(core.peer.IdleTimeoutMS) * 1000; peerIdle != 0 && peerIdle < idle {
		idle = peerIdle
	}
	idle = cmp.Or(idle, wire.DefaultIdleMS*1000)
	if three := 3 * c.ptoUS(); three > idle {
		idle = three
	}
	return idle
}

// onIdleDeadline handles idle expiry (section 20.5): silently quiesce.
// A live accepted parking lease replaces ordinary idle expiry only for
// its explicitly suspended interval; lease expiry itself quiesces.
func (c *Conn) onIdleDeadline(now uint64) {
	core := c.core
	if core.state.terminating() {
		return
	}
	if core.parked && core.parkLeaseUS != 0 &&
		(core.parkSt == parkLive || core.parkSt == parkWaking ||
			core.parkSt == parkAwakePend) {
		if now < core.parkLeaseUS {
			core.arm(tkIdle, core.parkLeaseUS)
			return
		}
		c.quiesce(&TransportError{Code: codeNoError}) // lease expired
		return
	}
	c.quiesce(&TransportError{Code: codeNoError})
}

// onHandshakeDeadline: establishment timeout.
func (c *Conn) onHandshakeDeadline(now uint64) {
	core := c.core
	switch core.state {
	case stCBuild, stCWaitReply, stCVerify, stSCollect, stSBuild:
		// Deadline before secure progress → QUIESCING (section 20.2).
		c.quiesce(errHandshakeFailed)
	case stCFinishing, stSFinishing:
		// Keys installed but not ESTABLISHED → CLOSING.
		c.closeWith(codeProtocolViolation, 0)
	}
}

// onCloseDeadline: CLOSING/DRAINING deadline → QUIESCING.
func (c *Conn) onCloseDeadline() {
	c.quiesce(nil)
}

// signalConfirmed fires CONFIRMED once.
func (c *Conn) signalConfirmed() {
	select {
	case <-c.confirmed:
	default:
		close(c.confirmed)
	}
}

// signalSecureReady fires SECURE_READY once (client side).
func (c *Conn) signalSecureReady() {
	select {
	case <-c.secureReady:
	default:
		close(c.secureReady)
	}
}

// updateLossTimer recomputes the earliest loss/PTO deadline.
func (c *Conn) updateLossTimer() {
	core := c.core
	var best uint64
	found := false
	now := nowUS()
	outst := core.ledger.outstanding()
	for pi := range core.paths {
		p := &core.paths[pi]
		if p.role == pathFree {
			continue
		}
		// Earliest time-loss candidate on this path: the first eliciting
		// entry below the acked-ordinal bound carries the smallest
		// send time on the send-ordered list.
		if p.hadAck {
			thr := maxU64(50, (9*maxU64(p.srtt, p.latestRTT)+7)/8)
			for sp := p.liveHead; sp != nil && p.maxAckOrd > sp.ordinal; sp = sp.pathNext {
				if !sp.eliciting {
					continue
				}
				d := sp.sentUS + thr
				if !found || d < best {
					best, found = d, true
				}
				break
			}
		}
		// PTO deadline.
		if outst && p.lastElicitUS > 0 {
			d := p.lastElicitUS + p.ptoDuration(uint64(core.peer.MaxAckDelayUS))
			if !found || d < best {
				best, found = d, true
			}
		}
	}
	if found {
		if best <= now {
			best = now + 50 // granularity floor
		}
		core.arm(tkLoss, best)
	} else {
		core.disarm(tkLoss)
	}
	// Idle deadline: last activity + max(idle_timeout, 3*base_pto). A
	// live granted lease replaces it with its own expiry — parking
	// suppresses the activity refresh that would otherwise postpone
	// quiescence past the promised lease (section 35.9 GRANTED→EXPIRED).
	idleDL := core.lastActivityUS + c.idleUS()
	if core.parked && core.parkLeaseUS != 0 &&
		(core.parkSt == parkLive || core.parkSt == parkWaking ||
			core.parkSt == parkAwakePend) {
		idleDL = core.parkLeaseUS
	}
	core.arm(tkIdle, idleDL)
	// Soft-limit key-refresh watcher runs on the same cadence.
	if core.state == stEstablished {
		c.refreshWatch(now)
	}
}
