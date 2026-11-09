package engine

import "gosuda.org/grt/internal/wire"

// The control queue holds reliable logical control items (spec section
// 14.5). Monotonic credits coalesce by semantic identity; one-shot items
// stay pending until their carrying packet is ACKed. The table is bounded
// and indices are stable while an item is referenced by an in-flight
// packet's loss list.

// itemKey uniquely identifies a control obligation: kind plus scope.
type itemKey struct {
	kind uint8
	slot uint16
	path int16 // pinned path index; -1 for unpinned obligations
	gen  uint64
	seq  uint64 // migration sequence for PATH_INTENT/PATH_SELECTED
}

// queueCtrl adds or refreshes an unpinned control obligation.
func (c *Conn) queueCtrl(kind uint8, slot uint16, a, b uint64, cc uint32, tok [16]byte) bool {
	return c.queueCtrlPath(kind, -1, slot, a, b, cc, tok)
}

// queueCtrlPath adds or refreshes a control obligation. The dedupe key
// is kind plus the full scope identity — for most kinds `a` carries it
// (stream ID, slot class, CID value or sequence, park request ID) — so
// distinct same-kind obligations never merge. For monotonic credits an
// existing same-identity item is updated in place; the loss list carries
// the emitted value so an ACK of an older value does not clear the new
// one. path >= 0 pins emission to that path slot (PATH_CHALLENGE goes
// to its candidate; PATH_RESPONSE returns on the challenge's ingress path).
func (c *Conn) queueCtrlPath(kind uint8, path int16, slot uint16, a, b uint64, cc uint32, tok [16]byte) bool {
	core := c.core
	key := itemKey{kind: kind, slot: slot, path: path}
	switch kind {
	case ctrlMaxStreamData, ctrlRetire, ctrlRetireAck, ctrlReset, ctrlStopSending:
		key.gen = wire.StreamID(a).Generation()
	case ctrlPathIntent, ctrlPathSelected:
		key.seq = a
	}
	for i := range core.ctrl {
		it := &core.ctrl[i]
		if !it.used {
			continue
		}
		keyMiss := it.kind != key.kind || it.slot != key.slot ||
			it.gen != key.gen || it.a2 != key.seq || it.path != key.path
		if keyMiss {
			continue
		}
		// Token-bearing kinds carry their token in the obligation
		// identity: a different challenge/refresh transaction is a
		// different item even when kind and path match.
		switch kind {
		case ctrlPathChallenge, ctrlPathResponse, ctrlKeyCommit,
			ctrlKeyConfirm, ctrlCryptoFrag:
			if it.tok != tok {
				continue
			}
		}
		// `a` is the obligation's scope identity for most kinds — the
		// full stream ID, slot class, CID value or sequence, or park
		// request ID — so a match on the coarse key alone must not
		// merge distinct obligations: two streams sharing slot and
		// generation in different classes, two MAX_SLOTS classes, or
		// two outstanding NEW_CIDs are separate reliable work.
		// CRYPTO_FRAGMENT is exempt: its `a` is the mutable
		// outstanding-fragment bitmap, not an identity.
		if kind != ctrlCryptoFrag && it.a != a {
			continue
		}
		switch kind {
		case ctrlMaxData, ctrlMaxStreamData, ctrlMaxSlots:
			it.b = b // refresh latest value; will re-emit
			it.live = false
		case ctrlPathIntent:
			// SELECT may advance 0→1 within one sequence (section
			// 17.4): the outstanding obligation becomes the SELECT
			// intent, re-emitted under the same allocation generation.
			if cc&1 != 0 && it.c&1 == 0 {
				it.b, it.c = b, cc
			}
			if cc&1 != 0 {
				it.live = false // a (re)queued SELECT emits promptly
			}
		case ctrlPathSelected:
			// The result for a sequence is regenerated per evaluation;
			// a still-queued older status (BUSY before READY) must
			// yield to the current decision.
			if it.c != cc {
				it.c = cc
				it.live = false
			}
		case ctrlKeyRequest:
			// A later soft-limit crossing refreshes the requested
			// epoch on the outstanding obligation.
			if it.c != cc {
				it.c = cc
				it.live = false
			}
		case ctrlParkWake, ctrlParkAwake:
			// A newer wake sequence under the same request ID
			// replaces the pending payload (section 35.8).
			if it.b != b {
				it.b = b
				it.live = false
			}
		case ctrlPing:
			// Each demand (PTO probe, keepalive, blocked signal) owes a
			// fresh emission even while an earlier one is in flight.
			it.live = false
		}
		return true // identical one-shot obligation already outstanding
	}
	i, agen := c.ctrlSlot()
	if i < 0 {
		return false
	}
	core.ctrl[i] = ctrlItem{
		kind: kind, slot: slot, path: path, gen: key.gen,
		a2: key.seq, a: a, b: b, c: cc, agen: agen,
		tok: tok, used: true,
	}
	return true
}

// ctrlSlot reserves one obligation under the admission bound —
// MaxPendingCtrl caps queued items below the negotiated table size — and
// returns the slot index plus a fresh allocation generation for loss-list
// references, or -1 when the table is full. A dead interior slot is
// reused before the table grows.
func (c *Conn) ctrlSlot() (int, uint32) {
	core := c.core
	limit := wire.MaxControlItems
	if m := core.cfg.MaxPendingCtrl; m > 0 && m < limit {
		limit = m
	}
	used, free := 0, -1
	for i := range core.ctrl {
		if core.ctrl[i].used {
			used++
		} else if free < 0 {
			free = i
		}
	}
	if used >= limit {
		return -1, 0
	}
	core.ctrlAgen++
	if free < 0 {
		core.ctrl = append(core.ctrl, ctrlItem{})
		free = len(core.ctrl) - 1
	}
	return free, core.ctrlAgen
}

