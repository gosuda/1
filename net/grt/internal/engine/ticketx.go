package engine

import (
	"crypto/sha256"
	"encoding/binary"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// applyNewToken caches the peer's REUSE token for a future handshake
// (spec section 6.3). At most one current token per context is kept.
func (c *Conn) applyNewToken(tok [wire.TokenBytes]byte) {
	c.core.newToken = tok
	c.core.hasNewToken = true
}

// applyNewTicket stores a server-issued resumption ticket (client side).
// The client derives the ticket PSK from the retained resumption master.
func (c *Conn) applyNewTicket(meta [wire.TicketMetaBytes]byte, ticket [wire.TicketBytes]byte) {
	core := c.core
	m := gc.DecodeTicketMeta(meta[:])
	core.resume = &ClientTicket{
		Ticket: ticket,
		Meta:   meta,
		PSK:    gc.TicketPSK(core.resumptionMaster, m.TicketID),
		ID:     m.TicketID,
	}
}

// issueTicket builds and queues a NEW_TICKET for the peer (server side,
// spec section 9.1). Requires a configured ticket key and issuer.
func (c *Conn) issueTicket() {
	core := c.core
	if core.cfg.TicketIssuer == nil || core.cfg.TicketKey == ([32]byte{}) ||
		core.resumptionMaster == ([32]byte{}) {
		return
	}
	prefix, counter := core.cfg.TicketIssuer()
	nonce := gc.TicketNonceIssue(prefix, counter)
	// Ticket ID binds the unique issuer nonce material so distinct
	// issuances never share a replay-ledger identity.
	var ib [24]byte
	copy(ib[:16], prefix[:])
	binary.BigEndian.PutUint64(ib[16:], counter)
	idFull := sha256.Sum256(ib[:])
	var id [16]byte
	copy(id[:], idFull[:16])
	var tp gc.TicketPlain
	tp.PSK = gc.TicketPSK(core.resumptionMaster, id)
	tp.TicketID = id
	var nowS uint64
	if core.cfg.NowUnix != nil {
		nowS = uint64(core.cfg.NowUnix())
	}
	tp.IssuedUnixS = nowS
	tp.ExpiresUnixS = nowS + gc.TicketMaxLifetimeS
	tp.ServiceHash = c.serviceHash()
	tp.MaxEarlyPlain = wire.MaxEarlyBytes
	tp.ReplayDomain = core.cfg.ReplayDomain
	tp.PolicyHash = core.cfg.PolicyHash
	var meta gc.TicketMetadata
	meta.TicketID = id
	meta.ExpiresUnixS = tp.ExpiresUnixS
	meta.ServiceHash = tp.ServiceHash
	meta.MaxEarlyPlain = tp.MaxEarlyPlain
	meta.ReplayDomain = tp.ReplayDomain
	wire.EncodeParams(meta.CachedParams[:], &core.local)
	mb := gc.EncodeTicketMeta(&meta)
	tp.MetadataHash = sha256.Sum256(mb[:])
	plain := gc.EncodeTicketPlain(&tp)
	ticket := gc.SealTicket(core.cfg.TicketKey, core.cfg.TicketKeyID, nonce, plain)
	tp.PSK = [32]byte{}
	gc.Zeroize(plain[:])
	body := make([]byte, wire.TicketMetaBytes+wire.TicketBytes)
	copy(body[:], mb[:])
	copy(body[wire.TicketMetaBytes:], ticket[:])
	c.queueCtrlBody(ctrlNewTicket, body)
}

// queueCtrlBody queues a control item whose full body travels in it.body.
// path must be the unpinned sentinel -1: a zero value would pin emission
// to path slot 0 (see ctrlForPath), so tickets, tokens, and crypto
// fragments could never leave on the post-migration active path. Like
// every control obligation it respects the MaxPendingCtrl admission bound
// and allocates a fresh generation; false means the table is full.
func (c *Conn) queueCtrlBody(kind uint8, body []byte) bool {
	core := c.core
	i, agen := c.ctrlSlot()
	if i < 0 {
		return false
	}
	core.ctrl[i] = ctrlItem{kind: kind, used: true, path: -1, agen: agen,
		body: append([]byte(nil), body...)}
	return true
}

// issueReuseToken queues a NEW_TOKEN carrying a REUSE token bound to the
// active path's remote address (server side, spec section 6.3).
func (c *Conn) issueReuseToken() {
	core := c.core
	if core.listener == nil || core.listener.tokens == nil {
		return
	}
	p := &core.paths[core.activePth]
	tok, ok := core.listener.tokens.IssueReuse(p.remote, p.local)
	if !ok {
		return
	}
	c.queueCtrlBody(ctrlNewToken, append([]byte(nil), tok[:]...))
}

// postEstablish runs once on the transition to ESTABLISHED: the server
// issues its resumption ticket and address-validation reuse token; both
// sides top up the peer's CID table to its advertised limit.
func (c *Conn) postEstablish() {
	core := c.core
	if core.role == roleServer {
		c.issueTicket()
		c.issueReuseToken()
	}
	c.issueCID()
}
