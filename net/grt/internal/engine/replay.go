package engine

import (
	"math/bits"

	"gosuda.org/grt/internal/wire"
)

// replayWin is the receive-side committed-PN replay window (spec section
// 13.1). Its width is the locally advertised receive_packet_history — a
// power of two between MinReplayWindow and MaxReplayWindow — so the
// bitmap is exactly as wide as the value the peer relies on.
type replayWin struct {
	bits         []uint64
	width        uint64 // negotiated window width; power of two
	largest      uint64
	init         bool
	lastCommitUS uint64 // commit time of the newest committed PN
}

// ensure lazily allocates the bitmap at the negotiated width. A width
// that is not a power of two inside the negotiated range falls back to
// the protocol minimum so a misconstructed receiver still behaves
// honestly — advertising more tracking than implemented is never allowed.
func (w *replayWin) ensure() {
	if w.bits != nil {
		return
	}
	if w.width < wire.MinReplayWindow || w.width > wire.MaxReplayWindow ||
		w.width&(w.width-1) != 0 {
		w.width = wire.MinReplayWindow
	}
	w.bits = make([]uint64, w.width/64)
}

// bit locates pn's flag: position pn mod width, word pos>>6, bit pos&63.
// width is a power-of-two multiple of 64, so pos&63 == pn&63.
func (w *replayWin) bit(pn uint64) (uint64, uint64) {
	pos := pn & (w.width - 1)
	return pos >> 6, 1 << (pn & 63)
}

// floorFor returns the lowest PN a window anchored at largest verifies.
func floorFor(largest, width uint64) uint64 {
	if largest < width {
		return 0
	}
	return largest - width + 1
}

// floor returns the lowest PN the window can verify.
func (w *replayWin) floor() uint64 {
	if !w.init {
		return 0
	}
	return floorFor(w.largest, w.width)
}

// fresh reports whether pn is inside the window and not yet committed.
// PNs below the floor are stale (rejected as replay-uncertain).
func (w *replayWin) fresh(pn uint64) bool {
	if !w.init || pn > w.largest {
		return true
	}
	if pn < w.floor() {
		return false
	}
	i, m := w.bit(pn)
	return w.bits[i]&m == 0
}

// clearBelow clears the bits of every PN strictly below floor — the
// packets the window slid past. Without this a stale bit aliases the
// in-window PN that shares its position: the unseen PN would report as
// committed, producing a false ACK, a duplicate drop, and a permanent
// stream hole (section 13.1).
func (w *replayWin) clearBelow(floor uint64) {
	old := w.floor()
	if floor <= old {
		return
	}
	if floor-old >= w.width {
		clear(w.bits)
		return
	}
	if old&63 != 0 {
		firstLen := 64 - int(old&63)
		if uint64(firstLen) > floor-old {
			firstLen = int(floor - old)
		}
		var mask uint64
		if firstLen == 64 {
			mask = ^uint64(0)
		} else {
			mask = ((uint64(1) << firstLen) - 1) << (old & 63)
		}
		w.bits[(old&(w.width-1))>>6] &^= mask
		old += uint64(firstLen)
	}
	for old+64 <= floor {
		w.bits[(old&(w.width-1))>>6] = 0
		old += 64
	}
	if old < floor {
		rem := int(floor - old)
		mask := (uint64(1) << rem) - 1
		w.bits[(old&(w.width-1))>>6] &^= mask
	}
}

// commit marks pn as received. It must only be called for fresh PNs.
func (w *replayWin) commit(pn uint64) {
	w.ensure()
	switch {
	case !w.init:
		w.init = true
		w.largest = pn
	case pn > w.largest:
		// Slide the window: vacated bits are cleared before the new
		// largest reuses their positions. Clearing must precede the
		// update because the new bit can share a vacated position.
		w.clearBelow(floorFor(pn, w.width))
		w.largest = pn
	case pn >= w.floor():
		i, m := w.bit(pn)
		w.bits[i] |= m
		return
	default:
		return // below floor: not fresh; never pollute the window
	}
	w.lastCommitUS = nowUS()
	i, m := w.bit(pn)
	w.bits[i] |= m
}

// seen reports whether pn was committed (for duplicate detection).
func (w *replayWin) seen(pn uint64) bool {
	if !w.init || pn > w.largest || pn < w.floor() {
		return false
	}
	i, m := w.bit(pn)
	return w.bits[i]&m != 0
}

