package engine

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// CID management (spec section 18): NEW_CID/RETIRE_CID handling, issuance,
// and stateless-reset recognition.

// cidSeqHandshake marks a CID record that carries no sequence: the
// handshake-issued initial CID and a retry DCID live outside the
// NEW_CID sequence space, which begins at zero for issued CIDs.
const cidSeqHandshake = ^uint64(0)

// applyNewCID stores a peer-issued CID. Sequences strictly increase from
// zero; duplicate sequences must be byte-identical; retire_prior_to bounds.
func (c *Conn) applyNewCID(seq, retirePrior, cid uint64, tok [16]byte) {
	core := c.core
	if retirePrior > seq {
		c.closeWith(codeConnectionID, wire.FrameNewCID)
		return
	}
	// A repeat must be byte-identical; a never-stored sequence not above
	// the high-water mark is stale (retired or superseded).
	if core.peerCIDSeqSet && seq <= core.peerCIDSeq {
		for i := range core.peerCIDs {
			r := &core.peerCIDs[i]
			if r.valid && r.seq == seq {
				if r.cid != cid || subtle.ConstantTimeCompare(r.token[:], tok[:]) != 1 {
					c.closeWith(codeConnectionID, wire.FrameNewCID)
				}
				return
			}
		}
		return
	}
	core.peerCIDSeq, core.peerCIDSeqSet = seq, true
	// Admission bound: at most our advertised active_cid_limit live
	// peer-issued CIDs, the handshake-issued initial CID included.
	live := 0
	for i := range core.peerCIDs {
		if core.peerCIDs[i].valid {
			live++
		}
	}
	if core.peerCID.valid {
		live++
	}
	limit := int(core.local.ActiveCIDLimit)
	if limit < 2 {
		limit = 2
	}
	if live >= limit {
		c.closeWith(codeConnectionID, wire.FrameNewCID)
		return
	}
	// Install in a free slot.
	for i := range core.peerCIDs {
		r := &core.peerCIDs[i]
		if !r.valid {
			*r = cidRec{cid: cid, seq: seq, token: tok, valid: true}
			break
		}
	}
	// retire_prior_to: queue RETIRE_CID for covered peer CIDs. The
	// handshake-issued initial CID has no sequence and is exempt.
	for i := range core.peerCIDs {
		r := &core.peerCIDs[i]
		if r.valid && r.seq < retirePrior {
			c.queueCtrl(ctrlRetireCID, 0, r.seq, 0, 0, [16]byte{})
			r.valid = false
		}
	}
	if core.peerCID.valid && core.peerCID.seq < retirePrior {
		core.peerCID.valid = false
		// The just-stored NEW_CID is a replacement: switch the active
		// destination CID to a live record so none remains zero-usable.
		c.rotatePeerCID()
	}
}

// applyRetireCID retires one of our issued local CIDs at the peer's request.
func (c *Conn) applyRetireCID(seq uint64) {
	core := c.core
	found := -1
	live := 0
	if core.localCID.valid {
		live++
	}
	for i := range core.localCIDs {
		r := &core.localCIDs[i]
		if r.valid {
			live++
			if r.seq == seq {
				found = i
			}
		}
	}
	if found < 0 {
		// Retirement of a never-issued sequence is CONNECTION_ID_ERROR.
		if seq >= core.localCIDSeq {
			c.closeWith(codeConnectionID, wire.FrameRetireCID)
		}
		return // already retired: idempotent
	}
	if live <= 1 {
		c.closeWith(codeConnectionID, wire.FrameRetireCID)
		return
	}
	c.deregCID(core.localCIDs[found].cid)
	core.localCIDs[found].valid = false
	// Keep the table replenished for the peer.
	c.issueCID()
}

// deregCID removes a retired local CID from the listener's demux so
// later datagrams addressed to it stop reaching this conn (section 18).
func (c *Conn) deregCID(cid uint64) {
	if l := c.core.listener; l != nil {
		l.mu.Lock()
		delete(l.conns, cid)
		l.mu.Unlock()
	}
}

