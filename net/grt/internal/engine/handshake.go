package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"net/netip"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// startClient runs NEW→C_BUILD→C_WAIT_REPLY for an outbound connection.
// Synchronous in the portable profile (the "prekey job" runs inline).
func (c *Conn) startClient(remote netip.AddrPort, id *ClientIdentity) {
	core := c.core
	core.role = roleClient
	core.state = stCBuild
	core.arm(tkHandshake, nowUS()+wire.HandshakeDeadlineUS)
	// Path 0 = active route.
	p := &core.paths[0]
	p.role = pathActive
	p.pstate = psActive
	p.remote = remote
	p.local = core.sock.LocalAddr()
	p.validated = true // client destination is locally authorized
	p.initCC()
	core.activePth = 0
	// Initial destination CID: client-chosen, replaced by Retry or the
	// server's ServerInitialCID.
	pcid, err := randCID()
	if err != nil {
		c.quiesce(err)
		return
	}
	core.peerCID = cidRec{cid: pcid, valid: true}
	// A stored REUSE token rides the first attempt's fragment headers
	// (section 6.3); a Retry token supersedes it on that attempt.
	if core.cfg.HasAddressToken {
		core.hs.token = core.cfg.AddressToken
		core.hs.hasToken = true
	}
	// Fresh identity keys.
	if id != nil && id.Priv != nil {
		core.hs.ikC, err = gc.X25519KeyPairFromPriv(id.Priv)
	} else {
		core.hs.ikC, err = gc.GenerateX25519()
	}
	if err != nil {
		c.quiesce(errHandshakeFailed)
		return
	}
	if err := c.buildClientFlight(); err != nil {
		c.quiesce(err)
		return
	}
	core.state = stCWaitReply
}

// ClientIdentity is an optional stable client identity for PQXDH.
type ClientIdentity struct {
	Priv []byte // 32-byte X25519 private
}

