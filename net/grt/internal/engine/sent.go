package engine

import (
	"sync"

	"gosuda.org/grt/internal/wire"
)

var sentPktPool = sync.Pool{
	New: func() any {
		return &sentPkt{
			items: make([]lossItem, 0, 8),
		}
	},
}

func getSentPkt() *sentPkt {
	sp := sentPktPool.Get().(*sentPkt)
	sp.items = sp.items[:0]
	return sp
}

func putSentPkt(sp *sentPkt) {
	if sp != nil {
		sp.items = sp.items[:0]
		sp.livePrev, sp.liveNext = nil, nil
		sp.pathPrev, sp.pathNext = nil, nil
		sentPktPool.Put(sp)
	}
}

func newSentPkt(pn uint64, sentUS uint64, bytes uint16, path uint8, ordinal uint64, epoch uint32, eliciting, padded bool, ectMark uint8, items []lossItem, probeID uint64) *sentPkt {
	sp := getSentPkt()
	sp.pn = pn
	sp.sentUS = sentUS
	sp.bytes = bytes
	sp.path = path
	sp.ordinal = ordinal
	sp.epoch = epoch
	sp.eliciting = eliciting
	sp.padded = padded
	sp.ectMark = ectMark
	sp.inFlight = false
	sp.limited = false
	sp.blocked = false
	if probeID != 0 {
		sp.isMTU, sp.mtuProbe = true, probeID
	} else {
		sp.isMTU, sp.mtuProbe = false, 0
	}
	sp.items = append(sp.items[:0], items...)
	return sp
}

// lossItem is one retransmittable logical reference inside a sent packet.
// Items identify work to requeue on loss or release on acknowledgment.
type lossItem struct {
	kind   uint8
	sid    uint64 // full StreamID for itemStream
	lo     uint64 // stream byte range [lo,hi); ctrl: emitted value / frag index
	hi     uint64
	flags  uint16 // stream frame flags (FIN); intent: emitted intent flags
	ctrlID uint32 // control item index for itemCtrl
	// ctrlGen is the ctrl slot's allocation generation when the reference
	// was taken: an item recycled since resolves nothing for this packet.
	ctrlGen uint32
}

const (
	itemStream uint8 = iota // stream data range [lo,hi), flags carries FIN
	itemCtrl                // control-table entry
	itemOpen                // STREAM_OPEN; sid identifies the slot
)

// sentPkt is one live entry in the sent table (spec section 14.1).
// Live entries sit on two intrusive lists: the ledger's packet-number
// order and their path's send-ordinal order. Emission order is strictly
// increasing for both, so link/unlink are O(1) and loss/ACK scans walk
// only the relevant entries in order.
type sentPkt struct {
	pn        uint64
	sentUS    uint64 // release timestamp (conservative, pre-publish)
	bytes     uint16 // UDP payload bytes
	path      uint8  // path slot
	ordinal   uint64 // path emission ordinal
	eliciting bool
	inFlight  bool // charged against cwnd (section 14.1)
	limited   bool // sent while app- or flow-control-limited (section 15.1)
	padded    bool // packet carried PADDING (counts for startup charging)
	epoch     uint32
	ectMark   uint8 // 0 Not-ECT, 2 ECT(0)
	isMTU     bool  // packet is an MTU_PROBE carrier
	mtuProbe  uint64
	blocked   bool // carried DATA_BLOCKED; requeue flag on async-send rollback
	items     []lossItem
	// Global list ordered by packet number; per-path list ordered by
	// send ordinal (same order — ordinals increase per path).
	livePrev, liveNext *sentPkt
	pathPrev, pathNext *sentPkt
}

// sentLedger is the bounded recent packet-number history plus the live
// sent table (spec section 13.3). Packet numbers are allocated densely so a
// PN modulo ring is a complete status window. Live entries are also held
// on a PN-ordered intrusive list and per-path ordinal-ordered lists rooted
// in the path records, so ACK, loss, and retention scans visit matching
// entries directly instead of iterating the map.
type sentLedger struct {
	hist     [compactHistory]sentStatus
	largest  int64 // highest allocated PN, -1 when none
	live     map[uint64]*sentPkt
	liveHead *sentPkt // lowest PN live entry
	liveTail *sentPkt
	paths    *[wire.MaxPaths]pathRec // per-path live lists, set by owner
	liveMax  int
	nLive    int
	liveEl   int // live ack-eliciting entries
}

const compactHistory = wire.CompactTXHistoryEntries // 256
const defaultLiveMax = 4096

func newSentLedger(liveMax int) *sentLedger {
	if liveMax <= 0 {
		liveMax = defaultLiveMax
	}
	l := &sentLedger{largest: -1, liveMax: liveMax}
	for i := range l.hist {
		l.hist[i] = sentUnused
	}
	l.live = make(map[uint64]*sentPkt, 256)
	return l
}

