package engine

import (
	"crypto/rand"
	"crypto/subtle"

	"gosuda.org/grt/internal/wire"
)

// Path states (spec section 17.7): FREE, VALIDATING, REACHABLE, READY,
// ACTIVE, OLD, FAILED. The pathRec.role field tracks the slot role
// (active/candidate/old); pstate tracks the validation lifecycle.
type pathSt uint8

const (
	psFree pathSt = iota
	psValidating
	psReachable
	psReady
	psActive
	psOld
	psFailed
)

// maxPathValidationUS returns the negotiated validation interval cap.
func (c *Conn) maxPathValidationUS() uint64 {
	ms := uint64(c.core.peer.MaxPathValidationMS)
	if ms == 0 {
		ms = wire.DefaultPathValidationMS
	}
	return ms * 1000
}

// startValidation (re)opens the challenge transaction on path pi with a
// fresh token and generation (spec section 17.3). All sends — the
// immediate first probe included — pass through onPathDeadline so the
// section-17.6 locally initiated probe bucket gates every one.
// challengeSentUS anchors the retry grid at the first actual send (t0).
func (c *Conn) startValidation(p *pathRec, pi int, now uint64, localInit bool) {
	core := c.core
	if _, err := rand.Read(p.challenge[:]); err != nil {
		// An entropy failure leaves a predictable challenge token; the
		// connection cannot safely proceed without fresh randomness.
		c.closeWith(codeInternal, 0)
		return
	}
	p.challengeBytes = 0
	p.responseBytes = 0
	p.probesSent = 0
	p.challengeSentUS = 0
	p.validating = true
	p.localInit = localInit
	vpto := p.validationPTO()
	vmax := c.maxPathValidationUS()
	if v7 := 7 * vpto; v7 < vmax {
		vmax = v7
	}
	p.validUntilUS = now + vmax
	p.nextProbeUS = now // first probe immediate when the gates permit
	if p.pstate != psActive {
		p.pstate = psValidating
	}
	core.arm(tkPathValid, now)
}

// createCandidate builds a peer-discovered candidate for an authenticated
// packet from an unmatched remote (spec section 17.2/17.3). The committed
// packet's own bytes supply the path's initial amplification credit.
func (c *Conn) createCandidate(e *connEvent) bool {
	core := c.core
	// Migration is not processed before handshake confirmation (section
	// 6): only an established connection retains an unmatched tuple's
	// committed packet as new candidate evidence.
	if core.state != stEstablished {
		return false
	}
	// NAT rebinding must be handled even when the application disabled
	// elective migration (section 17.1): the MIGRATION flags gate only
	// explicit intent-driven migration, never candidate discovery.
	for i := range core.paths {
		p := &core.paths[i]
		if p.role != pathFree {
			continue
		}
		old := &core.paths[core.activePth]
		*p = pathRec{
			role:   pathCandidate,
			remote: e.src,
			local:  e.dst,
			gen:    c.nextPathGen(),
			// Implicit-promotion guard (section 17.4): the fresh evidence
			// must postdate the active path's largest committed peer PN
			// at candidate creation.
			createdWatermark: old.peerPN,
		}
		p.initCC()
		now := nowUS()
		// Peer-discovered validation spends committed ingress credit,
		// not the persistent locally initiated probe bucket (17.6).
		c.startValidation(p, i, now, false)
		return true
	}
	return false
}

// nextPathGen returns a fresh path generation.
func (c *Conn) nextPathGen() uint32 {
	c.core.pathGen++
	return c.core.pathGen
}

// challengePending reports whether path pi has an unemitted challenge
// obligation in the control queue.
func (c *Conn) challengePending(pi int) bool {
	for i := range c.core.ctrl {
		it := &c.core.ctrl[i]
		if it.used && it.kind == ctrlPathChallenge && int(it.path) == pi {
			return true
		}
	}
	return false
}

// reviveChallenge re-arms the outstanding challenge item for path pi so it
// retransmits with a fresh PN under the same logical token (section 17.3).
// The queued token is refreshed to the live transaction's token; when the
// item was already retired by an ACK, a fresh obligation is queued.
func (c *Conn) reviveChallenge(pi int) {
	p := &c.core.paths[pi]
	for i := range c.core.ctrl {
		it := &c.core.ctrl[i]
		if it.used && it.kind == ctrlPathChallenge && int(it.path) == pi {
			it.live = false
			it.tok = p.challenge
			return
		}
	}
	c.queueCtrlPath(ctrlPathChallenge, int16(pi), 0, 0, 0, 0, p.challenge)
}