// buildClientFlight constructs the immutable client flight (section 8.2).
func (c *Conn) buildClientFlight() error {
	core := c.core
	// Fresh ephemeral/signed prekeys and the PQ keypair.
	spk, err := gc.GenerateX25519()
	if err != nil {
		return err
	}
	pqpk, err := gc.GenerateMLKEM768()
	if err != nil {
		return err
	}
	core.hs.pqpkC = pqpk
	core.hs.spkC = spk
	var m wire.ClientMessage
	m.Offer = c.cipherOffer()
	core.hs.offer = m.Offer
	if _, err := rand.Read(core.hs.attemptID[:]); err != nil {
		return err
	}
	m.AttemptID = core.hs.attemptID
	if _, err := rand.Read(m.ClientNonce[:]); err != nil {
		return err
	}
	if _, err := rand.Read(core.cliReset[:]); err != nil {
		return err
	}
	if m.ClientInitialCID, err = randCID(); err != nil {
		return err
	}
	m.SPKC = spk.Pub
	core.localCID = cidRec{cid: m.ClientInitialCID, seq: cidSeqHandshake,
		valid: true}
	m.OriginalDCID = core.peerCID.cid
	m.AppID = core.cfg.AppID
	m.AuthMode = core.cfg.AuthMode
	m.Authority = core.cfg.ServerName
	if core.cfg.AuthMode == 0 {
		m.ExpectedServerKey = core.cfg.ExpectedKey
	}
	m.IKC = core.hs.ikC.Pub
	// Sign the tagged key encodings under IK_C via XEdDSA (section 8.2:
	// EncodeEC(SPK_C) and EncodeKEM(PQPK_C), not the raw keys).
	var z [64]byte
	if _, err := rand.Read(z[:]); err != nil {
		return err
	}
	encSPK := gc.EncodeEC(spk.Pub)
	sig, err := gc.XEdDSASign(core.hs.ikC.Priv[:], encSPK[:], z[:])
	if err != nil {
		return err
	}
	m.SPKSignature = sig
	if _, err := rand.Read(z[:]); err != nil {
		return err
	}
	encPQPK := gc.EncodeKEM(pqpk.EK)
	sig, err = gc.XEdDSASign(core.hs.ikC.Priv[:], encPQPK[:], z[:])
	if err != nil {
		return err
	}
	m.PQPKSignature = sig
	copy(m.PQPKC[:], pqpk.EK[:])
	wire.EncodeParams(m.Params[:], &core.local)
	// Resumption/early data (spec section 9): a stored ticket is offered
	// when still valid and bound to this service; early data is opt-in
	// and rides the first client bidi stream's generation-zero batch.
	if t := core.cfg.Ticket; t != nil {
		meta := gc.DecodeTicketMeta(t.Meta[:])
		var nowS uint64
		if core.cfg.NowUnix != nil {
			nowS = uint64(core.cfg.NowUnix())
		}
		if (core.cfg.NowUnix == nil || nowS <= meta.ExpiresUnixS) &&
			meta.ServiceHash == c.serviceHash() {
			m.Ticket = t.Ticket[:]
			m.TicketMetadata = t.Meta
			core.hs.ticketPSK = t.PSK
			core.hs.hasPSK = true
			m.RequestedEarly = len(core.cfg.EarlyData) > 0 &&
				uint64(len(core.cfg.EarlyData)) <= uint64(meta.MaxEarlyPlain)
		}
	}
	msg, err := wire.EncodeClientMessage(&m)
	if err != nil {
		return err
	}
	if core.hs.hasPSK {
		if m.RequestedEarly {
			// CH0 ends before early_ciphertext_length; with an empty
			// ciphertext the tail is elen(2)+binder(32).
			ch0End := len(msg) - 2 - wire.BinderBytes
			ek := gc.DeriveEarly(core.hs.ticketPSK, msg[:ch0End])
			defer ek.Zeroize()
			plain, sid, perr := c.buildEarlyPlain()
			if perr == nil && plain != nil {
				if ct, cerr := gc.EarlySeal(&ek, plain, msg[:ch0End]); cerr == nil {
					m.EarlyCiphertext = ct
					core.hs.earlySID = uint64(sid)
					core.hs.earlyPlain = plain
				}
			}
			if len(m.EarlyCiphertext) == 0 {
				m.RequestedEarly = false
			}
			msg, err = wire.EncodeClientMessage(&m)
			if err != nil {
				return err
			}
		}
		// The early key and binder are computed over the final encoded
		// message: requested_early and the ciphertext are covered fields.
		ch0End := len(msg) - 2 - len(m.EarlyCiphertext) - wire.BinderBytes
		ek := gc.DeriveEarly(core.hs.ticketPSK, msg[:ch0End])
		m.Binder = gc.EarlyBinder(&ek, msg[:len(msg)-wire.BinderBytes])
		ek.Zeroize()
		msg, err = wire.EncodeClientMessage(&m)
		if err != nil {
			return err
		}
	}
	core.hs.clientMsg = msg
	core.hs.flightHash = wire.ClientFlightHash(msg)
	core.hs.clientFrags = wire.BuildFragments(msg, wire.ClassClientFlight,
		core.peerCID.cid, core.hs.attemptID, core.hs.token)
	// Arm the paced emitter: the first round releases at once.
	core.hs.flight = flightTx{frags: core.hs.clientFrags,
		burst: flightBurst, roundUS: nowUS(), rtxUS: bootstrapRtxUS}
	return nil
}

