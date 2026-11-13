package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// refreshTx is the section-19 bounded key-refresh transaction state.
type refreshTx struct {
	active   bool
	id       [16]byte
	epoch    uint32 // new epoch being staged
	deadline uint64
	// client offer state
	offer     []byte // OFFER message (client retains for RT)
	offerMask uint8  // unacked fragment bitmap
	// server answer state
	answer     []byte
	answerMask uint8
	// answeredHash binds the cached ANSWER to the exact OFFER message it
	// answered: a retransmitted OFFER reassembling to the same message is
	// re-served the identical logical response (section 19.3).
	answeredHash [32]byte
	// inbound CRYPTO_FRAGMENT reassembly
	rxFrags [][]byte
	rxTotal uint32
	rxCnt   uint16
	rxHave  int
	rxHash  [32]byte
	rxID    [16]byte
	rxKind  uint8
	// derived proof values
	rt         [32]byte
	commitMAC  [32]byte
	confirmMAC [32]byte
	answerMAC  [32]byte
	// staged-epoch working state
	xC gc.X25519KeyPair   // client fresh DH
	qC gc.MLKEM768KeyPair // client fresh KEM
	// commit protocol flags
	commitSent  bool // client: KEY_COMMIT issued under STAGED
	committed   bool // server: commit accepted; awaiting confirm ACK
	confirmed   bool // client: KEY_CONFIRM accepted
	needRequest bool // server: owe a KEY_REQUEST for a later threshold
	// client: next scheduled KEY_COMMIT retransmission under STAGED
	// (paced, fresh PNs, until a KEY_CONFIRM-carrying packet arrives)
	commitNextUS uint64
	// server: a KEY_CONFIRM-carrying packet was ACKed (section 19.4)
	confirmAcked bool
	// The most recently completed OFFER (server side): straggler
	// retransmissions of it must not open a bogus new transaction once
	// the live one has finished and its epoch has promoted.
	lastDoneID   [16]byte
	lastDoneHash [32]byte
	hasLastDone  bool
}

// refreshInFlight reports whether a refresh transaction is open.
func (c *Conn) refreshInFlight() bool { return c.core.refresh.active }

// softLimitHit reports whether the current epoch crossed a soft threshold.
func (c *Conn) softLimitHit() bool {
	s := c.core.keys.current()
	if s == nil {
		return false
	}
	softC := wire.RekeySoftPacketsXChaCha
	if s.tx.CipherID() == wire.CipherAES256GCM {
		softC = wire.RekeySoftPacketsAESGCM
	}
	return s.sealCount >= softC || s.sealBytes >= wire.RekeySoftBytes
}

// initiateKeyRefresh starts the client-side OFFER (spec section 19.3).
func (c *Conn) initiateKeyRefresh() {
	core := c.core
	eligible := core.state == stEstablished || core.state == stCFinishing
	if core.role != roleClient || core.refresh.active || !eligible {
		return
	}
	cur := core.keys.current()
	if cur == nil || cur.epoch == ^uint32(0) {
		return
	}
	// A new transaction requires the PREVIOUS slot erased: three slots
	// hold PREVIOUS, CURRENT, STAGED and no fourth exists (section 19.2).
	for i := range core.keys.slots {
		if core.keys.slots[i].state == epochSlotGrace {
			return
		}
	}
	if core.keys.staged() != nil {
		return // a staged slot lingers: previous transaction unfinished
	}
	rt := &core.refresh
	*rt = refreshTx{active: true, epoch: cur.epoch + 1}
	if _, err := rand.Read(rt.id[:]); err != nil {
		rt.active = false
		return
	}
	var err error
	rt.xC, err = gc.GenerateX25519()
	if err != nil {
		rt.active = false
		return
	}
	rt.qC, err = gc.GenerateMLKEM768()
	if err != nil {
		rt.active = false
		return
	}
	var nc [32]byte
	if _, err := rand.Read(nc[:]); err != nil {
		rt.active = false
		return
	}
	// OFFER: new_epoch:4 || cipher:2 || zero:2 || id:16 || N_C:32 || X_C:32 || Q_C:1184
	offer := make([]byte, 1272)
	binary.BigEndian.PutUint32(offer[0:], rt.epoch)
	binary.BigEndian.PutUint16(offer[4:], core.cipher)
	copy(offer[8:], rt.id[:])
	copy(offer[24:], nc[:])
	copy(offer[56:], rt.xC.Pub[:])
	copy(offer[88:], rt.qC.EK[:])
	rt.offer = offer
	if !c.queueFrags(rt.id, 1, offer) {
		// Control table full: abort the transaction before it starts
		// rather than strand an OFFER that can never be emitted.
		rt.active = false
		rt.offer = nil
		return
	}
	rt.deadline = nowUS() + c.refreshDeadlineUS()
	core.arm(tkRefresh, rt.deadline)
}

