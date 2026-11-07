package engine

import "gosuda.org/grt/internal/wire"

// Per-path RTT, loss, congestion, and pacing state lives in pathRec.
// All arithmetic follows spec sections 14 and 15 in microseconds.

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// initPathCC seeds a fresh path's congestion state (spec section 15.1).
func (p *pathRec) initCC() {
	p.m = wire.BaseUDPPayload
	p.cwnd = minU64(10*p.m, maxU64(2*p.m, 14720))
	p.ssthresh = wire.MaxDataValue
	p.srtt = wire.InitialRTTUS
	p.rttvar = wire.InitialRTTUS / 2
	p.firstSample = false
	p.minSet = false
	p.paceTokens = 2 * p.m * paceScale
	p.paceLastUS = nowUS()
}

// basePTO computes the section-14.4 base probe timeout.
func (p *pathRec) basePTO(peerMaxAckDelayUS uint64) uint64 {
	return maxU64(wire.MinPTOUS, p.srtt+maxU64(4*p.rttvar, 50)+peerMaxAckDelayUS)
}

// ptoDuration applies the pto_count backoff with the 60s clamp.
func (p *pathRec) ptoDuration(peerMaxAckDelayUS uint64) uint64 {
	d := p.basePTO(peerMaxAckDelayUS)
	for i := uint32(0); i < p.ptoCount; i++ {
		d *= 2
		if d >= 60000000 {
			return 60000000
		}
	}
	return d
}

// rttSample applies the section-14.2 estimator for a newly acknowledged
// largest packet. ackDelayUS is the peer-reported delay already clamped.
func (p *pathRec) rttSample(nowUS, sendUS, ackDelayUS, peerMaxAckDelayUS uint64) {
	latest := uint64(1)
	if nowUS > sendUS {
		latest = nowUS - sendUS
	}
	p.latestRTT = latest
	if !p.minSet || latest < p.minRTT {
		p.minRTT, p.minSet = latest, true
	}
	delay := minU64(ackDelayUS, peerMaxAckDelayUS)
	adjusted := latest
	if latest >= p.minRTT+delay {
		adjusted = latest - delay
	}
	if !p.firstSample {
		p.srtt = adjusted
		p.rttvar = (adjusted + 1) / 2
		p.firstSample = true
		return
	}
	old := p.srtt
	var d uint64
	if old > adjusted {
		d = old - adjusted
	} else {
		d = adjusted - old
	}
	p.rttvar = (3*p.rttvar + d + 3) / 4
	p.srtt = (7*old + adjusted + 7) / 8
}

// ---------------------------------------------------------------------------
// Pacing (section 15.3): two-datagram token bucket, 32.32 fixed point.

const paceScale = uint64(1) << 32

// paceRate returns tokens (bytes<<32) accrued per microsecond:
// rate = 5*cwnd / (4*max(srtt,50)).
func (p *pathRec) paceRate() uint64 {
	den := 4 * maxU64(p.srtt, 50)
	num := 5 * p.cwnd * paceScale
	return num / den
}

// paceRefill accrues tokens up to the 2*M cap.
func (p *pathRec) paceRefill(now uint64) {
	if now <= p.paceLastUS {
		return
	}
	dt := now - p.paceLastUS
	p.paceLastUS = now
	tokens := p.paceTokens + p.paceRate()*dt
	cap2m := 2 * p.m * paceScale
	if tokens > cap2m {
		tokens = cap2m
	}
	p.paceTokens = tokens
}

// paceAllow reports whether n bytes may be released now; when true the
// tokens are spent.
func (p *pathRec) paceAllow(now uint64, n uint64) bool {
	p.paceRefill(now)
	cost := n * paceScale
	if p.paceTokens < cost {
		return false
	}
	p.paceTokens -= cost
	return true
}

// paceWaitUS returns microseconds until n bytes are permitted.
func (p *pathRec) paceWaitUS(now, n uint64) uint64 {
	p.paceRefill(now)
	cost := n * paceScale
	if p.paceTokens >= cost {
		return 0
	}
	miss := cost - p.paceTokens
	rate := p.paceRate()
	if rate == 0 {
		return wire.MinPTOUS
	}
	return (miss + rate - 1) / rate
}