// buildEarlyPlain materializes the early batch (spec section 9.2): the
// first client-initiated bidi slot, generation zero, carrying
// STREAM_OPEN + STREAM(cfg.EarlyData) within the 32-frame/4096-byte
// bound and the ticket's cached server limits. The send half is created
// now so a rejected batch retransmits on the same stream handle and
// offsets through the ordinary path.
func (c *Conn) buildEarlyPlain() ([]byte, wire.StreamID, error) {
	core := c.core
	data := core.cfg.EarlyData
	if len(data) == 0 {
		return nil, 0, nil
	}
	meta := gc.DecodeTicketMeta(core.cfg.Ticket.Meta[:])
	cp, ok := wire.DecodeParams(meta.CachedParams[:])
	if !ok || cp.InitialBidiSlots < 1 ||
		uint64(len(data)) > uint64(cp.InitialStreamWindow) ||
		uint64(len(data)) > uint64(cp.InitialMaxData) {
		return nil, 0, errHandshakeFailed
	}
	sid := wire.MakeStreamID(0, 0, false, c.ourInitiator())
	fb := wire.NewFrameBuilder(make([]byte, 0, wire.MaxEarlyBytes))
	var ob [8]byte
	binary.BigEndian.PutUint64(ob[:], uint64(sid))
	if !fb.Add(wire.FrameStreamOpen, 0, 8, func(d []byte) { copy(d, ob[:]) }) {
		return nil, 0, errHandshakeFailed
	}
	var head [16]byte
	wire.BuildStreamHead(head[:], uint64(sid), 0)
	if !fb.Add(wire.FrameStream, 0, 16+len(data),
		func(d []byte) { copy(d, head[:]); copy(d[16:], data) }) {
		return nil, 0, errHandshakeFailed
	}
	if fb.Len() > wire.MaxEarlyBytes {
		return nil, 0, errHandshakeFailed
	}
	// Install the send half under the cached peer limits so rejection
	// needs no requeue: the bytes are already the stream's unsent range.
	s := core.slots.ensureSlot(classIdx(false, c.ourInitiator()), 0)
	s.state = slotActive
	s.gen = 0
	s.uni = false
	initCap := 256 * 1024
	if cp.InitialStreamWindow > uint32(initCap) {
		initCap = int(cp.InitialStreamWindow)
		if initCap > 1024*1024 {
			initCap = 1024 * 1024
		}
	}
	s.send = &sendHalf{state: sendOpen, buf: make([]byte, 0, initCap), limit: uint64(cp.InitialStreamWindow)}
	s.recv = &recvHalf{state: recvOpen, segs: make([]rxSeg, 0, 16), limit: uint64(core.local.InitialStreamWindow)}
	sh := s.send
	sh.buf = append(sh.buf, data...)
	sh.unsent.add(0, uint64(len(data)))
	sh.unacked.add(0, uint64(len(data)))
	sh.end = uint64(len(data))
	return fb.Bytes(), sid, nil
}

// cipherOffer returns the client's cipher preference list.
func (c *Conn) cipherOffer() [wire.MaxCipherOffers]uint16 {
	var offer [wire.MaxCipherOffers]uint16
	if len(c.core.cfg.CipherPreference) > 0 {
		idx := 0
		for _, id := range c.core.cfg.CipherPreference {
			if idx >= wire.MaxCipherOffers {
				break
			}
			if id == 0 {
				continue
			}
			offer[idx] = id
			idx++
		}
		return offer
	}
	if !c.core.cfg.DisableAES {
		offer[0] = wire.CipherAES256GCM
		offer[1] = wire.CipherXChaCha20Poly1305
	} else {
		offer[0] = wire.CipherXChaCha20Poly1305
	}
	return offer
}

// Bootstrap emission limits (spec section 6.4): bursts are at most two
// datagrams, then pacing at 12000/INITIAL_RTT_US bytes per microsecond.
// Retransmission rounds begin at 999000us and double.
const (
	flightBurst    = 2
	bootstrapRtxUS = 999000
)

// flightPaceUS is a datagram's transmission time at the bootstrap rate:
// bytes / (12000/INITIAL_RTT_US) = bytes * INITIAL_RTT_US / 12000.
func flightPaceUS(n int) uint64 {
	return uint64(n) * wire.InitialRTTUS / 12000
}