// issueCID queues a NEW_CID for one of our local CIDs when the peer's
// advertised limit permits another live CID.
func (c *Conn) issueCID() {
	core := c.core
	limit := uint64(core.peer.ActiveCIDLimit)
	if limit < 2 {
		limit = 2
	}
	live := uint64(0)
	for i := range core.localCIDs {
		if core.localCIDs[i].valid {
			live++
		}
	}
	if core.localCID.valid {
		live++
	}
	if live >= limit {
		return
	}
	// Find a free table slot.
	idx := -1
	for i := range core.localCIDs {
		if !core.localCIDs[i].valid {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	cid, err := randCID()
	if err != nil {
		return
	}
	tok := c.resetToken(cid)
	seq := core.localCIDSeq
	core.localCIDSeq++
	core.localCIDs[idx] = cidRec{cid: cid, seq: seq, token: tok, valid: true}
	// Field order: a=cid, b=sequence, tok=reset token; retire_prior_to
	// stays zero (we never ask the peer to retire our CIDs early).
	c.queueCtrl(ctrlNewCID, 0, cid, seq, 0, tok)
	// Register on the listener for demux.
	if core.listener != nil {
		core.listener.mu.Lock()
		core.listener.conns[cid] = c
		core.listener.mu.Unlock()
	}
}

// resetToken derives the stateless-reset token for a local CID:
// HMAC-SHA256(reset_secret, ASCII("GRT11 reset") || cid), first 16 bytes
// (spec section 18).
func (c *Conn) resetToken(cid uint64) [16]byte {
	var in [19]byte // len("GRT11 reset") + 8
	copy(in[:], "GRT11 reset")
	binary.BigEndian.PutUint64(in[11:], cid)
	mac := gc.HMAC256(c.core.resetSecret[:], in[:])
	var out [16]byte
	copy(out[:], mac[:16])
	return out
}

// checkStatelessReset reports whether pkt (an undecryptable datagram) ends
// in a live peer-issued reset token.
func (c *Conn) checkStatelessReset(pkt []byte) bool {
	core := c.core
	if len(pkt) < 16 {
		return false
	}
	var tok [16]byte
	copy(tok[:], pkt[len(pkt)-16:])
	zero := [16]byte{}
	if core.peerCID.valid && core.peerCID.token != zero &&
		subtle.ConstantTimeCompare(tok[:], core.peerCID.token[:]) == 1 {
		return true
	}
	for i := range core.peerCIDs {
		r := &core.peerCIDs[i]
		if r.valid && r.token != zero &&
			subtle.ConstantTimeCompare(tok[:], r.token[:]) == 1 {
			return true
		}
	}
	return false
}

// maybeStatelessReset transitions to DRAINING when an unparsable or
// undecryptable datagram ends in a live peer-issued reset token (spec
// section 18). Unknown tokens and short datagrams are ignored.
func (c *Conn) maybeStatelessReset(e *connEvent) {
	core := c.core
	if !core.state.keyBearing() || core.state.terminating() {
		return
	}
	if !c.checkStatelessReset(e.pkt) {
		return
	}
	core.termErr = &TransportError{Code: codeNoError, Remote: true}
	c.enterDraining()
}

// initResetSecret sets the per-connection reset secret. A server falls
// back to the durable ticket key so tokens survive process restarts of
// ephemeral state; a client uses a fresh random secret.
func (core *connCore) initResetSecret(cfg *Config) error {
	if cfg.ResetSecret != ([32]byte{}) {
		core.resetSecret = cfg.ResetSecret
		return nil
	}
	if cfg.TicketKey != ([32]byte{}) {
		core.resetSecret = gc.Hash256(cfg.TicketKey[:], []byte("reset secret"))
		return nil
	}
	_, err := rand.Read(core.resetSecret[:])
	return err
}