// ---------------------------------------------------------------------------
// Congestion events (section 15.1/15.2)

// onAcked applies congestion growth for newly acked in-flight bytes.
// eligible is the section-15.1 test: sent after recovery boundary and not
// app/flow-limited. The engine tracks limitedness per packet.
func (p *pathRec) onAcked(bytes uint64, eligible bool) {
	if !eligible {
		return
	}
	if p.inRecovery {
		// ACK beyond the boundary exits recovery (caller checks ordinal).
		return
	}
	if p.cwnd < p.ssthresh {
		p.cwnd += bytes // slow start
		return
	}
	p.caAccum += bytes
	for p.caAccum >= p.cwnd {
		p.caAccum -= p.cwnd
		p.cwnd += p.m
	}
}

// congestionEvent halves cwnd once per recovery boundary.
func (p *pathRec) congestionEvent() {
	p.ssthresh = maxU64(p.cwnd/2, 2*p.m)
	p.cwnd = p.ssthresh
	p.recBoundary = p.ordinal
	p.inRecovery = true
}

// persistentCongestion drops to minimum cwnd (section 15.2).
func (p *pathRec) persistentCongestion() {
	p.cwnd = 2 * p.m
	p.inRecovery = false
	p.caAccum = 0
	// finite ssthresh retained from the congestion event
}

// anti-amplification gate (spec section 6): a path to an unvalidated remote
// may send while tx + out <= 3*rx, in both bytes and packets.
func (p *pathRec) ampAllow(nBytes uint64) bool {
	return fsmEvalAmpGate(p.validated, p.txBytes, nBytes, p.rxBytes, p.txPkts, p.rxPkts, wire.AmpFactor)
}

// validationPTO is the section-17.3 challenge retry spacing:
// max(100ms, srtt + max(4*rttvar, 50ms)).
func (p *pathRec) validationPTO() uint64 {
	return maxU64(100000, p.srtt+maxU64(4*p.rttvar, 50000))
}

// probeSpend consumes one token from the persistent locally initiated
// probe bucket (capacity three 1200-byte datagrams, refill one per second;
// section 17.6). The bucket never refills on candidate deletion.
func (c *Conn) probeSpend(now uint64) bool {
	c.probeRefill(now)
	if c.core.probeToks < 1 {
		return false
	}
	c.core.probeToks--
	return true
}

// probeRefill lazily tops up the persistent probe bucket.
func (c *Conn) probeRefill(now uint64) {
	core := c.core
	if core.probeLastU == 0 {
		core.probeToks, core.probeLastU = 3, now
		return
	}
	if dt := now - core.probeLastU; dt > 0 {
		core.probeLastU = now
		core.probeToks += float64(dt) / 1e6
		if core.probeToks > 3 {
			core.probeToks = 3
		}
	}
}

// probeWaitUS returns the delay until the next probe token refills, or
// zero when a token is already held.
func (c *Conn) probeWaitUS(now uint64) uint64 {
	c.probeRefill(now)
	if c.core.probeToks >= 1 {
		return 0
	}
	return uint64((1-c.core.probeToks)*1e6) + 1
}

// migAllow reports whether n application/PTO bytes may ride a freshly
// promoted path: the migration startup bucket (12000-byte capacity, refill
// 12000 B/s, initially full) charges them until the path has its first
// same-path RTT sample and has remained ACTIVE for one srtt (section 17.6).
func (c *Conn) migAllow(now uint64, p *pathRec, n uint64) bool {
	core := c.core
	if core.migLastU == 0 {
		core.migToks, core.migLastU = 12000, now
	}
	if dt := now - core.migLastU; dt > 0 {
		core.migLastU = now
		core.migToks += dt * 12000 / 1e6
		if core.migToks > 12000 {
			core.migToks = 12000
		}
	}
	if p.promotedUS != 0 && (!p.migSampled || now-p.promotedUS < p.srtt) {
		if core.migToks < n {
			return false
		}
		core.migToks -= n
	}
	return true
}