// applyPathResponse validates a response against the outstanding challenge
// of the path it arrived on (exact token, tuple, generation, expiry,
// base-size guards — spec section 17.3/17.7).
func (c *Conn) applyPathResponse(tok [16]byte, pi int, dgramLen int) {
	core := c.core
	p := &core.paths[pi]
	if !p.validating || (p.role != pathCandidate && p.role != pathActive &&
		p.role != pathOld) {
		return
	}
	if subtle.ConstantTimeCompare(tok[:], p.challenge[:]) != 1 {
		return // wrong token: no validation, timer, or promotion effect
	}
	now := nowUS()
	if now > p.validUntilUS {
		return
	}
	p.responseBytes = dgramLen
	p.validating = false
	// READY requires ≥1200-byte evidence in both directions; a smaller
	// permitted response establishes REACHABLE only (section 17.2). Only
	// a full-sized exchange seeds the conservative RTT sample.
	if p.challengeBytes >= wire.BaseUDPPayload && dgramLen >= wire.BaseUDPPayload {
		if p.challengeSentUS != 0 {
			p.rttSample(now, p.challengeSentUS, 0, 0)
		}
		if p.role == pathActive {
			p.pstate = psActive
		} else {
			p.pstate = psReady
		}
		p.validated = true
		p.stale = false // fresh proof: no longer a stale parked route
		if core.localIntentPath == pi {
			c.selectIntent(pi, now)
		}
		c.checkImplicitPromotion(pi)
		c.wakeAdvance(now) // releases a pending PARK_AWAKE on this path
		if core.role == roleClient && core.parkSt == parkWaking && core.awakeSeen {
			c.applyParkAwake(core.parkID, core.wakeSeq)
		}
		return
	}
	p.pstate = psReachable
	// Send another full-sized challenge after budget becomes available
	// (section 17.2): open a fresh transaction whose first probe is
	// paced by nextProbeUS so retries accrue amplification credit
	// instead of ping-ponging at peer-packet speed. The send itself is
	// driven by onPathDeadline → reviveChallenge.
	if now < p.validUntilUS {
		p.validating = true
		if _, err := rand.Read(p.challenge[:]); err != nil {
			// No fresh token: a predictable challenge cannot validate the
			// path; close rather than silently weaken validation.
			p.validating = false
			c.closeWith(codeInternal, wire.FramePathResponse)
			return
		}
		p.challengeBytes = 0
		p.responseBytes = 0
		p.probesSent = 0
		p.challengeSentUS = 0 // anchored at the first actual send
		vpto := p.validationPTO()
		vmax := c.maxPathValidationUS()
		if v7 := 7 * vpto; v7 < vmax {
			vmax = v7
		}
		p.validUntilUS = now + vmax
		p.nextProbeUS = now + vpto
		core.arm(tkPathValid, p.nextProbeUS)
	}
}