// txClientFlight emits the cached client flight: an emission round sends
// at most flightBurst datagrams immediately and paces each further
// datagram at the section-6.4 bootstrap rate; completed rounds
// retransmit on a doubling backoff starting at 999000us. Fragments are
// immutable bytes; the client path is locally authorized so emission
// bypasses congestion accounting but never the bootstrap pace.
func (c *Conn) txClientFlight(now uint64) {
	core := c.core
	ft := &core.hs.flight
	if ft.idx >= len(ft.frags) {
		// Round emitted: hold until the retransmission deadline
		// anchored at this round's start, then double and resend.
		if now < ft.nextUS {
			return
		}
		ft.idx, ft.burst, ft.roundUS = 0, flightBurst, now
		if ft.rtxUS < bootstrapRtxUS {
			ft.rtxUS = bootstrapRtxUS
		} else {
			ft.rtxUS *= 2
		}
	}
	remote := core.paths[core.activePth].remote
	for ft.idx < len(ft.frags) {
		frag := ft.frags[ft.idx]
		if ft.burst == 0 && now < ft.nextUS {
			core.arm(tkFlight, ft.nextUS)
			return
		}
		if core.sock.Send(frag, remote, 0) != nil {
			// Transient backpressure: retry this fragment shortly
			// rather than skipping it until the next RTX round.
			core.arm(tkFlight, now+1000)
			return
		}
		ft.nextUS = now + flightPaceUS(len(frag))
		if ft.burst > 0 {
			ft.burst--
		}
		ft.idx++
	}
	// Round complete: the next round may begin rtxUS after this round's
	// first send (section 6.4 retransmission rounds).
	ft.nextUS = ft.roundUS + ft.rtxUS
	core.arm(tkFlight, ft.nextUS)
}

// txServerFlight emits the cached server flight under the same
// section-6.4 burst/pacing envelope, additionally gated by
// anti-amplification while the handshake path is unvalidated. A new
// emission round is armed per admitted duplicate client flight; when an
// unvalidated budget is exhausted the next client retransmission
// supplies the credit that drives the remaining fragments.
func (c *Conn) txServerFlight(now uint64) {
	core := c.core
	ft := &core.hs.flight
	p := &core.paths[core.activePth]
	for ft.idx < len(ft.frags) {
		frag := ft.frags[ft.idx]
		if ft.burst == 0 && now < ft.nextUS {
			core.arm(tkFlight, ft.nextUS)
			return
		}
		if !p.ampAllow(uint64(len(frag))) {
			return // unvalidated budget spent: client dups drive progress
		}
		if core.sock.Send(frag, p.remote, 0) != nil {
			// Transient backpressure: retry this fragment shortly rather
			// than stalling until the next external trigger.
			core.arm(tkFlight, now+1000)
			return
		}
		p.txBytes += uint64(len(frag))
		p.txPkts++
		ft.nextUS = now + flightPaceUS(len(frag))
		if ft.burst > 0 {
			ft.burst--
		}
		ft.idx++
	}
}

// rxRetry handles a RETRY datagram during C_WAIT_REPLY (section 6.2).
func (c *Conn) rxRetry(e *connEvent, h wire.Header) {
	core := c.core
	if core.state != stCWaitReply || core.hs.retrySeen {

		return
	}
	r, ok := wire.ParseRetry(e.pkt[wire.HeaderBytes:])
	if !ok {
		return
	}
	if r.AttemptID != core.hs.attemptID {
		return
	}
	if r.ClientFlightHash != core.hs.flightHash {
		return
	}
	if r.OriginalDCID != core.peerCID.cid {
		return
	}
	if h.DestCID != core.localCID.cid || r.RetryDCID == 0 {
		return
	}
	// Syntactic token check: kind-0 whose binding equals the original CID.
	tk, ok := wire.DecodeToken(r.Token[:])
	if !ok || tk.Kind != wire.TokenKindRetry || tk.Binding != r.OriginalDCID {
		return
	}
	core.hs.retrySeen = true
	c.retrySeen.Store(true)
	core.hs.token = r.Token
	core.hs.retryDCID = r.RetryDCID
	core.peerCID = cidRec{cid: r.RetryDCID, seq: cidSeqHandshake, valid: true}
	// Rebuild fragments carrying the token; message bytes are identical.
	// The flight re-emits immediately as a fresh paced round (the
	// retransmission backoff keeps its progression).
	core.hs.clientFrags = wire.BuildFragments(core.hs.clientMsg,
		wire.ClassClientFlight, core.peerCID.cid, core.hs.attemptID,
		core.hs.token)
	core.hs.flight = flightTx{frags: core.hs.clientFrags,
		burst: flightBurst, roundUS: nowUS(), rtxUS: core.hs.flight.rtxUS}
}