// floor is the lowest PN still covered by the history window.
func (l *sentLedger) floor() uint64 {
	if l.largest < compactHistory {
		return 0
	}
	return uint64(l.largest) - compactHistory + 1
}

// reserve allocates PN and marks it RESERVED.
func (l *sentLedger) reserve(pn uint64) {
	l.hist[pn%compactHistory] = sentReserved
	if int64(pn) > l.largest {
		l.largest = int64(pn)
	}
}

// set updates the history status of an allocated PN. Below-floor writes
// are skipped: the ring slot belongs to a newer PN by then.
func (l *sentLedger) set(pn uint64, s sentStatus) {
	if int64(pn) > l.largest || pn < l.floor() {
		return
	}
	fsmSentSet(&l.hist[pn%compactHistory], s)
}

// status returns the recorded status of an allocated PN, or sentUnused when
// the PN is below the verification floor (unverifiable).
func (l *sentLedger) status(pn uint64) sentStatus {
	if int64(pn) > l.largest {
		return sentUnused // beyond largest is not a real allocation
	}
	if pn < l.floor() {
		return sentUnused // unverifiable — below floor
	}
	return l.hist[pn%compactHistory]
}

// liveFull reports whether the live table admission bound is reached.
func (l *sentLedger) liveFull() bool { return l.nLive >= l.liveMax }

// outstanding reports whether any live entry is ack-eliciting.
func (l *sentLedger) outstanding() bool { return l.liveEl > 0 }

// linkLive inserts p into the global PN-ordered list and its path's
// ordinal-ordered list. Both orders match emission order, so insertion
// is a tail append; an out-of-order insertion (test injection) falls
// back to a bounded backward scan.
func (l *sentLedger) linkLive(p *sentPkt) {
	if l.liveTail == nil || p.pn > l.liveTail.pn {
		p.livePrev = l.liveTail
		if l.liveTail != nil {
			l.liveTail.liveNext = p
		} else {
			l.liveHead = p
		}
		l.liveTail = p
	} else {
		cur := l.liveTail
		for cur.livePrev != nil && cur.livePrev.pn > p.pn {
			cur = cur.livePrev
		}
		p.livePrev, p.liveNext = cur.livePrev, cur
		if cur.livePrev != nil {
			cur.livePrev.liveNext = p
		} else {
			l.liveHead = p
		}
		cur.livePrev = p
	}
	if l.paths != nil && int(p.path) < len(l.paths) {
		pr := &l.paths[p.path]
		if pr.liveTail == nil || p.ordinal > pr.liveTail.ordinal {
			p.pathPrev = pr.liveTail
			if pr.liveTail != nil {
				pr.liveTail.pathNext = p
			} else {
				pr.liveHead = p
			}
			pr.liveTail = p
		} else {
			cur := pr.liveTail
			for cur.pathPrev != nil && cur.pathPrev.ordinal > p.ordinal {
				cur = cur.pathPrev
			}
			p.pathPrev, p.pathNext = cur.pathPrev, cur
			if cur.pathPrev != nil {
				cur.pathPrev.pathNext = p
			} else {
				pr.liveHead = p
			}
			cur.pathPrev = p
		}
	}
}

// unlinkLive detaches p from both lists. Safe on unlinked entries.
func (l *sentLedger) unlinkLive(p *sentPkt) {
	if p.livePrev != nil {
		p.livePrev.liveNext = p.liveNext
	} else if l.liveHead == p {
		l.liveHead = p.liveNext
	}
	if p.liveNext != nil {
		p.liveNext.livePrev = p.livePrev
	} else if l.liveTail == p {
		l.liveTail = p.livePrev
	}
	p.livePrev, p.liveNext = nil, nil
	if l.paths != nil && int(p.path) < len(l.paths) {
		pr := &l.paths[p.path]
		if p.pathPrev != nil {
			p.pathPrev.pathNext = p.pathNext
		} else if pr.liveHead == p {
			pr.liveHead = p.pathNext
		}
		if p.pathNext != nil {
			p.pathNext.pathPrev = p.pathPrev
		} else if pr.liveTail == p {
			pr.liveTail = p.pathPrev
		}
	}
	p.pathPrev, p.pathNext = nil, nil
}

// track inserts a live entry for an emitted packet.
func (l *sentLedger) track(p *sentPkt) {
	l.live[p.pn] = p
	l.nLive++
	if p.eliciting {
		l.liveEl++
	}
	l.linkLive(p)
	l.set(p.pn, sentEmitted)
}

// release removes a live entry and records the terminal history status.
func (l *sentLedger) release(pn uint64, s sentStatus) {
	if sp, ok := l.live[pn]; ok {
		delete(l.live, pn)
		l.nLive--
		if sp.eliciting {
			l.liveEl--
		}
		l.unlinkLive(sp)
		putSentPkt(sp)
	}
	l.set(pn, s)
}

// get returns the live entry or nil.
func (l *sentLedger) get(pn uint64) *sentPkt {
	p, _ := l.live[pn]
	return p
}