// refreshDeadlineUS is max(10s, 6*basePTO).
func (c *Conn) refreshDeadlineUS() uint64 {
	d := 6 * c.ptoUS()
	if d < wire.HandshakeDeadlineUS {
		d = wire.HandshakeDeadlineUS
	}
	return d
}

// queueFrags queues the CRYPTO_FRAGMENT logical item for a whole message.
// The item's a field carries the unemitted/unacked fragment bitmap. Like
// every control obligation it respects the MaxPendingCtrl admission bound;
// false means the table is full.
func (c *Conn) queueFrags(id [16]byte, kind uint8, msg []byte) bool {
	core := c.core
	n := (len(msg) + wire.CryptoFragChunk - 1) / wire.CryptoFragChunk
	mask := uint64(0)
	for i := 0; i < n; i++ {
		mask |= 1 << uint(i)
	}
	i, agen := c.ctrlSlot()
	if i < 0 {
		return false
	}
	core.ctrl[i] = ctrlItem{kind: ctrlCryptoFrag, used: true, path: -1,
		a: mask, b: uint64(kind), tok: id, agen: agen,
		body: append([]byte(nil), msg...)}
	return true
}

// preflightCryptoFrag semantically gates a CRYPTO_FRAGMENT. Structural
// bounds were already enforced by ParseCryptoFrag; inconsistent fragments
// are ignored at commit (a hostile sender must not be able to abort a
// connection by desyncing reassembly), so this admits everything the
// parser accepted.
func (c *Conn) preflightCryptoFrag(cf *wire.CryptoFrag) closeCode {
	return codeNoError
}

// applyCryptoFrag feeds one refresh fragment into bounded reassembly.
func (c *Conn) applyCryptoFrag(cf *wire.CryptoFrag) {
	core := c.core
	rt := &core.refresh
	want := uint8(1)
	if core.role == roleClient {
		want = 2 // client reassembles ANSWER
	}
	if cf.Kind != want {
		return // wrong direction for our role
	}
	if !rt.active || rt.id != cf.RefreshID {
		// Server: a fresh OFFER starts a transaction when none is active.
		if core.role == roleServer && cf.Kind == 1 && !rt.active {
			if rt.hasLastDone && cf.RefreshID == rt.lastDoneID &&
				cf.MessageHash == rt.lastDoneHash {
				return // straggler retransmit of the completed OFFER
			}
			c.beginOfferReassembly(cf)
		}
		return
	}
	c.absorbFrag(cf)
}

// beginOfferReassembly starts a server-side OFFER reassembly transaction.
func (c *Conn) beginOfferReassembly(cf *wire.CryptoFrag) {
	rt := &c.core.refresh
	*rt = refreshTx{active: true, id: cf.RefreshID, rxKind: 1}
	rt.rxFrags = make([][]byte, cf.Count)
	rt.rxTotal = cf.TotalLength
	rt.rxCnt = cf.Count
	rt.rxHash = cf.MessageHash
	rt.rxID = cf.RefreshID
	rt.deadline = nowUS() + c.refreshDeadlineUS()
	c.core.arm(tkRefresh, rt.deadline)
	c.absorbFrag(cf)
}