// ackRanges collects up to maxRanges newest contiguous committed ranges,
// each [lo,hi] inclusive, newest first. Returns the count.
func (w *replayWin) ackRanges(out *[wire.MaxAckRanges][2]uint64, maxRanges int) int {
	if !w.init || maxRanges <= 0 {
		return 0
	}
	if maxRanges > wire.MaxAckRanges {
		maxRanges = wire.MaxAckRanges
	}
	n := 0
	pn := w.largest
	floor := w.floor()

	for n < maxRanges && pn >= floor {
		// 1. Scan downwards for the highest set bit (hi).
		for pn >= floor {
			wIdx := (pn & (w.width - 1)) >> 6
			k := pn & 63
			word := w.bits[wIdx]

			var mask uint64
			if k == 63 {
				mask = ^uint64(0)
			} else {
				mask = (uint64(1) << (k + 1)) - 1
			}
			if pn-uint64(k) <= floor {
				mask &^= (uint64(1) << (floor & 63)) - 1
			}

			sub := word & mask
			if sub != 0 {
				setK := 63 - bits.LeadingZeros64(sub)
				pn -= uint64(k - uint64(setK))
				break
			}
			if pn-uint64(k) <= floor || pn < uint64(k+1) {
				return n
			}
			pn -= uint64(k + 1)
		}
		if pn < floor {
			break
		}

		hi := pn

		// 2. Scan downwards for the contiguous range of 1s until the first 0-bit or floor.
		for pn >= floor {
			wIdx := (pn & (w.width - 1)) >> 6
			k := pn & 63
			inv := ^w.bits[wIdx]

			var mask uint64
			if k == 63 {
				mask = ^uint64(0)
			} else {
				mask = (uint64(1) << (k + 1)) - 1
			}
			if pn-uint64(k) <= floor {
				mask &^= (uint64(1) << (floor & 63)) - 1
			}

			subInv := inv & mask
			if subInv != 0 {
				zeroK := 63 - bits.LeadingZeros64(subInv)
				pn -= uint64(k - uint64(zeroK+1))
				break
			}
			if pn-uint64(k) <= floor {
				pn = floor
				break
			}
			pn -= uint64(k)
			if pn == 0 {
				break
			}
			prevIdx := ((pn - 1) & (w.width - 1)) >> 6
			if w.bits[prevIdx]&(uint64(1)<<63) == 0 {
				break
			}
			pn--
		}

		out[n] = [2]uint64{pn, hi}
		n++

		if pn <= floor || pn == 0 {
			break
		}
		pn--
	}
	return n
}

// largestObserved returns the largest committed PN.
func (w *replayWin) largestObserved() (uint64, bool) {
	return w.largest, w.init
}

// ackState tracks ACK generation obligations (spec section 13.2).
//
// ack_ratio adapts within the authenticated set {2,4,8,16,32} up to the
// advertised ceiling using the reference controller: newly committed
// eliciting child datagrams are sampled over 10ms windows into a filtered
// rate r (r=(3r+sample)/4), and the ratio is the largest allowed value
// not exceeding max(2, floor(r*max_ack_delay_us/2000000)). The ratio
// resets to 2 after one RTT without eliciting data or on any immediate
// (gap/CE/progress) trigger.
type ackState struct {
	elicitSinceAck int    // eliciting child datagrams committed since last ACK
	dueUS          uint64 // soft deadline for the oldest unacked eliciting datagram
	dueValid       bool
	immediate      bool // gap/CE/immediate trigger
	// adaptive ratio state
	ratio    uint16 // current ack_ratio ∈ {2,4,8,16,32}
	maxRatio uint16 // advertised ceiling
	lastUS   uint64 // last eliciting commit time (idle detection)
	winUS    uint64 // current sampling window anchor
	winCnt   uint32 // eliciting commits inside the window
	filtRate uint64 // filtered eliciting packets per second
}

// init binds the negotiated local parameters into the ack state.
func (a *ackState) init(local *wire.Params) {
	a.ratio = local.InitialAckRatio
	if !validRatio(a.ratio) {
		a.ratio = 2
	}
	a.maxRatio = local.MaxAckRatio
	if !validRatio(a.maxRatio) {
		a.maxRatio = wire.DefaultMaxAckRatio
	}
	if a.ratio > a.maxRatio {
		a.ratio = a.maxRatio
	}
}

func validRatio(v uint16) bool {
	switch v {
	case 2, 4, 8, 16, 32:
		return true
	}
	return false
}

// ackSampleUS is the reference controller's rate-sampling window.
const ackSampleUS = 10000

func (a *ackState) onEliciting(nowUS uint64, srttUS uint64, maxDelayUS uint64, immediate bool) {
	a.elicitSinceAck++
	// Idle for more than one RTT, or a gap/CE/progress trigger: the
	// controller restarts at ratio 2 (section 13.2).
	if (a.lastUS != 0 && nowUS-a.lastUS > srttUS) || immediate {
		a.ratio = 2
	}
	a.lastUS = nowUS
	if immediate {
		a.immediate = true
	}
	if a.winUS == 0 {
		a.winUS = nowUS
	}
	a.winCnt++
	if el := nowUS - a.winUS; el >= ackSampleUS {
		rate := uint64(a.winCnt) * 100 // commits per second over 10ms
		a.filtRate = (3*a.filtRate + rate) / 4
		a.winUS, a.winCnt = nowUS, 0
		// Largest allowed ratio ≤ max(2, floor(r·max_ack_delay/2e6)).
		bound := a.filtRate * maxDelayUS / 2000000
		best := uint64(2)
		for _, r := range [5]uint64{32, 16, 8, 4} {
			if uint64(r) <= bound && r <= uint64(a.maxRatio) {
				best = r
				break
			}
		}
		a.ratio = uint16(best)
	}
	if !a.dueValid {
		a.dueValid = true
		a.dueUS = nowUS + maxDelayUS
	}
}

// wantAck reports whether an ACK should be emitted now.
func (a *ackState) wantAck(nowUS uint64) bool {
	if a.elicitSinceAck == 0 {
		return false
	}
	if a.immediate || a.elicitSinceAck >= int(a.ratio) {
		return true
	}
	return a.dueValid && nowUS >= a.dueUS
}

func (a *ackState) reset() {
	a.elicitSinceAck = 0
	a.dueValid = false
	a.immediate = false
}
