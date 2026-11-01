package engine

import (
	"math/bits"

	"gosuda.org/grt/internal/wire"
)

// sendState is the section-12.5 send-half FSM.
type sendState uint8

const (
	sendOpen sendState = iota
	sendFin
	sendReset
	sendDone
)

// recvState is the section-12.6 receive-half FSM.
type recvState uint8

const (
	recvOpen recvState = iota
	recvFin
	recvReset
	recvDone
)

// sendHalf is one direction's outbound byte state.
//
// Byte tracking (spec sections 12.4–12.5):
//   - unacked: every accepted byte not yet acknowledged (emitted or not).
//   - unsent: accepted but never emitted.
//   - retx: emitted then declared lost; awaiting retransmission.
//
// unacked = unsent ∪ in-flight ∪ retx. txHigh is the reserved high-water:
// flow credit is charged once at first emission and never rolls back.
type sendHalf struct {
	state      sendState
	buf        []byte // accepted app bytes
	head       int    // index in buf where valid data begins (bufBase is stream offset of buf[head])
	bufBase    uint64 // stream offset of buf[head]
	end        uint64 // one past the last accepted byte
	txHigh     uint64 // reserved high-water: highest emitted offset end
	unacked    rangeSet
	unsent     rangeSet
	retx       rangeSet
	limit      uint64 // peer's absolute MAX_STREAM_DATA
	final      uint64
	finEmitted bool
	finPending bool // FIN obligation outstanding
	resetFinal uint64
	resetCode  uint32
	stopReq    bool
	drr        uint64 // carried deficit for fair scheduling (section 14.5)
	drrBuild   uint64 // packet-build tag: quantum already credited
}

// bytes returns the buffered copy of [lo,hi); caller guarantees coverage.
func (s *sendHalf) bytes(lo, hi uint64) []byte {
	start := s.head + int(lo-s.bufBase)
	end := s.head + int(hi-s.bufBase)
	return s.buf[start:end]
}

// live reports whether this half can still take app writes.
func (s *sendHalf) live() bool { return s.state == sendOpen }

// advanceBase drops the ACKed prefix of buf.
func (s *sendHalf) advanceBase() {
	lo, _, ok := s.unacked.first()
	want := s.end
	if ok {
		want = lo
	}
	if s.state == sendReset || s.state == sendDone {
		want = s.end
	}
	if want > s.bufBase {
		diff := int(want - s.bufBase)
		s.head += diff
		s.bufBase = want
		if s.head >= len(s.buf) {
			s.buf = s.buf[:0]
			s.head = 0
		} else if s.head >= 32768 && s.head >= len(s.buf)/2 {
			rem := copy(s.buf, s.buf[s.head:])
			s.buf = s.buf[:rem]
			s.head = 0
		}
	}
}

// maxRxSegs bounds the retained out-of-order runs per receive half
// (section 12.3: bounded reassembly). When the table is full the
// closest pair merges across its gap, so adversarial sparse fragments
// cannot grow entry count or per-insert scan cost without bound.
const maxRxSegs = 256

// rxSeg is one buffered run covering [off, off+len(data)). valid is the
// run's present bitmap: bit i set means data[i] holds a received byte.
// A run may span not-yet-received holes after a cap merge. Runs are
// kept sorted by off, pairwise disjoint, and never abutting.
//
// When ref is set, data is a subslice of a refcounted decrypted-packet
// buffer instead of owned storage: the run was inserted verbatim from a
// single packet (the common in-order case) and retains its backing until
// consumed. Referenced runs never merge in place or enter staging —
// merges copy them into owned storage and release the reference.
type rxSeg struct {
	off   uint64
	data  []byte
	valid []byte
	full  bool // no holes: valid is all-ones over data
	ref   *ptRef
}

// ptRef tracks one decrypted-plaintext buffer shared by the segments
// that reference it. The packet buffer returns to the pool when the
// last referencing segment is consumed or discarded — buf is nil from
// that point on, which is also how a commit tells the buffer was freed
// mid-transaction. Owner: the connection goroutine — n is never touched
// concurrently. The ref itself is plain heap: it lives exactly as long
// as some segment or the committing packet references it.
type ptRef struct {
	buf []byte
	n   int
}

// releaseSeg drops s's plaintext reference, returning the backing
// buffer to the pool when it was the last user.
func (r *recvHalf) releaseSeg(s *rxSeg) {
	if s.ref == nil {
		return
	}
	ref := s.ref
	s.ref = nil
	ref.n--
	if ref.n == 0 && ref.buf != nil {
		PutPacketBuf(ref.buf)
		ref.buf = nil
	}
}

// releaseAll discards every retained run, dropping their references.
func (r *recvHalf) releaseAll() {
	for i := range r.segs {
		r.releaseSeg(&r.segs[i])
		r.segs[i] = rxSeg{}
	}
	r.segs = nil
	r.pending = 0
}

func segEnd(s *rxSeg) uint64 { return s.off + uint64(len(s.data)) }

// segHas reports whether position i of s holds a received byte.
func segHas(s *rxSeg, i uint64) bool {
	if s.full {
		return true
	}
	return s.valid[i>>3]>>(i&7)&1 != 0
}

