package engine

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// serverBuild runs S_BUILD: validate the assembled client flight, perform
// the server-side PQXDH, construct and cache the immutable server flight,
// install epoch zero, and enter S_FINISHING (spec section 8.3).
func (c *Conn) serverBuild() {
	core := c.core
	if core.state != stSBuild || core.role != roleServer {
		return
	}
	// Snapshot the fields a refused attempt's bounded stateless RETRY
	// needs: quiesce scrubs handshake residue before we get here.
	msg := core.hs.cliMsg
	c.serverFlight()
	if core.state != stDead || core.hs.hasToken || core.listener == nil {
		return
	}
	// Tokenless admission fallback: a failed bootstrap earns at most one
	// bounded RETRY under the listener's emission policy (section 6.4).
	if len(msg) < 90 || msg[0] != wire.MsgClient || msg[1] != wire.Version ||
		msg[4] < 1 || msg[4] > 2 || msg[5] != 0 ||
		subtle.ConstantTimeCompare(msg[12:28], core.hs.attemptID[:]) != 1 {
		return
	}
	clientCID := binary.BigEndian.Uint64(msg[60:68])
	if clientCID == 0 || core.hs.retryDCID == 0 {
		return
	}
	core.listener.emitRetry(core.hs.attemptID, core.hs.flightHash,
		core.hs.retryDCID, clientCID,
		core.paths[0].remote, core.paths[0].local)
}