// onPathDeadline retries or fails outstanding path validations
// (section 17.3: retry at +vpto then +2*vpto, at most three sends; the
// transaction deadline never extends on duplicates).
func (c *Conn) onPathDeadline(now uint64) {
	core := c.core
	// Bounded migration result deadline (section 17.5): a SELECT left
	// unanswered past its deadline fails the election — pinned intent
	// obligations drop and routing returns to the retained authorized
	// path when this election had already promoted the candidate.
	if !core.localIntentDone && core.localIntentPath >= 0 &&
		core.localIntentDL != 0 && now >= core.localIntentDL {
		pi := core.localIntentPath
		core.localIntentPath = -1
		core.localIntentDone = true
		c.dropIntentItems(pi)
		if core.paths[pi].role == pathActive && core.paths[pi].intentSelected {
			c.rollbackIntent()
		}
	}
	var next uint64
	found := false
	// An unresolved intent's deadline joins the timer's next-fire scan so
	// it still fires when no path validation is in flight.
	if !core.localIntentDone && core.localIntentPath >= 0 &&
		core.localIntentDL != 0 {
		next, found = core.localIntentDL, true
	}
	for i := range core.paths {
		p := &core.paths[i]
		if !p.validating {
			continue
		}
		if now >= p.validUntilUS {
			// Transaction expired: the candidate fails; a healthy ACTIVE
			// path is unaffected (section 17.3).
			p.validating = false
			if p.role == pathCandidate {
				p.pstate = psFailed
				p.role = pathFree
				if core.localIntentPath == i {
					core.localIntentPath = -1
					core.localIntentDone = true
					c.dropIntentItems(i)
				}
			} else if p.role == pathActive {
				p.pstate = psActive // revalidation failed: keep route, stay stale
				p.stale = true
			} else if p.role == pathOld {
				p.pstate = psOld // retained path revalidation expired
			}
			continue
		}
		vpto := p.validationPTO()
		if p.probesSent < wire.MaxChallengeTransmissions && now >= p.nextProbeUS {
			if p.localInit && !c.probeSpend(now) {
				// Section-17.6 persistent probe bucket is empty: hold
				// the send until a token refills. A held probe never
				// consumed a transmission.
				p.nextProbeUS = now + c.probeWaitUS(now)
			} else {
				p.probesSent++
				if p.challengeSentUS == 0 {
					p.challengeSentUS = now // t0 = first actual send
				}
				// Cumulative grid anchored at t0: sends at t0, t0+vpto,
				// t0+3*vpto; the anchor never moves on retransmission.
				p.nextProbeUS = p.challengeSentUS +
					((1<<uint(p.probesSent))-1)*vpto
				c.reviveChallenge(i)
			}
		}
		d := minU64(p.validUntilUS, p.nextProbeUS)
		if !found || d < next {
			next, found = d, true
		}
	}
	if found {
		core.arm(tkPathValid, next)
	}
}

// applyPathIntent handles the peer's migration intent (spec section 17.4).
// Sequences strictly increase; SELECT may only move 0→1 within a sequence
// and its cutover/flags are then immutable across retries.
func (c *Conn) applyPathIntent(seq, cutover uint64, flags uint32, pi int) {
	core := c.core
	if flags&^3 != 0 || seq == 0 {
		c.closeWith(codePathState, wire.FramePathIntent)
		return
	}
	sel := flags&1 != 0
	p := &core.paths[pi]

	switch {
	case core.peerIntentRec && seq < core.peerIntentSeq:
		// Older completed sequence: never rolls routing backward; a
		// repeated SELECT regenerates its cached result.
		if sel && core.selOK && core.selSeq == seq {
			c.queueCtrlPath(ctrlPathSelected, int16(pi), 0,
				core.selSeq, 0, uint32(core.selStatus), [16]byte{})
		}
		return
	case core.peerIntentRec && seq == core.peerIntentSeq:
		// Same-sequence repeat must be consistent and arrive on the
		// recorded ingress path.
		if pi != core.peerIntentPath {
			c.closeWith(codePathState, wire.FramePathIntent)
			return
		}
		if core.peerIntentSel {
			// SELECT already recorded: cutover and flags are immutable.
			if !sel || cutover != core.peerIntentCut || flags != core.peerIntentFlags {
				c.closeWith(codePathState, wire.FramePathIntent)
				return
			}
			// An identical SELECT repeat re-evaluates the current
			// decision: the peer retries until ACCEPTED, and a prior
			// BUSY must upgrade once the path is READY (section 17.4).
		} else if !sel {
			return // probe intent repeat: nothing to do
		}
		// A recorded SELECT=0 may advance to SELECT=1 (0→1 transition).
	default:
		// New sequence (seq > peerIntentSeq or first intent).
	}

	if sel {
		// cutover must be nonzero and not beyond the carrying packet's PN.
		if cutover == 0 || cutover > core.lastPeerPN {
			c.closeWith(codePathState, wire.FramePathIntent)
			return
		}
	} else if cutover != 0 {
		// Probe-only intents carry cutover_pn=0.
		c.closeWith(codePathState, wire.FramePathIntent)
		return
	}

	// Record the intent at connection level; the ingress-path binding is
	// peerIntentPath.
	core.peerIntentRec = true
	core.peerIntentSeq = seq
	core.peerIntentSel = sel
	core.peerIntentCut = cutover
	core.peerIntentFlags = flags
	core.peerIntentPath = pi
	if !sel {
		return // probe-only intent: no selection result is owed
	}

	// SELECT: explicit migration requires both MIGRATION flags
	// (section 17.1); accept when the intent's path is READY or already
	// the active path (implicit promotion may have beaten the SELECT);
	// a path still validating answers BUSY; anything else declines.
	// DECLINED never revokes an existing active path (section 17.4).
	status := uint16(1) // DECLINED
	switch {
	case core.peer.FeatureFlags&wire.FeatureMigration == 0 ||
		core.local.FeatureFlags&wire.FeatureMigration == 0:
		// Elective migration is not negotiated: stay declined.
	case pi == core.activePth:
		status = 0 // ACCEPTED — the asked-for tuple is already selected
		c.peerSelect(pi, cutover)
	case p.pstate == psReady:
		status = 0 // ACCEPTED
		c.peerSelect(pi, cutover)
	case p.role == pathOld && !p.validating:
		// Deliberate return to a retained OLD path requires revalidation
		// (section 17.4): open the peer-driven transaction and answer
		// BUSY so the peer retries once READY.
		c.startValidation(p, pi, nowUS(), false)
		status = 2
	case p.validating || p.pstate == psReachable:
		status = 2 // BUSY: validation in flight; peer retries SELECT
	}
	core.selSeq, core.selStatus, core.selOK = seq, status, true
	c.queueCtrlPath(ctrlPathSelected, int16(pi), 0, seq, 0, uint32(status), [16]byte{})
}