// segRun returns the length of the contiguous received run starting at
// position d of s (d < len(s.data)); the run ends at the first hole or at
// the segment boundary.
func segRun(s *rxSeg, d uint64) uint64 {
	lim := uint64(len(s.data))
	if s.full {
		return lim - d
	}
	i := d
	for i < lim {
		// TrailingZeros over the inverted byte counts consecutive
		// received positions from bit i&7; bits below i are shifted out.
		v := ^(s.valid[i>>3] >> (i & 7))
		w := uint64(8) - (i & 7)
		ones := uint64(bits.TrailingZeros8(v))
		if ones < w {
			i += ones
			break
		}
		i += w
	}
	return min(i, lim) - d
}

// setBits marks bits [lo,hi) of bitmap b.
func setBits(b []byte, lo, hi int) {
	for lo < hi {
		if lo&7 == 0 && hi-lo >= 8 {
			b[lo>>3] = 0xFF
			lo += 8
			continue
		}
		b[lo>>3] |= 1 << (lo & 7)
		lo++
	}
}

// copyValid copies s's validity bits into dst at bit offset doff.
func copyValid(dst []byte, doff int, s *rxSeg) {
	if s.full {
		setBits(dst, doff, doff+len(s.data))
		return
	}
	sh := doff & 7
	if sh == 0 {
		for i := 0; i < len(s.valid); i++ {
			dst[(doff>>3)+i] |= s.valid[i]
		}
		return
	}
	var carry byte
	for i, v := range s.valid {
		dst[(doff>>3)+i] |= carry | v<<sh
		carry = v >> (8 - sh)
	}
	if (doff>>3)+len(s.valid) < len(dst) {
		dst[(doff>>3)+len(s.valid)] |= carry
	}
}

// recvHalf is one direction's inbound byte state.
type recvHalf struct {
	state      recvState
	base       uint64 // contiguous delivered prefix
	segs       []rxSeg
	staging    []byte // reusable backing buffer for in-order runs
	pending    uint64 // retained received bytes at offsets >= base
	high       uint64 // received high-water
	limit      uint64 // our advertised absolute bound
	final      uint64
	finalKnown bool
	resetCode  uint32
	stopped    bool // APP_STOP sent
}

// Receive-half terminal results recorded on the slot: a stream's EOF or
// RESET remains reportable to the application after the half itself was
// released or the generation retired (section 12.6 terminal states).
const (
	termNone  uint8 = 0
	termEOF   uint8 = 1
	termReset uint8 = 2
)

// streamSlot is one slot table entry (spec section 21.1).
type streamSlot struct {
	state     slotState
	gen       uint64
	uni       bool
	initiator uint8 // initiator bit of the CURRENT generation's stream
	send      *sendHalf
	recv      *recvHalf
	openAcked bool   // initiator: an OPEN-carrying packet was ACKed
	openBuild uint64 // packet-build tag: OPEN already emitted this packet
	// readTerm/readTermGen record the last terminated generation's
	// application-visible receive result so a stale handle still reads
	// EOF/reset rather than an opaque close (section 21.1: the slot
	// preserves enough retirement metadata to respond idempotently).
	readTerm    uint8
	readTermGen uint64
}

// termReadErr is the recorded terminal result for a stale consume.
func (s *streamSlot) termReadErr() error {
	if s.readTerm == termReset {
		return errStreamReset
	}
	return errEOF
}

// slotTable holds the four initiator/direction classes of slots.
type slotTable struct {
	slots [4][]*streamSlot
	// Absolute slot-count limits we granted the peer (per class) and the
	// peer granted us.
	localLimit [4]uint32
	peerLimit  [4]uint32
}

func classIdx(uni bool, initiator uint8) uint8 {
	u := uint8(0)
	if uni {
		u = 1
	}
	return u<<1 | (initiator & 1)
}

// slotOf returns the slot record for a handle's (class, slot), creating the
// EMPTY record if within the table bounds.
func (t *slotTable) slotOf(class uint8, slot uint16) *streamSlot {
	l := t.slots[class]
	if int(slot) >= len(l) {
		return nil
	}
	return l[slot]
}

// ensureSlot grows the class table to include slot and returns the record.
// It returns nil when slot is at/above the table bound.
func (t *slotTable) ensureSlot(class uint8, slot uint16) *streamSlot {
	if slot >= wire.MaxStreamSlots {
		return nil
	}
	l := t.slots[class]
	for uint16(len(l)) <= slot {
		l = append(l, &streamSlot{state: slotEmpty})
	}
	t.slots[class] = l
	return l[slot]
}

// freeSlot scans the class for an EMPTY slot below the peer-granted limit.
func (t *slotTable) freeSlot(class uint8, limit uint32) (uint16, bool) {
	for i := uint32(0); i < limit && i < wire.MaxStreamSlots; i++ {
		s := t.ensureSlot(class, uint16(i))
		if s == nil {
			return 0, false
		}
		if s.state == slotEmpty {
			return uint16(i), true
		}
	}
	return 0, false
}
