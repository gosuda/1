package engine

import (
	"crypto/sha256"
	"encoding/binary"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// emitProgressFrames adds handshake/key/path-progress work first
// (section 14.5 priority 1). pi is the packet's egress path: path-pinned
// items emit only on their pinned path. KEY_COMMIT is excluded here — it
// rides dedicated STAGED-epoch packets built by txStagedCommit.
func (c *Conn) emitProgressFrames(fb *wire.FrameBuilder, tb *txBudget, maxPlain int, pi int) {
	core := c.core
	// FINISH rides every client packet until its obligation is ACKed;
	// HANDSHAKE_DONE likewise stays reliable until ACKed, independent of
	// the lifecycle state it was queued under.
	if core.role == roleClient {
		if id, ok := c.ctrlIDOf(ctrlFinish, 0, 0); ok {
			c.emitCtrlItem(fb, tb, id)
		}
	}
	if core.role == roleServer {
		if id, ok := c.ctrlIDOf(ctrlHandshakeDone, 0, 0); ok {
			c.emitCtrlItem(fb, tb, id)
		}
	}
	// Path validation items (challenge retries / responses / intents).
	rt := &core.refresh
	for i := range core.ctrl {
		it := &core.ctrl[i]
		if !it.used {
			continue
		}
		if it.kind == ctrlKeyConfirm {
			// KEY_CONFIRM rides every new-epoch packet until a
			// confirm-carrying packet is acknowledged (section 19.4).
			if rt.committed && !rt.confirmAcked {
				c.emitCtrlItem(fb, tb, uint32(i))
			}
			continue
		}
		if it.live || !c.ctrlForPath(uint32(i), pi) {
			continue
		}
		switch it.kind {
		case ctrlPathResponse, ctrlPathChallenge, ctrlPathIntent,
			ctrlPathSelected, ctrlKeyRequest, ctrlCryptoFrag:
			c.emitCtrlItem(fb, tb, uint32(i))
		}
	}
}

// emitAck writes the newest ≤8 committed ranges as an ACK frame.
func (c *Conn) emitAck(fb *wire.FrameBuilder, tb *txBudget, now uint64) {
	core := c.core
	var ranges [wire.MaxAckRanges][2]uint64
	n := core.replay.ackRanges(&ranges, int(core.local.MaxAckRanges))
	if n == 0 {
		return
	}
	var a wire.AckFrame
	// ack_delay_us: time since the largest committed PN was committed,
	// clamped to our advertised max.
	delay := uint64(0)
	if core.replay.init {
		if now > core.replay.lastCommitUS {
			delay = now - core.replay.lastCommitUS
		}
		if d := uint64(core.local.MaxAckDelayUS); delay > d {
			delay = d
		}
	}
	a.AckDelayUS = uint32(delay)
	if core.peer.FeatureFlags&wire.FeatureECN != 0 {
		// Committed-DATA counters are connection-level (section 15.4).
		a.ECT0, a.CE = core.rxECT[0], core.rxECT[2]
		a.ECT1 = core.rxECT[1]
	}
	a.Count = n
	copy(a.Ranges[:], ranges[:])
	bodyLen := wire.AckBodyLen(n)
	if dst, ok := fb.Reserve(wire.FrameAck, 0, bodyLen); ok {
		wire.BuildAckBody(dst, &a)
		// The acker resets only if this packet commits emission.
		tb.ackSent = true
	}
}

// emitCtrl flushes pending non-progress control items in queue order.
// Items pinned to another path are skipped (they emit via txPathPacket).
func (c *Conn) emitCtrl(fb *wire.FrameBuilder, tb *txBudget, maxPlain int, pi int) {
	core := c.core
	for i := range core.ctrl {
		it := &core.ctrl[i]
		if !it.used || it.live || !c.ctrlForPath(uint32(i), pi) {
			continue
		}
		switch it.kind {
		case ctrlPathResponse, ctrlPathChallenge, ctrlPathIntent,
			ctrlPathSelected, ctrlFinish, ctrlHandshakeDone,
			ctrlKeyCommit, ctrlKeyConfirm, ctrlKeyRequest, ctrlCryptoFrag:
			continue // handled by emitProgressFrames / txStagedCommit
		}
		c.emitCtrlItem(fb, tb, uint32(i))
	}
	if core.wantDataBlocked {
		if dst, ok := fb.Reserve(wire.FrameDataBlocked, 0, 8); ok {
			binary.BigEndian.PutUint64(dst, core.connTxLim)
			core.wantDataBlocked = false
			tb.blockedEmit = true
		}
	}
}

// emitCtrlItem serializes one control item into a frame. A returned
// lossItem keeps the obligation until packet ACK.
func (c *Conn) emitCtrlItem(fb *wire.FrameBuilder, tb *txBudget, id uint32) {
	core := c.core
	it := &core.ctrl[id]
	// Stream-scoped items die when their generation moved on; the
	// initiator's half-control frames additionally require the stream's
	// OPEN in the same packet until it is acknowledged (section 12.2).
	switch it.kind {
	case ctrlMaxStreamData, ctrlReset, ctrlStopSending, ctrlRetire, ctrlRetireAck:
		sid := wire.StreamID(it.a)
		s := core.slots.slotOf(sid.Class(), sid.Slot())
		if s == nil || s.gen != sid.Generation() {
			it.used = false
			return
		}
		switch it.kind {
		case ctrlMaxStreamData, ctrlReset, ctrlStopSending, ctrlRetire:
			if !c.emitOpen(fb, tb, s, sid) {
				return
			}
		}
	}
	var body [80]byte
	var typ uint16
	var n int
	emit := func(t uint16, nb int) bool {
		if dst, ok := fb.Reserve(t, 0, nb); ok {
			copy(dst, body[:nb])
			return true
		}
		return false
	}
	switch it.kind {
	case ctrlMaxData:
		typ, n = wire.FrameMaxData, 8
		binary.BigEndian.PutUint64(body[:], it.b)
	case ctrlMaxStreamData:
		typ, n = wire.FrameMaxStreamData, 16
		wire.BuildMaxStreamData(body[:], it.a, it.b)
	case ctrlMaxSlots:
		typ, n = wire.FrameMaxSlots, 8
		wire.BuildMaxSlots(body[:], uint8(it.a), uint32(it.b))
	case ctrlReset:
		sid := wire.StreamID(it.a)
		s := core.slots.slotOf(sid.Class(), sid.Slot())
		if s == nil || s.send == nil {
			it.used = false
			return
		}
		typ, n = wire.FrameResetStream, 24
		wire.BuildResetStream(body[:], it.a, s.send.resetFinal, it.c)
	case ctrlStopSending:
		typ, n = wire.FrameStopSending, 16
		wire.BuildStopSending(body[:], it.a, it.c)
	case ctrlRetire:
		typ, n = wire.FrameStreamRetire, 8
		binary.BigEndian.PutUint64(body[:], it.a)
	case ctrlRetireAck:
		typ, n = wire.FrameRetireAck, 8
		binary.BigEndian.PutUint64(body[:], it.a)
	case ctrlFinish:
		mac := gc.ClientFinishMAC(core.finKey, core.t1, core.cliReset)
		typ, n = wire.FrameFinish, 48
		copy(body[:32], mac[:])
		copy(body[32:], core.cliReset[:])
	case ctrlHandshakeDone:
		mac := gc.ServerDoneMAC(core.doneKey, core.t1, core.hs.cliFinishMAC)
		typ, n = wire.FrameHandshakeDone, 32
		copy(body[:], mac[:])
	case ctrlNewCID:
		typ, n = wire.FrameNewCID, 40
		// it.b carries the CID sequence; it.a the CID value;
		// retire_prior_to stays zero (we never preemptively retire).
		wire.BuildNewCID(body[:], it.b, 0, it.a, it.tok)
	case ctrlRetireCID:
		typ, n = wire.FrameRetireCID, 8
		binary.BigEndian.PutUint64(body[:], it.a)
	case ctrlNewToken:
		// Full 64-byte token travels in it.body.
		if fb.Add(wire.FrameNewToken, 0, wire.TokenBytes, func(d []byte) { copy(d, it.body) }) {
			it.live = true
			tb.items = append(tb.items, lossItem{kind: itemCtrl,
				ctrlID: id, ctrlGen: it.agen})
		}
		return
	case ctrlNewTicket:
		if fb.Add(wire.FrameNewTicket, 0, len(it.body), func(d []byte) { copy(d, it.body) }) {
			it.live = true
			tb.items = append(tb.items, lossItem{kind: itemCtrl,
				ctrlID: id, ctrlGen: it.agen})
		}
		return
	case ctrlPathChallenge:
		typ, n = wire.FramePathChallenge, 16
		copy(body[:], it.tok[:])
		tb.probe = true
	case ctrlPathResponse:
		typ, n = wire.FramePathResponse, 16
		copy(body[:], it.tok[:])
		tb.probe = true
	case ctrlPathIntent:
		typ, n = wire.FramePathIntent, 24
		// a2 is the migration sequence, b the cutover barrier, c the
		// intent flags.
		wire.BuildPathIntent(body[:], it.a2, it.b, uint32(it.c))
	case ctrlPathSelected:
		typ, n = wire.FramePathSelected, 16
		wire.BuildPathSelected(body[:], it.a2, uint16(it.c))
	case ctrlPing:
		typ, n = wire.FramePing, 0
	case ctrlKeyRequest:
		typ, n = wire.FrameKeyRequest, 8
		binary.BigEndian.PutUint32(body[:], it.c)
		binary.BigEndian.PutUint32(body[4:], 0)
	case ctrlCryptoFrag:
		// it.a is the outstanding (owed) fragment bitmap; it.a2 tracks
		// fragments carried by unresolved packets so one ACK only clears
		// its own fragment and a loss restores retransmission exactly.
		avail := it.a &^ it.a2
		if avail == 0 {
			if it.a == 0 {
				it.used = false
				it.body = nil
			}
			return
		}
		total := len(it.body)
		nfrag := (total + wire.CryptoFragChunk - 1) / wire.CryptoFragChunk
		fi := 0
		for fi < nfrag && avail&(1<<uint(fi)) == 0 {
			fi++
		}
		if fi >= nfrag {
			return
		}
		lo := fi * wire.CryptoFragChunk
		hi := min(lo+wire.CryptoFragChunk, total)
		var cf wire.CryptoFrag
		cf.RefreshID = it.tok
		cf.Kind = uint8(it.b)
		cf.TotalLength = uint32(total)
		cf.MessageHash = sha256.Sum256(it.body)
		cf.Index = uint16(fi)
		cf.Count = uint16(nfrag)
		blen := wire.CryptoFragPrefix + (hi - lo)
		if fb.Add(wire.FrameCryptoFragment, 0, blen, func(d []byte) {
			wire.BuildCryptoFrag(d, &cf)
			copy(d[wire.CryptoFragPrefix:], it.body[lo:hi])
		}) {
			it.a2 |= 1 << uint(fi) // mark in-flight; cleared on resolution
			tb.items = append(tb.items, lossItem{kind: itemCtrl,
				ctrlID: id, ctrlGen: it.agen, lo: uint64(fi)})
		}
		return
	case ctrlKeyCommit:
		typ, n = wire.FrameKeyCommit, 56
		wire.BuildKeyCommit(body[:], uint32(it.a), it.tok, c.refreshCommitMAC())
	case ctrlKeyConfirm:
		typ, n = wire.FrameKeyConfirm, 56
		wire.BuildKeyCommit(body[:], uint32(it.a), it.tok, c.refreshConfirmMAC())
	case ctrlParkRequest:
		typ, n = wire.FrameParkRequest, 16
		wire.BuildParkRequest(body[:], it.a, uint32(it.b))
	case ctrlParkGrant:
		typ, n = wire.FrameParkGrant, 16
		wire.BuildParkGrant(body[:], it.a, uint32(it.b), uint16(it.c))
	case ctrlParkWake:
		typ, n = wire.FrameParkWake, 16
		wire.BuildParkWake(body[:], it.a, it.b)
	case ctrlParkAwake:
		typ, n = wire.FrameParkAwake, 16
		wire.BuildParkWake(body[:], it.a, it.b)
	case ctrlClose:
		typ, n = wire.FrameClose, 24
		wire.BuildClose(body[:], uint32(core.closeCode), 0, c.diagHash())
	default:
		it.used = false
		return
	}
	if emit(typ, n) {
		it.live = true
		li := lossItem{kind: itemCtrl, ctrlID: id, ctrlGen: it.agen}
		// The loss reference records the semantics this packet actually
		// emitted so an ACK covering an older emission cannot retire a
		// refreshed obligation (section 12.4 "highest unsatisfied
		// value" and the section-11 reliability column).
		switch it.kind {
		case ctrlMaxData, ctrlMaxStreamData, ctrlMaxSlots:
			li.lo = it.b // absolute limit carried by this packet
		case ctrlPathIntent:
			// The emitted flags must resolve an ACK only for this exact
			// obligation: a SELECT=0-carrying packet never settles a
			// SELECT=1 upgrade queued under the same sequence.
			li.flags = uint16(it.c)
		case ctrlPathSelected:
			li.lo = uint64(it.c) // emitted selection status
		case ctrlKeyRequest:
			li.lo = uint64(it.c) // emitted requested epoch
		case ctrlParkWake, ctrlParkAwake:
			li.lo = it.b // emitted wake_seq
		}
		tb.items = append(tb.items, li)
	}
}

// emitStreamData packs stream ranges within flow limits, retransmissions
// before new data (priorities 4/5, section 14.1). Streams are served in
// deficit round robin order (section 14.5): each sweep resumes the scan
// at the rotating cursor, a send half is credited quantum (2·M) at most
// once per packet build, and emission is limited by its carried deficit —
// so a continuously eligible stream cannot starve the others.
func (c *Conn) emitStreamData(fb *wire.FrameBuilder, tb *txBudget, maxPlain int) {
	c.streamSweep(fb, tb, maxPlain, false) // lost ranges
	c.streamSweep(fb, tb, maxPlain, true)  // new data
}

// streamSweep visits each active send half once in ring order from the
// rotating cursor. newData=false emits retransmissions only; newData=true
// emits any retransmission remainder first, then new data. Both sweeps
// draw on the same per-build deficit, so service order across streams is
// one rotating round, not two independent passes.
func (c *Conn) streamSweep(fb *wire.FrameBuilder, tb *txBudget, maxPlain int, newData bool) {
	core := c.core
	total := 0
	for class := uint8(0); class < 4; class++ {
		total += len(core.slots.slots[class])
	}
	if total == 0 {
		return
	}
	quantum := 2 * uint64(core.paths[core.activePth].m)
	class, si := core.schedClass, int(core.schedIdx)
	for seen := 0; seen < total; seen++ {
		for si >= len(core.slots.slots[class]) {
			si, class = 0, (class+1)&3
		}
		s := core.slots.slots[class][si]
		si++
		core.schedClass, core.schedIdx = class, uint16(si)
		if s.state != slotActive || s.send == nil || fb.Remaining() < 2 {
			continue
		}
		sh := s.send
		if sh.drrBuild != core.txBuildID {
			sh.drrBuild = core.txBuildID
			sh.drr += quantum
			if sh.drr > 2*quantum {
				// A credit-blocked half must not hoard deficit: carry
				// at most one interrupted round's remainder.
				sh.drr = 2 * quantum
			}
		}
		sid := wire.MakeStreamID(s.gen, uint16(si-1), s.uni, s.initiator)
		emitted := c.emitOneStream(fb, tb, maxPlain, s, sh, sid, sh.drr, newData)
		sh.drr -= emitted
		if sh.retx.empty() && sh.unsent.empty() &&
			!(sh.finPending && !sh.finEmitted) {
			sh.drr = 0
		}
		if streamRoom(fb, maxPlain) <= 0 {
			break
		}
	}
}

// streamRoom is the remaining capacity for STREAM payload bytes in this
// packet: frame header + stream head (16) + worst-case alignment pad (7).
func streamRoom(fb *wire.FrameBuilder, maxPlain int) int {
	return maxPlain - fb.Len() - wire.FrameHeaderBytes - 16 - 7
}

// emitOpen appends STREAM_OPEN when this packet carries data or
// half-control for an initiator stream whose OPEN is not yet acknowledged
// (section 12.2): every such packet repeats the same OPEN until an
// OPEN-carrying packet is ACKed, so out-of-order arrivals cannot find a
// not-yet-open stream. openBuild dedupes repeats inside one packet.
func (c *Conn) emitOpen(fb *wire.FrameBuilder, tb *txBudget, s *streamSlot, sid wire.StreamID) bool {
	core := c.core
	if s.initiator != c.ourInitiator() || s.openAcked || s.openBuild == core.txBuildID {
		return true
	}
	dst, ok := fb.Reserve(wire.FrameStreamOpen, 0, 8)
	if !ok {
		return false
	}
	binary.BigEndian.PutUint64(dst, uint64(sid))
	s.openBuild = core.txBuildID
	tb.items = append(tb.items, lossItem{kind: itemOpen, sid: uint64(sid)})
	tb.elicit = true
	tb.appData = true
	return true
}

// emitOneStream emits one priority tier for one send half, up to budget
// payload bytes; returns the bytes emitted. newData=false emits the retx
// set only (priority 4); newData=true emits the unsent set and the
// zero-length FIN (priority 5). A retx range never carries into the new
// sweep: it either drained or exhausted the deficit in its own sweep.
func (c *Conn) emitOneStream(fb *wire.FrameBuilder, tb *txBudget, maxPlain int, s *streamSlot, sh *sendHalf, sid wire.StreamID, budget uint64, newData bool) uint64 {
	core := c.core
	// Any packet that will carry data or the FIN for this stream must first
	// carry the same OPEN while its acknowledgment is outstanding.
	set := &sh.retx
	if newData {
		set = &sh.unsent
	}
	hasWork := !set.empty() ||
		(newData && sh.finPending && !sh.finEmitted)
	if hasWork && !c.emitOpen(fb, tb, s, sid) {
		return 0
	}
	var emitted uint64
	for fb.Remaining() > 1 && !set.empty() && emitted < budget {
		lo, hi, _ := set.first()
		// room for stream data bytes: header(8) + stream-head(16) +
		// worst-case alignment pad (7)
		room := streamRoom(fb, maxPlain)
		if room <= 0 {
			return emitted
		}
		n := uint64(room)
		if hi-lo < n {
			n = hi - lo
		}
		if budget-emitted < n {
			n = budget - emitted
		}
		// Stream absolute limit: end must not exceed sh.limit.
		if lo+n > sh.limit {
			if lo >= sh.limit {
				tb.flowLimited = true // stream-credit blocked with work left
				break
			}
			n = sh.limit - lo
		}
		// Connection credit: only the part above txHigh charges.
		avail := uint64(0)
		if core.connTxLim > core.connTxUsed {
			avail = core.connTxLim - core.connTxUsed
		}
		if lo+n > sh.txHigh+avail {
			if lo >= sh.txHigh+avail {
				tb.flowLimited = true // connection credit exhausted
				break
			}
			n = sh.txHigh + avail - lo
		}
		inc := uint64(0)
		if lo+n > sh.txHigh {
			inc = lo + n - sh.txHigh
		}
		fin := sh.finPending && !sh.finEmitted && lo+n == sh.final
		data := sh.bytes(lo, lo+n)
		flags := uint16(0)
		if fin {
			flags = wire.FrameFlagStreamFin
		}
		bodyLen := 16 + len(data)
		dst, ok := fb.Reserve(wire.FrameStream, flags, bodyLen)
		if !ok {
			return emitted
		}
		wire.BuildStreamHead(dst[:16], uint64(sid), lo)
		copy(dst[16:], data)
		tb.appData = true
		set.remove(lo, lo+n)
		emitted += n
		if fin {
			sh.finEmitted = true
		}
		if inc > 0 {
			core.connTxUsed += inc
			sh.txHigh = lo + n
		}
		tb.items = append(tb.items, lossItem{
			kind: itemStream, sid: uint64(sid), lo: lo, hi: lo + n,
			flags: flags,
		})
		tb.elicit = true
	}
	// Zero-length FIN (empty stream closed at offset 0): emitted only when
	// all data ranges are exhausted and the send half is finalized. This is
	// a new emission, never part of the retransmission sweep.
	if newData && sh.finPending && !sh.finEmitted && sh.retx.empty() && sh.unsent.empty() {
		if dst, ok := fb.Reserve(wire.FrameStream, wire.FrameFlagStreamFin, 16); ok {
			wire.BuildStreamHead(dst, uint64(sid), sh.final)
			tb.appData = true
			sh.finEmitted = true
			tb.items = append(tb.items, lossItem{
				kind: itemStream, sid: uint64(sid),
				lo: sh.final, hi: sh.final, flags: wire.FrameFlagStreamFin,
			})
			tb.elicit = true
		}
	}
	return emitted
}

// emitDatagrams drains unreliable datagrams (priority 6).
func (c *Conn) emitDatagrams(fb *wire.FrameBuilder, tb *txBudget, maxPlain int) {
	core := c.core
	if core.peer.FeatureFlags&wire.FeatureDatagram == 0 {
		core.dgOut = nil
		return
	}
	for len(core.dgOut) > 0 && fb.Remaining() > 0 {
		dg := core.dgOut[0]
		room := maxPlain - fb.Len() - wire.FrameHeaderBytes - 8
		if room < 0 || len(dg) > room {
			return
		}
		bodyLen := 8 + len(dg)
		dst, ok := fb.Reserve(wire.FrameDatagram, 0, bodyLen)
		if !ok {
			return
		}
		binary.BigEndian.PutUint64(dst[:8], 0) // context 0
		copy(dst[8:], dg)
		core.dgOut = core.dgOut[1:]
		tb.elicit = true
		tb.appData = true
	}
}