// rxServerFlight reassembles server flight fragments in C_WAIT_REPLY.
func (c *Conn) rxServerFlight(e *connEvent, h wire.Header) {
	core := c.core
	if core.state != stCWaitReply {
		return
	}
	f, ok := wire.ParseFragment(e.pkt[wire.HeaderBytes:])
	if !ok {
		return
	}
	if f.AttemptID != core.hs.attemptID {
		return
	}
	// message_hash covers the complete server message, not our flight.
	// ParseFragment already enforces count/length consistency and zero
	// padding; here we bind the attempt and pin the hash.
	if core.hs.srvFrags == nil {
		core.hs.srvFrags = make([]rxSeg, f.Count)
		core.hs.srvTotal = f.TotalLength
		core.hs.srvCnt = f.Count
		core.hs.srvHash = f.MessageHash
		core.hs.srvHave = 0
	} else if core.hs.srvTotal != f.TotalLength || int(core.hs.srvCnt) != int(f.Count) ||
		core.hs.srvHash != f.MessageHash {
		return // inconsistent reassembly: drop fragment
	}
	off := uint64(f.Index) * wire.BootstrapChunk
	seg := &core.hs.srvFrags[f.Index]
	if seg.data != nil {
		return // duplicate fragment
	}
	seg.off = off
	seg.data = append([]byte(nil), f.Data...)
	core.hs.srvHave++
	if core.hs.srvHave < int(core.hs.srvCnt) {

		return
	}
	// Assemble.
	msg := make([]byte, 0, core.hs.srvTotal)
	for i := range core.hs.srvFrags {
		msg = append(msg, core.hs.srvFrags[i].data...)
	}
	if uint32(len(msg)) != core.hs.srvTotal ||
		sha256.Sum256(msg) != core.hs.srvHash {
		return
	}
	core.hs.srvFrags = nil
	c.verifyServerFlight(msg)
}