// peerSelect performs the receiver-side promotion after an accepted peer
// SELECT: the cutover barrier becomes peer_cutover_floor and the path is
// atomically selected (spec section 17.4/17.5).
func (c *Conn) peerSelect(pi int, cutover uint64) {
	core := c.core
	core.peerCutoverFloor = cutover
	c.promotePath(pi)
}

// selectIntent performs the section-17.4/17.5 ordering barrier once the
// live intent's candidate is locally READY: cutover_pn is
// next_packet_number after every previously allocated packet, the
// outstanding probe intent upgrades 0→1 under the same sequence, and the
// local selection promotes the candidate — new application handoffs now
// leave on it. The SELECT result deadline is the bounded migration
// deadline (7*validation_pto capped by the negotiated interval).
func (c *Conn) selectIntent(pi int, now uint64) {
	core := c.core
	p := &core.paths[pi]
	if p.intentSelected {
		return
	}
	p.intentSelected = true
	core.localIntentCutover = maxU64(core.nextPN, 1)
	core.localIntentDL = now + minU64(7*p.validationPTO(),
		c.maxPathValidationUS())
	c.queueCtrlPath(ctrlPathIntent, int16(pi), 0,
		core.localIntentSeq, core.localIntentCutover, 1, [16]byte{})
	// Pull the path timer in if the new result deadline is earlier — it
	// must fire even once the candidate is no longer validating.
	if !core.armed[tkPathValid] || core.localIntentDL < core.deadline[tkPathValid] {
		core.arm(tkPathValid, core.localIntentDL)
	}
	c.promotePath(pi)
}

// applyPathSelected processes the peer's answer to our live SELECT
// intent (section 17.4): ACCEPTED completes the migration; BUSY repeats
// the SELECT while the bounded migration deadline holds; DECLINED fails
// the selection and returns routing to the retained authorized path.
func (c *Conn) applyPathSelected(seq uint64, status uint16, pi int) {
	core := c.core
	if core.localIntentPath < 0 || seq != core.localIntentSeq ||
		pi != core.localIntentPath || core.localIntentDone {
		return // stale, mismatched, or already-resolved result
	}
	switch status {
	case 0: // ACCEPTED
		core.localIntentDone = true
		core.localIntentPath = -1
	case 2: // BUSY: repeat SELECT until ACCEPTED or the deadline
		if nowUS() < core.localIntentDL {
			c.queueCtrlPath(ctrlPathIntent, int16(pi), 0,
				seq, core.localIntentCutover, 1, [16]byte{})
		}
	default: // DECLINED
		core.localIntentDone = true
		core.localIntentPath = -1
		c.dropIntentItems(pi)
		c.rollbackIntent()
	}
}

// rollbackIntent returns routing to the retained authorized path after a
// declined elective selection (section 17.5: a failed elective candidate
// returns to the original ACTIVE path).
func (c *Conn) rollbackIntent() {
	core := c.core
	for i := range core.paths {
		p := &core.paths[i]
		if i != core.activePth && p.role == pathOld && p.validated {
			c.promotePath(i)
			return
		}
	}
}

