package engine

import "gosuda.org/grt/internal/wire"

// ECN handling (spec section 15.4): the initial path probes with ECT(0)
// on its first ten ack-eliciting DATA packets; three consistently
// reported marked acknowledgments validate marking. Any migration kills
// sender ECN for the rest of the connection; receive counters continue.

// ecnMark returns the DSCP/ECN bits for a new packet on path p: ECT(0)
// while probing (bounded) or once validated, Not-ECT otherwise.
func (c *Conn) ecnMark(p *pathRec) uint8 {
	core := c.core
	if core.ecnKilled ||
		core.local.FeatureFlags&wire.FeatureECN == 0 ||
		core.peer.FeatureFlags&wire.FeatureECN == 0 ||
		p.ecnDisabled || !fsmECNIsMarkAllowed(p.ecn) {
		return 0
	}
	if p.ecn == ecnUnknown && p.sentECT0 >= 10 {
		return 0 // probing budget exhausted awaiting validation
	}
	return 2 // ECT(0)
}

// applyECN validates peer-reported ECN counters in an ACK (spec section
// 15.4). Only a newer largest-acknowledged report may move the accepted
// counters; inconsistent or impossible feedback disables marking on the
// ACK's arrival path. A CE increase is a congestion event bounded by the
// usual once-per-recovery rule.
func (c *Conn) applyECN(a *wire.AckFrame, pi int, now uint64) {
	core := c.core
	p := &core.paths[pi]
	if core.peer.FeatureFlags&wire.FeatureECN == 0 || core.ecnKilled {
		return // sender ECN is off for the connection after a tuple change
	}
	// A report with an older largest acknowledged packet cannot roll
	// counters back and is not re-evaluated.
	largest := a.Ranges[0][1]
	if core.ackLargestOK && largest <= core.ackLargestPN {
		return
	}
	e0, e1, ce := uint64(a.ECT0), uint64(a.ECT1), uint64(a.CE)
	newAckE0 := p.ackedECT0 - p.lastCountedE0 // marked pkts newly acked since last report
	bad := e1 != 0 ||
		e0 < p.lastAckECT0 || ce < p.lastAckCE ||
		e0+ce > p.sentECT0 ||
		e0+ce-(p.lastAckECT0+p.lastAckCE) < newAckE0
	if bad {
		p.ecn = ecnBroken
		p.ecnDisabled = true
		return
	}
	ceNew := ce > p.lastAckCE
	p.lastAckECT0, p.lastAckCE = e0, ce
	p.lastCountedE0 = p.ackedECT0
	core.ackLargestPN, core.ackLargestOK = largest, true

	if p.ecn == ecnUnknown && p.ackedECT0 >= 3 {
		p.ecn = ecnCapable // three consistent marked acknowledgments
	}
	if ceNew {
		// Congestion event once per recovery boundary.
		if !p.inRecovery {
			p.congestionEvent()
		}
	}
}