// verifyServerFlight validates the complete server flight (section 8.3/20).
// On a forged/bad reply the attempt is retained until the deadline.
func (c *Conn) verifyServerFlight(msg []byte) {
	core := c.core
	if core.state != stCWaitReply {
		return
	}
	sm, err := wire.ParseServerPrefix(msg)
	if err != nil {
		return
	}
	if sm.AttemptID != core.hs.attemptID ||
		sm.ClientFlightHash != core.hs.flightHash ||
		sm.ClientInitialCID != core.localCID.cid {
		return
	}
	// accepted_routing_dcid must equal the DCID in effect (original or the
	// Retry-issued CID), and accepted_token_hash must equal the hash of
	// the token this attempt presented — including the all-zero token
	// when none was used (section 8.3: the client rejects a mismatch
	// with its actual attempt).
	if sm.AcceptedRoutingCID != core.peerCID.cid {
		return
	}
	if sm.AcceptedTokenHash != sha256.Sum256(core.hs.token[:]) {
		return
	}
	// Deterministic selection check (spec section 4.1): the client checks
	// that selected_cipher equals the first client-offered cipher whose bit is
	// asserted in server_cipher_support_mask.
	offer := core.hs.offer
	if offer[0] == 0 {
		offer = c.cipherOffer()
	}
	expectedCipher := uint16(0)
	for _, o := range offer {
		if o == 0 {
			continue
		}
		if o <= 16 && (sm.ServerCipherMask&(1<<(o-1))) != 0 {
			expectedCipher = o
			break
		}
	}
	if expectedCipher == 0 || sm.SelectedCipher != expectedCipher {
		return
	}
	if int(sm.EncryptedBodyLength) != len(msg)-len(sm.RawSP) ||
		sm.EncryptedBodyLength < 32+16+128 {
		return
	}
	// PQXDH roles on the client: SPK_C.priv·IK_S, IK_C.priv·EK_S,
	// SPK_C.priv·EK_S, then ML-KEM decapsulation.
	dh1, err := gc.X25519(core.hs.spkC.Priv, sm.IKS[:])
	if err != nil {
		return
	}
	dh2, err := gc.X25519(core.hs.ikC.Priv, sm.EKS[:])
	if err != nil {
		return
	}
	dh3, err := gc.X25519(core.hs.spkC.Priv, sm.EKS[:])
	if err != nil {
		return
	}
	ss := gc.MLKEM768Decaps(core.hs.pqpkC.Seed, sm.CTS[:])
	sk := gc.PQXDHKDF(dh1, dh2, dh3, ss)
	for _, d := range []*[32]byte{&dh1, &dh2, &dh3, &ss} {
		for i := range d {
			d[i] = 0
		}
	}
	defer gc.Zeroize(sk[:])
	if sm.PSKAccepted && !core.hs.hasPSK {
		return // offered no ticket but PSK claimed
	}
	var psk [32]byte
	if sm.PSKAccepted {
		psk = core.hs.ticketPSK
	}
	defer gc.Zeroize(psk[:])
	hs := gc.DeriveServerFlight(core.hs.clientMsg, sm.RawSP, sk, psk,
		sm.PSKAccepted, sm.SelectedCipher)
	defer hs.Zeroize() // transient bundle; retained fields are copied out
	// Open the body: ciphertext = msg[len(SP):], AAD = SP.
	ct := msg[len(sm.RawSP):]
	body, err := gc.OpenOnce(sm.SelectedCipher, hs.HsKey[:],
		hs.HsIV[:gc.NonceLen(sm.SelectedCipher)], ct, sm.RawSP, nil)
	if err != nil {
		return
	}
	if uint32(len(body)) != sm.EncryptedBodyLength-16 {
		return
	}
	if len(body) < 32 {
		return
	}
	sb, fin := body[:len(body)-32], body[len(body)-32:]
	wantMAC := gc.ServerFinishedMAC(hs.SfKey, hs.T0, sb)
	if subtle.ConstantTimeCompare(wantMAC[:], fin) != 1 {
		return
	}
	if err := wire.ParseServerBody(sb, &sm); err != nil {
		return
	}
	// Service identity: PINNED exact match, or CHAIN validation.
	if !c.checkServerIdentity(&sm) {
		return
	}
	// Transcript commitment: T1/ROOT0 over the full server message.
	gc.DeriveEstablished(&hs, core.hs.clientMsg, msg, sm.SelectedCipher)
	// --- Commit: install epoch zero and transition ---
	core.cipher = sm.SelectedCipher
	core.cipherSt = cipherLocked
	core.peer = sm.Params
	core.t1 = hs.T1
	core.hs.t0, core.hs.t1 = hs.T0, hs.T1
	core.root0 = hs.Root
	core.finKey = hs.FinishKey
	core.doneKey = hs.DoneKey
	core.resumptionMaster = hs.ResumptionMaster
	core.exporterMaster = hs.ExporterMaster
	// The ticket PSK has served its only use — feeding the handshake KDF;
	// it is dead once the flight verifies (section 9.2/19.5 hygiene).
	core.hs.ticketPSK = [32]byte{}
	core.hs.hasPSK = false
	// Install epoch-0 directional keys.
	var keyB [32]byte
	var ivB [24]byte
	gc.DeriveEpochKeysFrom(hs.Root, hs.T1, core.cipher, 0, gc.DirC2S, keyB[:], ivB[:])
	tx, err := gc.NewEpochAEAD(core.cipher, keyB[:], ivB[:gc.NonceLen(core.cipher)])
	if err != nil {
		c.quiesce(errHandshakeFailed)
		return
	}
	gc.DeriveEpochKeysFrom(hs.Root, hs.T1, core.cipher, 0, gc.DirS2C, keyB[:], ivB[:])
	rxa, err := gc.NewEpochAEAD(core.cipher, keyB[:], ivB[:gc.NonceLen(core.cipher)])
	if err != nil {
		c.quiesce(errHandshakeFailed)
		return
	}
	for i := range keyB {
		keyB[i] = 0
	}
	for i := range ivB {
		ivB[i] = 0
	}
	core.keys.slots[0] = epochSlot{
		state: epochSlotInstalled, epoch: 0, tx: tx, rx: rxa,
		installedUS: nowUS(),
	}
	core.keys.root[0] = hs.Root
	core.keys.base = 0
	// Peer CID: server's chosen CID becomes our destination CID; its
	// stateless-reset token was delivered inside the encrypted flight.
	core.peerCID = cidRec{cid: sm.ServerInitialCID, seq: cidSeqHandshake,
		token: sm.ServerResetToken, valid: true}
	// Our CID stays ClientInitialCID; server knows it via the flight.
	// Erase one-time private keys (section 8.3).
	core.hs.ikC.Zeroize()
	core.hs.spkC.Zeroize()
	core.hs.pqpkC.Zeroize()
	// Queue FINISH (reliable until acked) and transition.
	c.queueCtrl(ctrlFinish, 0, 0, 0, 0, [16]byte{})
	core.state = stCFinishing
	core.disarm(tkFlight) // client flight emission stops at reply
	c.signalSecureReady()
	// Apply flow limits from peer params.
	core.connTxLim = core.peer.InitialMaxData
	core.connRxLim = core.local.InitialMaxData
	core.slots.peerLimit[classIdx(false, c.ourInitiator())] = uint32(core.peer.InitialBidiSlots)
	core.slots.peerLimit[classIdx(true, c.ourInitiator())] = uint32(core.peer.InitialUniSlots)
	core.slots.localLimit[classIdx(false, c.peerInitiator())] = uint32(core.local.InitialBidiSlots)
	core.slots.localLimit[classIdx(true, c.peerInitiator())] = uint32(core.local.InitialUniSlots)
	// Early-data resolution (section 9.3): an accepted batch is
	// acknowledged by its plaintext hash; a rejected or absent batch
	// stays queued as unsent on the same stream handle and offsets.
	core.hs.earlyStatus = sm.EarlyStatus
	if core.hs.earlyPlain != nil {
		sid := wire.StreamID(core.hs.earlySID)
		if s := core.slots.slotOf(sid.Class(), sid.Slot()); s != nil && s.send != nil {
			s.send.limit = uint64(core.peer.InitialStreamWindow)
			switch sm.EarlyStatus {
			case wire.EarlyAccepted:
				if sm.EarlyPlaintextHash != sha256.Sum256(core.hs.earlyPlain) {
					return // response acknowledges a different batch
				}
				s.openAcked = true
				end := s.send.end
				s.send.txHigh = end
				c.ackStreamRange(sid, 0, end, 0)
				core.connTxUsed += end
			default:
				core.hs.earlyReject = true
			}
		}
		core.hs.earlyPlain = nil
	}
}