func (c *Conn) serverFlight() {
	core := c.core
	if sha256.Sum256(core.hs.cliMsg) != core.hs.flightHash {
		c.quiesce(fmt.Errorf("grt: serverhs L20: %w", errHandshakeFailed))
		return
	}
	cm, ch0End, noBinder, err := wire.ParseClientMessage(core.hs.cliMsg)
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L25: %w", errHandshakeFailed))
		return
	}
	// Service policy: the client's app_id and authority must name us, and
	// a server identity must be configured.
	if cm.AppID != core.cfg.AppID || core.cfg.ServerCert == nil {
		c.quiesce(fmt.Errorf("grt: serverhs L31: %w", errHandshakeFailed))
		return
	}
	if core.cfg.ServerName != "" && cm.Authority != core.cfg.ServerName {
		c.quiesce(fmt.Errorf("grt: serverhs L35: %w", errHandshakeFailed))
		return
	}
	// Kind-0 token binding: the message's original DCID must equal the
	// token binding (spec section 6.2).
	if core.hs.hasToken {
		tk, ok := wire.DecodeToken(core.hs.token[:])
		if !ok || (tk.Kind == wire.TokenKindRetry && tk.Binding != cm.OriginalDCID) {
			c.quiesce(fmt.Errorf("grt: serverhs L43: %w", errHandshakeFailed))
			return
		}
	}
	// Cipher selection: client offer order over local support mask (spec 4.1).
	mask := uint16(1)
	if !core.cfg.DisableAES {
		mask |= 2
	}
	cipher := uint16(0)
	for _, o := range cm.Offer {
		if o == 0 {
			continue
		}
		if o <= 16 && (mask&(1<<(o-1))) != 0 {
			cipher = o
			break
		}
	}
	if cipher == 0 {
		c.quiesce(fmt.Errorf("grt: serverhs L57: %w", errHandshakeFailed))
		return
	}
	// Prekey signatures under IK_C (XEdDSA over the tagged encodings
	// EncodeEC/EncodeKEM, section 8.2).
	if !gc.XEdDSAPublicIsValid(cm.IKC[:]) || !gc.CanonicalX25519Public(cm.SPKC[:]) {
		c.quiesce(fmt.Errorf("grt: serverhs L62: %w", errHandshakeFailed))
		return
	}
	encSPK := gc.EncodeEC(cm.SPKC)
	encPQPK := gc.EncodeKEM(cm.PQPKC)
	if !gc.XEdDSAVerify(cm.IKC[:], encSPK[:], cm.SPKSignature[:]) ||
		!gc.XEdDSAVerify(cm.IKC[:], encPQPK[:], cm.PQPKSignature[:]) {
		c.quiesce(fmt.Errorf("grt: serverhs L67: %w", errHandshakeFailed))
		return
	}
	// Ticket + early data (spec section 9).
	var psk [32]byte
	defer gc.Zeroize(psk[:])
	pskOK := false
	earlyStatus := uint8(0)
	var earlyHash [32]byte
	var earlyFrames []wire.Frame
	if len(cm.Ticket) == wire.TicketBytes {
		pskOK = c.checkTicket(&cm, ch0End, noBinder, &psk)
		if len(cm.EarlyCiphertext) > 0 {
			earlyStatus = 2 // rejected unless fully accepted below
			if pskOK {
				if ef, ph := c.tryEarlyData(&cm, psk, ch0End, noBinder); ef != nil {
					earlyFrames = ef
					earlyHash = ph
					earlyStatus = 1
				}
			}
		}
	}
	// Fresh server ephemeral, nonce, and KEM encapsulation.
	ekS, err := gc.GenerateX25519()
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L92: %w", errHandshakeFailed))
		return
	}
	cts, ss, err := gc.MLKEM768Encaps(cm.PQPKC[:])
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L97: %w", errHandshakeFailed))
		return
	}
	ikS, err := gc.X25519KeyPairFromPriv(core.cfg.ServerCert.IdentityPriv)
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L102: %w", errHandshakeFailed))
		return
	}
	dh1, err := gc.X25519(ikS.Priv, cm.SPKC[:])
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L107: %w", errHandshakeFailed))
		return
	}
	dh2, err := gc.X25519(ekS.Priv, cm.IKC[:])
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L112: %w", errHandshakeFailed))
		return
	}
	dh3, err := gc.X25519(ekS.Priv, cm.SPKC[:])
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L117: %w", errHandshakeFailed))
		return
	}
	sk := gc.PQXDHKDF(dh1, dh2, dh3, ss)
	for _, d := range []*[32]byte{&dh1, &dh2, &dh3, &ss} {
		for i := range d {
			d[i] = 0
		}
	}
	// Server message fields.
	var sm wire.ServerMessage
	sm.SelectedCipher = cipher
	sm.ServerCipherMask = mask
	sm.AttemptID = core.hs.attemptID
	sm.ClientFlightHash = core.hs.flightHash
	if _, err := rand.Read(sm.ServerNonce[:]); err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L162: %w", errHandshakeFailed))
		return
	}
	sm.ClientInitialCID = cm.ClientInitialCID
	if sm.ServerInitialCID, err = randCID(); err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L165: %w", errHandshakeFailed))
		return
	}
	sm.OriginalDCID = cm.OriginalDCID
	sm.AcceptedRoutingCID = cm.OriginalDCID
	if core.hs.retryDCID != 0 {
		sm.AcceptedRoutingCID = core.hs.retryDCID
	}
	sm.AcceptedTokenHash = sha256.Sum256(core.hs.token[:])
	sm.IKS = ikS.Pub
	sm.EKS = ekS.Pub
	sm.CTS = cts
	sm.PSKAccepted = pskOK
	// SB body.
	sm.Params = core.local
	sm.Credentials = core.cfg.ServerCert.Chain
	sm.EarlyStatus = earlyStatus
	sm.ServerResetToken = c.serverResetToken(sm.ServerInitialCID)
	sm.EarlyPlaintextHash = earlyHash
	sb := wire.EncodeServerBody(&sm)
	sm.EncryptedBodyLength = uint32(len(sb) + 32 + wire.TagBytes)
	sp := wire.EncodeServerPrefix(&sm)
	// Handshake secrets and the single immutable body ciphertext.
	hs := gc.DeriveServerFlight(core.hs.cliMsg, sp, sk, psk, pskOK, cipher)
	defer hs.Zeroize() // transient bundle; retained fields are copied out
	fin := gc.ServerFinishedMAC(hs.SfKey, hs.T0, sb)
	plain := append(sb, fin[:]...)
	ct, err := gc.SealOnce(cipher, hs.HsKey[:],
		hs.HsIV[:gc.NonceLen(cipher)], plain, sp, nil)
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L161: %w", errHandshakeFailed))
		return
	}
	serverMsg := append(sp, ct...)
	gc.DeriveEstablished(&hs, core.hs.cliMsg, serverMsg, cipher)
	// Commit state.
	core.cipher = cipher
	core.cipherSt = cipherLocked
	p, ok := wire.DecodeParams(cm.Params[:])
	if !ok {
		c.quiesce(fmt.Errorf("grt: serverhs L171: %w", errHandshakeFailed))
		return
	}
	core.peer = p
	core.t1 = hs.T1
	core.hs.t0, core.hs.t1 = hs.T0, hs.T1
	core.root0 = hs.Root
	core.finKey = hs.FinishKey
	core.doneKey = hs.DoneKey
	core.resumptionMaster = hs.ResumptionMaster
	core.exporterMaster = hs.ExporterMaster
	// Epoch-zero keys: server TX is s2c, RX is c2s.
	var keyB [32]byte
	var ivB [24]byte
	gc.DeriveEpochKeysFrom(hs.Root, hs.T1, cipher, 0, gc.DirS2C, keyB[:], ivB[:])
	tx, err := gc.NewEpochAEAD(cipher, keyB[:], ivB[:gc.NonceLen(cipher)])
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L187: %w", errHandshakeFailed))
		return
	}
	gc.DeriveEpochKeysFrom(hs.Root, hs.T1, cipher, 0, gc.DirC2S, keyB[:], ivB[:])
	rxa, err := gc.NewEpochAEAD(cipher, keyB[:], ivB[:gc.NonceLen(cipher)])
	if err != nil {
		c.quiesce(fmt.Errorf("grt: serverhs L193: %w", errHandshakeFailed))
		return
	}
	for i := range keyB {
		keyB[i] = 0
	}
	for i := range ivB {
		ivB[i] = 0
	}
	core.keys.slots[0] = epochSlot{state: epochSlotInstalled, epoch: 0, tx: tx,
		rx: rxa, installedUS: nowUS()}
	core.keys.root[0] = hs.Root
	core.keys.base = 0
	core.peerCID = cidRec{cid: cm.ClientInitialCID, seq: cidSeqHandshake,
		valid: true}
	core.localCID = cidRec{cid: sm.ServerInitialCID, seq: cidSeqHandshake,
		valid: true}
	// Cache the immutable flight and arm its paced emitter (section
	// 6.4): txServerFlight releases at most a two-datagram burst, then
	// paces each further fragment at 12000/INITIAL_RTT_US bytes/us.
	core.hs.serverFlight = wire.BuildFragments(serverMsg, wire.ClassServerFlight,
		cm.ClientInitialCID, core.hs.attemptID, [wire.TokenBytes]byte{})
	p0 := &core.paths[core.activePth]
	if !core.hs.hasToken {
		// ALLOW_TOKENLESS admission (section 6): the complete reply must
		// fit within three times the bootstrap datagrams actually
		// received on this exact attempt/source, in both bytes and
		// packets. Otherwise decline the attempt with a rate-limited
		// stateless RETRY instead.
		var total uint64
		for _, frag := range core.hs.serverFlight {
			total += uint64(len(frag))
		}
		if total > wire.AmpFactor*p0.rxBytes ||
			uint64(len(core.hs.serverFlight)) > wire.AmpFactor*p0.rxPkts {
			// serverBuild's fallback emits the bounded stateless RETRY.
			c.quiesce(nil)
			return
		}
	}
	core.hs.flight = flightTx{frags: core.hs.serverFlight,
		burst: flightBurst, roundUS: nowUS()}
	// Erase server one-time material (EK_S.private, SK, SS).
	ekS.Zeroize()
	for i := range sk {
		sk[i] = 0
	}
	// Install flow/slot limits from the negotiated parameters.
	core.connTxLim = core.peer.InitialMaxData
	core.connRxLim = core.local.InitialMaxData
	core.slots.peerLimit[classIdx(false, c.ourInitiator())] = uint32(core.peer.InitialBidiSlots)
	core.slots.peerLimit[classIdx(true, c.ourInitiator())] = uint32(core.peer.InitialUniSlots)
	core.slots.localLimit[classIdx(false, c.peerInitiator())] = uint32(core.local.InitialBidiSlots)
	core.slots.localLimit[classIdx(true, c.peerInitiator())] = uint32(core.local.InitialUniSlots)
	if earlyStatus == 1 {
		c.commitEarly(earlyFrames)
	}
	if core.listener != nil {
		core.listener.registerConn(c)
		select {
		case core.listener.accept <- c:
		default:
		}
	}
	core.state = stSFinishing
}