// ctrlIDOf locates a pending (unemitted or unacked) item by kind and
// primary argument, for progress-frame re-emission scans.
func (c *Conn) ctrlIDOf(kind uint8, a, b uint64) (uint32, bool) {
	for i := range c.core.ctrl {
		it := &c.core.ctrl[i]
		if it.used && it.kind == kind && it.a == a && it.b == b {
			return uint32(i), true
		}
	}
	return 0, false
}

// reviveCtrlKind marks the first pending item of kind live again for
// retransmission (bounded control resend schedules).
func (c *Conn) reviveCtrlKind(kind uint8) {
	for i := range c.core.ctrl {
		it := &c.core.ctrl[i]
		if it.used && it.kind == kind {
			it.live = false
			return
		}
	}
}

// ctrlForPath reports whether item id may be emitted while building a
// packet for path pi: unpinned items go anywhere; pinned items only on
// their recorded path.
func (c *Conn) ctrlForPath(id uint32, pi int) bool {
	it := &c.core.ctrl[id]
	return it.path < 0 || int(it.path) == pi
}

// ackCtrlItem resolves a control item when its packet is ACKed. The
// lossItem records what that packet actually carried — the emitted
// absolute credit, result status, wake sequence, or requested epoch in
// lo; the fragment index in lo; or the intent flags in flags — plus the
// slot's allocation generation, so a stale reference taken before the
// slot was recycled resolves nothing.
func (c *Conn) ackCtrlItem(li *lossItem) {
	core := c.core
	if int(li.ctrlID) >= len(core.ctrl) {
		return
	}
	it := &core.ctrl[li.ctrlID]
	if !it.used || it.agen != li.ctrlGen {
		return
	}
	switch it.kind {
	case ctrlCryptoFrag:
		// lo carries the fragment index; only that fragment resolves.
		// The item retires once every fragment is acknowledged.
		it.a &^= 1 << uint(li.lo)
		it.a2 &^= 1 << uint(li.lo)
		if it.a != 0 {
			return
		}
	case ctrlMaxData, ctrlMaxStreamData, ctrlMaxSlots:
		// it.b is the pending absolute limit and li.lo the value this
		// packet emitted: an ACK covering an older value cannot retire
		// a refreshed obligation (section 12.4 "highest unsatisfied
		// value", section 14.5 coalescing by semantic identity).
		if it.b > li.lo {
			it.live = false // newer value still owed
			return
		}
	case ctrlPathIntent:
		// An ACK for a probe-intent packet does not resolve a SELECT
		// obligation that superseded it (section 17.4 0→1 upgrade).
		if uint16(it.c) != li.flags {
			it.live = false
			return
		}
	case ctrlPathSelected:
		// li.lo records the emitted status; a result regenerated since
		// remains owed (section 11: ACCEPTED until ACK or superseded).
		if uint64(it.c) != li.lo {
			it.live = false
			return
		}
	case ctrlKeyRequest:
		// li.lo records the emitted requested epoch; a refreshed epoch
		// remains owed.
		if uint64(it.c) != li.lo {
			it.live = false
			return
		}
	case ctrlReset:
		sid := wire.StreamID(it.a)
		s := core.slots.slotOf(sid.Class(), sid.Slot())
		if s != nil && s.send != nil && s.gen == it.gen &&
			s.send.state == sendReset {
			fsmSendSet(&s.send.state, sendDone)
			c.maybeRetire(s, sid)
			notify(c.writeWake)
		}
	case ctrlKeyCommit:
		// KEY_COMMIT is reliable until KEY_CONFIRM, not until packet ACK
		// (section 19.4); an ACK proves delivery, so retransmission is
		// paced rather than immediate. The check is scoped to this
		// item's transaction: once it has closed (confirmed or aborted)
		// the obligation retires with this packet instead of re-arming
		// a slot no emission path can reach again.
		if rt := &core.refresh; rt.active && it.tok == rt.id && !rt.confirmed {
			it.live = false
			rt.commitNextUS = nowUS() + 4*c.ptoUS()
			return
		}
	case ctrlKeyConfirm:
		core.refresh.confirmAcked = true
		c.finishRefresh()
	case ctrlParkWake:
		// li.lo records the emitted wake_seq; a refreshed sequence
		// remains owed (section 35.8).
		if it.b != li.lo {
			it.live = false
			return
		}
	case ctrlParkAwake:
		// As for PARK_WAKE; the server send gate opens only on an ACK
		// of the current result (section 35.9).
		if it.b != li.lo {
			it.live = false
			return
		}
		c.parkAwakeAcked()
	}
	it.used = false
	it.body = nil
	c.compactCtrl()
}

// lossCtrlItem requeues a control reference after packet loss: the
// fragment index clears only its in-flight bit; other kinds return the
// item to pending. A stale reference on a recycled slot resolves nothing.
func (c *Conn) lossCtrlItem(li *lossItem) {
	if int(li.ctrlID) >= len(c.core.ctrl) {
		return
	}
	it := &c.core.ctrl[li.ctrlID]
	if !it.used || it.agen != li.ctrlGen {
		return
	}
	if it.kind == ctrlCryptoFrag {
		it.a2 &^= 1 << uint(li.lo)
		return
	}
	it.live = false
}

// compactCtrl trims dead tail entries; dead interior entries are reused by
// queueCtrl's scan.
func (c *Conn) compactCtrl() {
	core := c.core
	for len(core.ctrl) > 0 && !core.ctrl[len(core.ctrl)-1].used {
		core.ctrl = core.ctrl[:len(core.ctrl)-1]
	}
}