// checkServerIdentity validates PINNED/CHAIN service binding.
func (c *Conn) checkServerIdentity(sm *wire.ServerMessage) bool {
	core := c.core
	switch core.cfg.AuthMode {
	case 0: // PINNED
		return subtle.ConstantTimeCompare(sm.IKS[:], core.cfg.ExpectedKey[:]) == 1
	case 1: // CHAIN
		var nowS int64 = -1
		if core.cfg.NowUnix != nil {
			nowS = core.cfg.NowUnix()
		}
		return gc.VerifyCredentialChain(sm.Credentials, sm.IKS,
			core.cfg.TrustAnchors, c.serviceHash(), nowS)
	}
	return false
}

// serviceHash computes the service binding hash for credentials/tickets.
func (c *Conn) serviceHash() [32]byte {
	return gc.ServiceHash(c.core.cfg.ServerName, c.core.cfg.AppID)
}

// offerContains checks a selected cipher against our offer.
func (c *Conn) offerContains(cs uint16) bool {
	for _, o := range c.cipherOffer() {
		if o == cs {
			return true
		}
	}
	return false
}

// randCID returns a nonzero random connection ID.
func randCID() (uint64, error) {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		v := binary.BigEndian.Uint64(b[:])
		if v != 0 {
			return v, nil
		}
	}
}