// checkTicket validates a presented resumption ticket: envelope, service,
// metadata consistency, validity interval, and the mandatory binder.
// On success it writes the ticket PSK into pskOut.
func (c *Conn) checkTicket(cm *wire.ClientMessage, ch0End, noBinder int, pskOut *[32]byte) bool {
	core := c.core
	tp, err := gc.OpenTicket(core.cfg.TicketKey, cm.Ticket)
	if err != nil {
		return false
	}
	defer func() { tp.PSK = [32]byte{} }()
	meta := gc.DecodeTicketMeta(cm.TicketMetadata[:])
	svc := c.serviceHash()
	mh := sha256.Sum256(cm.TicketMetadata[:])
	var nowS uint64
	if core.cfg.NowUnix != nil {
		nowS = uint64(core.cfg.NowUnix())
	}
	serviceOK := tp.ServiceHash == svc && meta.ServiceHash == svc
	metaOK := tp.TicketID == meta.TicketID && tp.MetadataHash == mh &&
		tp.ExpiresUnixS == meta.ExpiresUnixS
	validityOK := tp.IssuedUnixS <= nowS && nowS <= tp.ExpiresUnixS &&
		nowS <= meta.ExpiresUnixS
	// The ticket must have been minted under the server's current
	// authentication policy and locally authorized replay domain
	// (section 9.1); a policy rotation or domain change invalidates
	// outstanding tickets.
	policyOK := tp.PolicyHash == core.cfg.PolicyHash &&
		tp.ReplayDomain == core.cfg.ReplayDomain &&
		meta.ReplayDomain == core.cfg.ReplayDomain
	if !serviceOK || !metaOK || !validityOK || !policyOK {
		return false
	}
	// The binder is required whenever a ticket is present (section 8.2).
	ek := gc.DeriveEarly(tp.PSK, core.hs.cliMsg[:ch0End])
	defer ek.Zeroize()
	if !gc.EarlyBinderOK(&ek, core.hs.cliMsg[:noBinder], cm.Binder) {
		return false
	}
	*pskOut = tp.PSK
	return true
}