// dropIntentItems frees PATH_INTENT obligations pinned to a dead path:
// they can never emit on a FREE slot and nothing else retires them.
func (c *Conn) dropIntentItems(pi int) {
	for j := range c.core.ctrl {
		it := &c.core.ctrl[j]
		if it.used && it.kind == ctrlPathIntent && int(it.path) == pi {
			it.used = false
			it.body = nil
		}
	}
}

// checkImplicitPromotion applies the section-17.4 implicit-selection rule:
// a READY candidate with fresh non-probing evidence newer than both its
// creation watermark and peer_cutover_floor is selected automatically.
func (c *Conn) checkImplicitPromotion(pi int) {
	core := c.core
	p := &core.paths[pi]
	if p.role != pathCandidate || p.pstate != psReady {
		return
	}
	if p.evidencePN <= p.createdWatermark || p.evidencePN <= core.peerCutoverFloor {
		return
	}
	// Routine elective promotions observe hysteresis (section 17.6).
	now := nowUS()
	if now-core.lastPromoUS < maxU64(c.ptoUS(), 1000000) && core.lastPromoUS != 0 {
		return
	}
	c.promotePath(pi)
}

// promotePath atomically cuts routing over to path pi (spec section
// 17.5/17.7): the path must be READY (or a still-authorized retained OLD
// for hard-failure fallback); old ACTIVE becomes OLD with a bounded
// retention window; ECN is disabled for the rest of the connection.
func (c *Conn) promotePath(pi int) {
	core := c.core
	if pi == core.activePth {
		return
	}
	p := &core.paths[pi]
	if p.pstate != psReady && !(p.role == pathOld && p.validated) {
		return
	}
	now := nowUS()
	old := core.activePth
	oldP := &core.paths[old]
	oldPTO := oldP.basePTO(uint64(core.peer.MaxAckDelayUS))
	if oldP.role == pathActive {
		oldP.role = pathOld
		oldP.pstate = psOld
		oldP.cutoverPN = core.lastPeerPN
		oldP.validating = false
	}
	p.role = pathActive
	p.pstate = psActive
	p.validated = true
	p.promotedUS = now
	p.migSampled = false
	core.activePth = pi
	core.lastPromoUS = now
	// Tuple change: sender ECN is disabled for the remainder of the
	// connection (section 15.4); the new path restarts at M=1200.
	core.ecnKilled = true
	p.ecn = ecnUnknown
	p.ecnDisabled = true
	p.sentECT0, p.ackedECT0 = 0, 0
	// Prefer a fresh peer-issued CID on the new path when one is stocked
	// (section 18 SHOULD); the old CID stays valid.
	c.rotatePeerCID()
	// Retain OLD for min(3*old_base_pto, 10s) for late ACK attribution.
	retain := 3 * oldPTO
	if retain > 10000000 {
		retain = 10000000
	}
	core.arm(tkPathRetention, now+retain)
}

// rotatePeerCID switches the destination CID to a stocked peer-issued CID
// different from the current one, when available. The displaced record —
// including its reset token — swaps into the table and stays valid so a
// late stateless reset for it remains recognizable (section 18).
func (c *Conn) rotatePeerCID() {
	core := c.core
	for i := range core.peerCIDs {
		r := &core.peerCIDs[i]
		if r.valid && r.cid != core.peerCID.cid {
			*r, core.peerCID = core.peerCID, *r
			return
		}
	}
}

// onRetentionDeadline reclaims the OLD path after its drain window:
// live old-path packets are administratively detached as ABANDONED and
// their logical items requeue under the new path — detachment is not a
// congestion signal (spec section 17.5/17.7).
func (c *Conn) onRetentionDeadline(now uint64) {
	core := c.core
	for i := range core.paths {
		p := &core.paths[i]
		if p.role != pathOld {
			continue
		}
		if p.validating {
			// A deliberate-return revalidation outlives the retention
			// tick; its own bounded deadline reaps the transaction.
			continue
		}
		for sp := p.liveHead; sp != nil; {
			next := sp.pathNext
			c.declareAbandoned(sp)
			sp = next
		}
		p.role = pathFree
		p.pstate = psFree
	}
}