// applyFinish validates the client's FINISH (server side, section 8.4).
// An incorrect authenticated FINISH MAC is PROTOCOL_VIOLATION. Returns true
// on a valid MAC.
func (c *Conn) applyFinish(mac [32]byte, tok [16]byte, pi int) bool {
	core := c.core
	want := gc.ClientFinishMAC(core.finKey, core.t1, tok)
	if subtle.ConstantTimeCompare(mac[:], want[:]) != 1 {
		c.closeWith(codeProtocolViolation, wire.FrameFinish)
		return false
	}
	core.hs.cliFinishMAC = mac
	// The token belongs to the handshake-issued client CID (our peer
	// record for it), so a later stateless reset is recognizable. That
	// record may sit in the table after a CID rotation.
	if core.peerCID.seq == cidSeqHandshake {
		core.peerCID.token = tok
	}
	for i := range core.peerCIDs {
		if r := &core.peerCIDs[i]; r.valid && r.seq == cidSeqHandshake {
			r.token = tok
		}
	}
	// A valid FINISH proves the client received our flight — on the
	// admitted handshake path only (section 6). A FINISH arriving on
	// another tuple does not validate it; the candidate's own
	// PATH_CHALLENGE transaction remains the only way its
	// anti-amplification cap lifts.
	if pi == 0 {
		core.paths[0].validated = true
	}
	c.queueCtrl(ctrlHandshakeDone, 0, 0, 0, 0, [16]byte{})
	return true
}

// applyHandshakeDone validates HANDSHAKE_DONE (client side).
func (c *Conn) applyHandshakeDone(mac [32]byte) bool {
	core := c.core
	if core.state != stCFinishing && core.state != stEstablished {
		return false
	}
	ourMAC := gc.ClientFinishMAC(core.finKey, core.t1, core.cliReset)
	want := gc.ServerDoneMAC(core.doneKey, core.t1, ourMAC)
	if subtle.ConstantTimeCompare(mac[:], want[:]) != 1 {
		c.closeWith(codeProtocolViolation, wire.FrameHandshakeDone)
		return false
	}
	return true
}

// rxClientFlightDup handles a repeated client flight on the server
// (S_FINISHING): it arms a fresh paced emission round of the cached
// server flight. The reemission stays subject to all egress limits
// (section 7.1): the full attempt tuple must match, the datagram credits
// the path like any received traffic, each fragment passes the
// anti-amplification gate while the path is unvalidated, the section-6.4
// burst/pacing envelope applies, and the answer is emitted at most once
// per client retransmit round. After FINISH the flight cache is released
// at its confirmation fence and further duplicates are dropped
// (section 9.4).
func (c *Conn) rxClientFlightDup(e *connEvent, h wire.Header) {
	core := c.core
	if core.role != roleServer || core.state != stSFinishing {
		return
	}
	f, ok := wire.ParseFragment(e.pkt[wire.HeaderBytes:])
	if !ok || f.AttemptID != core.hs.attemptID ||
		f.MessageHash != core.hs.flightHash {
		return
	}
	p := &core.paths[core.activePth]
	if e.src != p.remote || e.dst != p.local ||
		len(core.hs.serverFlight) == 0 {
		return
	}
	p.rxBytes += uint64(len(e.pkt))
	p.rxPkts++
	now := nowUS()
	if core.hs.dupNextUS != 0 && now < core.hs.dupNextUS {
		return
	}
	core.hs.dupNextUS = now + bootstrapRtxUS // one round per client RTX
	// A new paced round re-serves the whole cached flight; txSchedule
	// emits the burst immediately and paces the rest under the
	// anti-amplification gate.
	core.hs.flight = flightTx{frags: core.hs.serverFlight,
		burst: flightBurst, roundUS: now}
}