// absorbFrag stores one fragment; on completion runs the semantic handler.
func (c *Conn) absorbFrag(cf *wire.CryptoFrag) {
	rt := &c.core.refresh
	if rt.rxFrags == nil {
		rt.rxFrags = make([][]byte, cf.Count)
		rt.rxTotal = cf.TotalLength
		rt.rxCnt = cf.Count
		rt.rxHash = cf.MessageHash
		rt.rxID = cf.RefreshID
		rt.rxKind = cf.Kind
	}
	if rt.rxTotal != cf.TotalLength || rt.rxCnt != cf.Count ||
		rt.rxHash != cf.MessageHash || rt.rxID != cf.RefreshID ||
		rt.rxKind != cf.Kind {
		return // inconsistent: ignore fragment
	}
	if rt.rxFrags[cf.Index] != nil {
		return // identical duplicate retained
	}
	rt.rxFrags[cf.Index] = append([]byte(nil), cf.Data...)
	rt.rxHave++
	if rt.rxHave < int(rt.rxCnt) {
		return
	}
	msg := make([]byte, 0, rt.rxTotal)
	for _, fr := range rt.rxFrags {
		msg = append(msg, fr...)
	}
	if sha256.Sum256(msg) != rt.rxHash || uint32(len(msg)) != rt.rxTotal {
		rt.rxFrags = nil
		rt.rxHave = 0
		return
	}
	rt.rxFrags = nil
	rt.rxHave = 0
	if c.core.role == roleServer && rt.rxKind == 1 {
		if rt.answer != nil {
			// A retransmitted OFFER reassembling to the answered message
			// receives the cached logical ANSWER again; a different
			// message under the same RefreshID cannot be answered and
			// is dropped without disturbing the transaction.
			if rt.answeredHash == sha256.Sum256(msg) {
				c.requeueAnswer()
			}
			return
		}
		c.serverAnswer(msg)
	} else if c.core.role == roleClient && rt.rxKind == 2 {
		if rt.commitSent {
			// A retransmitted ANSWER reassembles after the staged keys
			// and commit obligation were already derived; the duplicate
			// is ignored (section 19.3 duplicate semantics).
			return
		}
		c.clientAcceptAnswer(msg)
	}
}

// requeueAnswer re-owes every ANSWER fragment on the live control item,
// or queues a fresh item when the original retired after full
// acknowledgment (spec section 19.3: the cached logical response).
func (c *Conn) requeueAnswer() {
	core := c.core
	rt := &core.refresh
	n := (len(rt.answer) + wire.CryptoFragChunk - 1) / wire.CryptoFragChunk
	var mask uint64
	for i := 0; i < n; i++ {
		mask |= 1 << uint(i)
	}
	for i := range core.ctrl {
		it := &core.ctrl[i]
		if it.used && it.kind == ctrlCryptoFrag && it.tok == rt.id &&
			uint8(it.b) == 2 {
			// Fragments already acknowledged need no re-emission;
			// in-flight bits (a2) resolve normally.
			it.a |= mask
			it.live = false
			return
		}
	}
	c.queueFrags(rt.id, 2, rt.answer)
}