// serverResetToken derives the reset token for a local CID.
func (c *Conn) serverResetToken(cid uint64) [16]byte {
	return c.resetToken(cid)
}

// tryEarlyData authenticates and parses the early batch. Returns the frame
// slice and plaintext hash only on full acceptance (spec section 9.2/9.3).
func (c *Conn) tryEarlyData(cm *wire.ClientMessage, psk [32]byte, ch0End, noBinder int) ([]wire.Frame, [32]byte) {
	var zero [32]byte
	core := c.core
	if core.cfg.ReplayLedger == nil || len(cm.EarlyCiphertext) > wire.MaxEarlyBytes+wire.TagBytes {
		return nil, zero
	}
	ch0 := core.hs.cliMsg[:ch0End]
	ek := gc.DeriveEarly(psk, ch0)
	defer ek.Zeroize()
	if !gc.EarlyBinderOK(&ek, core.hs.cliMsg[:noBinder], cm.Binder) {
		return nil, zero
	}
	pt, err := gc.EarlyOpen(&ek, cm.EarlyCiphertext, ch0)
	if err != nil || len(pt) > wire.MaxEarlyBytes {
		return nil, zero
	}
	var frbuf [wire.MaxFrames]wire.Frame
	frames, _, ferr := wire.ParseFrames(pt, frbuf[:0])
	if ferr != nil {
		return nil, zero
	}
	// Grammar: only OPEN/STREAM/PADDING; generation-zero peer-initiated
	// streams; bounded by our current receive limits. Commit is
	// all-or-nothing (section 9.2): every STREAM must reference a stream
	// opened earlier in this batch, and overlapping ranges must carry
	// identical bytes — insert can then never fail mid-commit.
	var total uint64
	var opened [wire.MaxFrames]uint64
	var nOpened int
	type sseg struct {
		sid, off, end uint64
		data          []byte
	}
	var segs [wire.MaxFrames]sseg
	var nSeg int
	isOpen := func(sid uint64) bool {
		for i := 0; i < nOpened; i++ {
			if opened[i] == sid {
				return true
			}
		}
		return false
	}
	for i := range frames {
		f := &frames[i]
		switch f.Type {
		case wire.FramePadding:
		case wire.FrameStreamOpen:
			sid64, ok := wire.ParseU64(f.Body)
			if !ok {
				return nil, zero
			}
			sid := wire.StreamID(sid64)
			if sid.Generation() != 0 || sid.Initiator() != c.peerInitiator() {
				return nil, zero
			}
			limit := core.local.InitialBidiSlots
			if sid.Uni() {
				limit = core.local.InitialUniSlots
			}
			if sid.Slot() >= limit {
				return nil, zero
			}
			if !isOpen(uint64(sid)) && nOpened < wire.MaxFrames {
				opened[nOpened] = uint64(sid)
				nOpened++
			}
		case wire.FrameStream:
			sid64, off, data, ok := wire.ParseStreamBody(f.Body)
			if !ok {
				return nil, zero
			}
			sid := wire.StreamID(sid64)
			if sid.Generation() != 0 || sid.Initiator() != c.peerInitiator() ||
				!isOpen(uint64(sid)) {
				return nil, zero
			}
			end := off + uint64(len(data))
			if end > uint64(core.local.InitialStreamWindow) {
				return nil, zero
			}
			for j := 0; j < nSeg; j++ {
				s := &segs[j]
				if s.sid != uint64(sid) || s.end <= off || s.off >= end {
					continue
				}
				lo, hi := maxU64(off, s.off), minU64(end, s.end)
				if !bytes.Equal(data[lo-off:hi-off], s.data[lo-s.off:hi-s.off]) {
					return nil, zero // conflicting overlap
				}
			}
			// Unique coverage charges connection flow control once:
			// identical retransmissions don't double-count.
			var hi uint64
			for j := 0; j < nSeg; j++ {
				if segs[j].sid == uint64(sid) && segs[j].end > hi {
					hi = segs[j].end
				}
			}
			segs[nSeg] = sseg{uint64(sid), off, end, data}
			nSeg++
			if end > hi {
				total += end - hi
			}
			if total > core.local.InitialMaxData {
				return nil, zero
			}
		default:
			return nil, zero
		}
	}
	// Replay ledger: durable consume before any exposure.
	tp, terr := gc.OpenTicket(core.cfg.TicketKey, cm.Ticket)
	if terr != nil {
		return nil, zero
	}
	defer func() { tp.PSK = [32]byte{} }()
	if !core.cfg.ReplayLedger.ConsumeEarly(tp.ReplayDomain, tp.TicketID,
		core.hs.attemptID, core.hs.flightHash) {
		return nil, zero
	}
	return frames, sha256.Sum256(pt)
}

// commitEarly applies the validated early batch as one transaction.
func (c *Conn) commitEarly(frames []wire.Frame) {
	for i := range frames {
		f := &frames[i]
		switch f.Type {
		case wire.FrameStreamOpen:
			sid64, ok := wire.ParseU64(f.Body)
			if ok {
				c.applyOpen(wire.StreamID(sid64))
			}
		case wire.FrameStream:
			sid64, off, data, ok := wire.ParseStreamBody(f.Body)
			if ok {
				c.applyStreamData(wire.StreamID(sid64), off, data,
					f.Flags&wire.FrameFlagStreamFin != 0, nil)
			}
		}
	}
}