// serverAnswer processes a complete OFFER and emits the ANSWER
// (spec section 19.3 server side).
func (c *Conn) serverAnswer(offer []byte) {
	core := c.core
	rt := &core.refresh
	if len(offer) != 1272 {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	epoch := binary.BigEndian.Uint32(offer[0:])
	cs := binary.BigEndian.Uint16(offer[4:])
	if binary.BigEndian.Uint16(offer[6:]) != 0 {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	cur := core.keys.current()
	if cur == nil || cs != core.cipher || epoch != cur.epoch+1 {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	rt.epoch = epoch
	xS, err := gc.GenerateX25519()
	if err != nil {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	ctr, ssR, err := gc.MLKEM768Encaps(offer[88 : 88+1184])
	if err != nil {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	dR, err := gc.X25519(xS.Priv, offer[56:88])
	if err != nil {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	// AP prefix.
	ap := make([]byte, 1176)
	binary.BigEndian.PutUint32(ap[0:], epoch)
	binary.BigEndian.PutUint16(ap[4:], core.cipher)
	copy(ap[8:], rt.id[:])
	var ns [32]byte
	if _, err := rand.Read(ns[:]); err != nil {
		c.closeWith(codeInternal, wire.FrameCryptoFragment)
		return
	}
	copy(ap[24:], ns[:])
	copy(ap[56:], xS.Pub[:])
	copy(ap[88:], ctr[:])
	rs := gc.DeriveRefresh(core.t1, offer, ap,
		gc.DeriveRefreshSecret(core.keys.rootFor(cur.epoch), core.t1, core.cipher, cur.epoch),
		dR, ssR)
	for i := range dR {
		dR[i] = 0
	}
	for i := range ssR {
		ssR[i] = 0
	}
	rt.rt, rt.commitMAC, rt.confirmMAC, rt.answerMAC = rs.RT, rs.CommitMAC, rs.ConfirmMAC, rs.AnswerMAC
	// ANSWER = AP || answer_mac.
	ans := append(ap, rs.AnswerMAC[:]...)
	rt.answer = ans
	rt.answeredHash = sha256.Sum256(offer)
	// Install STAGED keys under the new epoch root.
	if !c.installStaged(epoch, rs.RootNew) {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	c.queueFrags(rt.id, 2, ans)
}

// installStaged derives and installs the new epoch's directional keys in
// a free slot and records its root at the same index. Returns false when
// no slot is free.
func (c *Conn) installStaged(epoch uint32, root [32]byte) bool {
	core := c.core
	if core.keys.find(epoch) != nil {
		return false
	}
	idx := -1
	for i := range core.keys.slots {
		if core.keys.slots[i].state == epochSlotEmpty {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	var kTx, kRx [32]byte
	var ivTx, ivRx [24]byte
	txDir, rxDir := gc.DirS2C, gc.DirC2S
	if core.role == roleClient {
		txDir, rxDir = gc.DirC2S, gc.DirS2C
	}
	gc.DeriveEpochKeysFrom(root, core.t1, core.cipher, epoch, txDir, kTx[:], ivTx[:])
	tx, err := gc.NewEpochAEAD(core.cipher, kTx[:], ivTx[:gc.NonceLen(core.cipher)])
	if err != nil {
		return false
	}
	gc.DeriveEpochKeysFrom(root, core.t1, core.cipher, epoch, rxDir, kRx[:], ivRx[:])
	rxa, err := gc.NewEpochAEAD(core.cipher, kRx[:], ivRx[:gc.NonceLen(core.cipher)])
	if err != nil {
		tx.Zeroize()
		return false
	}
	for i := range kTx {
		kTx[i], kRx[i] = 0, 0
	}
	for i := range ivTx {
		ivTx[i], ivRx[i] = 0, 0
	}
	core.keys.slots[idx] = epochSlot{state: epochSlotInstalled, epoch: epoch,
		tx: tx, rx: rxa, installedUS: nowUS()}
	core.keys.root[idx] = root
	// Datagrams that arrived ahead of this key install become
	// processable now (section 4.5 hold queue).
	c.flushDeferred()
	return true
}

// rootFor returns the stored root for epoch e.
func (k *keyState) rootFor(e uint32) [32]byte {
	for i := range k.slots {
		if k.slots[i].epoch == e {
			return k.root[i]
		}
	}
	return [32]byte{}
}

// clientAcceptAnswer validates a complete ANSWER and stages keys
// (spec section 19.3 client side).
func (c *Conn) clientAcceptAnswer(ans []byte) {
	core := c.core
	rt := &core.refresh
	if len(ans) != 1176+32 || rt.offer == nil {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	ap := ans[:1176]
	epoch := binary.BigEndian.Uint32(ap[0:])
	cs := binary.BigEndian.Uint16(ap[4:])
	if binary.BigEndian.Uint16(ap[6:]) != 0 ||
		cs != core.cipher || epoch != rt.epoch ||
		subtle.ConstantTimeCompare(ap[8:24], rt.id[:]) != 1 {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	dR, err := gc.X25519(rt.xC.Priv, ap[56:88])
	if err != nil {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	ssR, err := gc.MLKEM768Decaps(rt.qC.Seed, ap[88:1176])
	if err != nil {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	rs := gc.DeriveRefresh(core.t1, rt.offer, ap,
		gc.DeriveRefreshSecret(core.keys.rootFor(rt.epoch-1), core.t1, core.cipher, rt.epoch-1),
		dR, ssR)
	if subtle.ConstantTimeCompare(rs.AnswerMAC[:], ans[1176:]) != 1 {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	rt.rt, rt.commitMAC, rt.confirmMAC = rs.RT, rs.CommitMAC, rs.ConfirmMAC
	if !c.installStaged(epoch, rs.RootNew) {
		c.closeWith(codeKeyUpdate, wire.FrameCryptoFragment)
		return
	}
	// Erase fresh private inputs now that derivation is complete.
	rt.xC.Zeroize()
	rt.qC.Zeroize()
	for i := range dR {
		dR[i] = 0
	}
	for i := range ssR {
		ssR[i] = 0
	}
	// Queue KEY_COMMIT; emitted under STAGED by the TX path. A dropped
	// obligation would stall the refresh until its deadline — a local
	// resource failure is reported instead.
	rt.commitSent = true
	if !c.queueCtrl(ctrlKeyCommit, 0, uint64(epoch), 0, 0, rt.id) {
		c.closeWith(codeLocalResource, wire.FrameKeyCommit)
	}
}

// applyKeyCommit handles a KEY_COMMIT inside a STAGED-epoch packet
// (server side, spec section 19.4).
func (c *Conn) applyKeyCommit(epoch uint32, id [16]byte, mac [32]byte) {
	core := c.core
	rt := &core.refresh
	if core.role != roleServer || !rt.active || rt.answer == nil {
		return
	}
	if epoch != rt.epoch || id != rt.id {
		c.closeWith(codeKeyUpdate, wire.FrameKeyCommit)
		return
	}
	if subtle.ConstantTimeCompare(mac[:], rt.commitMAC[:]) != 1 {
		return // not a valid commit: packet dropped without ACK upstream
	}
	if !rt.committed {
		rt.committed = true
		c.promoteStaged()
	}
	// Idempotent duplicates receive the same logical confirmation.
	c.queueCtrl(ctrlKeyConfirm, 0, uint64(epoch), 0, 0, rt.id)
}

// applyKeyConfirm handles KEY_CONFIRM inside a new-epoch packet
// (client side, spec section 19.4).
func (c *Conn) applyKeyConfirm(body []byte) {
	core := c.core
	rt := &core.refresh
	epoch, id, mac, ok := wire.ParseKeyCommit(body)
	if !ok {
		return
	}
	if core.role != roleClient || !rt.active || !rt.commitSent {
		return
	}
	if epoch != rt.epoch || id != rt.id {
		c.closeWith(codeKeyUpdate, wire.FrameKeyConfirm)
		return
	}
	if subtle.ConstantTimeCompare(mac[:], rt.confirmMAC[:]) != 1 {
		c.closeWith(codeKeyUpdate, wire.FrameKeyConfirm)
		return
	}
	if !rt.confirmed {
		rt.confirmed = true
		c.promoteStaged()
		c.finishRefresh() // client transaction closes at confirm
	}
}

// promoteStaged makes STAGED current and demotes old CURRENT to PREVIOUS
// with a three-PTO grace before erasure (spec section 19.4). The staged
// slot is the one matching the live transaction's epoch.
func (c *Conn) promoteStaged() {
	core := c.core
	rt := &core.refresh
	staged := core.keys.find(rt.epoch)
	if staged == nil {
		return
	}
	for i := range core.keys.slots {
		s := &core.keys.slots[i]
		if s.state == epochSlotInstalled && s.epoch == rt.epoch {
			core.keys.cur = i
			continue
		}
		if s.state == epochSlotInstalled && s.epoch == rt.epoch-1 {
			fsmEpochSlotSet(&s.state, epochSlotGrace)
			s.graceUntilUS = nowUS() + 3*c.ptoUS()
			core.arm(tkGrace, s.graceUntilUS)
		}
	}
}

// onGraceDeadline erases grace-expired PREVIOUS slots and rearms while
// any remain (spec section 19: previous-epoch grace then erasure).
func (c *Conn) onGraceDeadline(now uint64) {
	core := c.core
	var next uint64
	found := false
	for i := range core.keys.slots {
		s := &core.keys.slots[i]
		if s.state != epochSlotGrace {
			continue
		}
		if now >= s.graceUntilUS {
			old := s.epoch
			s.tx.Zeroize()
			s.rx.Zeroize()
			fsmEpochSlotSet(&s.state, epochSlotEmpty)
			core.keys.root[i] = [32]byte{}
			// Transmissions stranded under the erased epoch detach
			// administratively: items requeue under the current epoch,
			// counted bytes release once, and no congestion evidence
			// accrues (section 19.5).
			for sp := core.ledger.liveHead; sp != nil; {
				next := sp.liveNext
				if sp.epoch == old {
					c.declareAbandoned(sp)
				}
				sp = next
			}
			continue
		}
		if !found || s.graceUntilUS < next {
			next, found = s.graceUntilUS, true
		}
	}
	if found {
		core.arm(tkGrace, next)
	}
}

// finishRefresh closes out a completed transaction (spec section 19.4):
// the client's commit is confirmed, or a KEY_CONFIRM-carrying packet was
// ACKed server-side. Transaction state and the deadline are cleared; the
// PREVIOUS slot drains on its own grace deadline.
func (c *Conn) finishRefresh() {
	rt := &c.core.refresh
	if !rt.active {
		return
	}
	id, hash := rt.id, rt.answeredHash
	*rt = refreshTx{} // drops offer/answer buffers and reassembly state
	if c.core.role == roleServer {
		rt.lastDoneID, rt.lastDoneHash, rt.hasLastDone = id, hash, true
	}
	c.core.disarm(tkRefresh)
}

// applyKeyRequest queues a refresh on an effective KEY_REQUEST (client).
func (c *Conn) applyKeyRequest(epoch uint32) {
	core := c.core
	if core.role != roleClient {
		return
	}
	cur := core.keys.current()
	if cur == nil || epoch != cur.epoch+1 {
		return // stale/mismatched request: ignore
	}
	c.initiateKeyRefresh()
}

// refreshCommitMAC returns the pending commit MAC for emission.
func (c *Conn) refreshCommitMAC() [32]byte { return c.core.refresh.commitMAC }

// refreshConfirmMAC returns the pending confirm MAC for emission.
func (c *Conn) refreshConfirmMAC() [32]byte { return c.core.refresh.confirmMAC }

// onRefreshDeadline expires an uncompleted refresh and paces KEY_COMMIT
// retransmission (section 19.4/19.5). The timer is armed at the earlier
// of the transaction deadline and the next commit slot.
func (c *Conn) onRefreshDeadline(now uint64) {
	core := c.core
	rt := &core.refresh
	if rt.active {
		if now >= rt.deadline {
			c.closeWith(codeKeyUpdate, 0)
			return
		}
		// Commit pacing is consumed by txDataPacket this cycle.
		c.armRefreshPace()
		return
	}
	// Threshold watcher: trigger or request refresh before hard limits.
	c.refreshWatch(now)
}

// refreshWatch runs the soft-limit triggers while no transaction is
// open: a client initiates on packet/byte/time thresholds; a server
// emits KEY_REQUEST for the next epoch (section 19.2).
func (c *Conn) refreshWatch(now uint64) {
	core := c.core
	rt := &core.refresh
	if rt.active {
		return
	}
	s := core.keys.current()
	if s == nil {
		return
	}
	due := c.softLimitHit()
	if !due && s.installedUS != 0 && now-s.installedUS >= wire.RekeySoftTimeUS {
		due = true
	}
	if !due {
		return
	}
	switch core.role {
	case roleClient:
		c.initiateKeyRefresh()
	case roleServer:
		if !rt.needRequest {
			rt.needRequest = true
			c.queueCtrl(ctrlKeyRequest, 0, 0, 0, s.epoch+1, [16]byte{})
		}
	}
}
