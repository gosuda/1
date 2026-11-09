package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// stubSock records sent datagrams; fail toggles send errors.
type stubSock struct {
	addr netip.AddrPort
	sent [][]byte
	ecns []uint8
	fail bool
}

var errStubRecv = errors.New("stub: no datagram")

func (s *stubSock) Send(b []byte, dst netip.AddrPort, ecn uint8) error {
	if s.fail {
		return errors.New("stub: send failed")
	}
	s.sent = append(s.sent, append([]byte(nil), b...))
	s.ecns = append(s.ecns, ecn)
	return nil
}

func (s *stubSock) LocalAddr() netip.AddrPort { return s.addr }

func (s *stubSock) Recv(buf []byte) (int, netip.AddrPort, netip.AddrPort, uint8, error) {
	return 0, netip.AddrPort{}, netip.AddrPort{}, 0, errStubRecv
}

// queueSock captures QueueSend payloads for the asynchronous QueueSocket
// path. Reports are driven by the test through c.sendReport; captured
// payloads are released back to the packet pool per the adapter contract.
type queueSock struct {
	addr     netip.AddrPort
	sent     [][]byte
	tags     []uint64
	reporter func(tag uint64, err error)
}

func (q *queueSock) Send(b []byte, dst netip.AddrPort, ecn uint8) error { return nil }

func (q *queueSock) LocalAddr() netip.AddrPort { return q.addr }

func (q *queueSock) QueueSend(payload []byte, dst netip.AddrPort, ecn uint8, tag uint64) {
	q.sent = append(q.sent, append([]byte(nil), payload...))
	q.tags = append(q.tags, tag)
	PutPacketBuf(payload)
}

func (q *queueSock) SetSendReporter(f func(tag uint64, err error)) { q.reporter = f }

// unitConn returns a conn in stEstablished with epoch-0 keys installed,
// one validated active path, and the given socket. It runs no goroutines;
// callers drive internal methods directly.
func unitConn(t *testing.T, sock Socket) *Conn {
	t.Helper()
	cfg := &Config{
		Params:      wire.DefaultParams(),
		MaxLiveSent: 4096,
		NowUnix:     func() int64 { return time.Now().Unix() },
	}
	c := newConn(cfg, sock, 1)
	var key [32]byte
	var iv [24]byte
	rand.Read(key[:])
	rand.Read(iv[:])
	ae, err := gc.NewEpochAEAD(wire.CipherXChaCha20Poly1305, key[:], iv[:])
	if err != nil {
		t.Fatal(err)
	}
	c.core.keys.slots[0] = epochSlot{
		state: epochSlotInstalled, epoch: 0, tx: ae, rx: ae,
		installedUS: nowUS(),
	}
	c.core.keys.cur = 0
	c.core.peerCID = cidRec{cid: 7, valid: true}
	c.core.peer = wire.DefaultParams() // peer advertised all features
	c.core.paths[0] = pathRec{
		role:      pathActive,
		pstate:    psActive,
		validated: true,
		remote:    netip.MustParseAddrPort("10.0.0.1:4433"),
		local:     sock.LocalAddr(),
		m:         wire.BaseUDPPayload,
		cwnd:      1 << 20,
		ssthresh:  1 << 20,
	}
	c.core.activePth = 0
	c.core.state = stEstablished
	return c
}

// TestAckFloorStraddle: an ACK range straddling the compact-history floor
// must be accepted (below-floor members are unverifiable, not errors), and
// a range entirely below the floor must be ignored — not fatal.
func TestAckFloorStraddle(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	for pn := uint64(0); pn < 1000; pn++ {
		core.ledger.reserve(pn)
		core.ledger.set(pn, sentEmitted)
	}
	core.nextPN = 1000
	// floor is now 1000-256+1 = 745.
	var a wire.AckFrame
	a.Count = 1
	a.Ranges[0] = [2]uint64{700, 999}
	if got := c.preflightAck(&a); got != codeNoError {
		t.Fatalf("straddling ACK rejected: %v", got)
	}
	a.Ranges[0] = [2]uint64{10, 100}
	if got := c.preflightAck(&a); got != codeNoError {
		t.Fatalf("below-floor ACK rejected: %v", got)
	}
	// An in-window PN that was never committed is still an error.
	core.ledger.reserve(1500)
	core.nextPN = 1501
	a.Ranges[0] = [2]uint64{1490, 1500}
	if got := c.preflightAck(&a); got != codeAckRange {
		t.Fatalf("unemitted in-window ACK accepted: %v", got)
	}
}

// TestCloseBodyParseable: txClose must emit the section-10 24-byte CLOSE
// body that ParseClose accepts.
func TestCloseBodyParseable(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	c.core.state = stClosing
	c.core.closeCode = codeNoError
	c.txClose(nowUS())
	if len(sock.sent) != 1 {
		t.Fatalf("close emitted %d packets", len(sock.sent))
	}
	pkt := sock.sent[0]
	h, ok := wire.ParseHeader(pkt)
	if !ok {
		t.Fatal("close packet header unparseable")
	}
	slot := c.core.keys.current()
	pt, err := slot.rx.Open(h.PacketNumber, pkt[:wire.HeaderBytes],
		pkt[wire.HeaderBytes:], nil)
	if err != nil {
		t.Fatal("close packet failed authentication")
	}
	var frames [wire.MaxFrames]wire.Frame
	fs, _, err := wire.ParseFrames(pt, frames[:])
	if err != nil {
		t.Fatal(err)
	}
	var closeSeen bool
	for _, f := range fs {
		if f.Type != wire.FrameClose {
			continue
		}
		closeSeen = true
		if _, _, _, ok := wire.ParseClose(f.Body); !ok {
			t.Fatalf("CLOSE body %d bytes is not the 24-byte wire form", len(f.Body))
		}
	}
	if !closeSeen {
		t.Fatal("no CLOSE frame in close packet")
	}
}

// TestNewCIDFields: the NEW_CID frame must carry the item's sequence in
// the sequence field — not retire_prior_to.
func TestNewCIDFields(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	cid := uint64(0x1122334455667788)
	var tok [16]byte
	tok[0] = 0xab
	if !c.queueCtrl(ctrlNewCID, 0, cid, 7, 0, tok) {
		t.Fatal("queueCtrl failed")
	}
	fb := wire.NewFrameBuilder(c.frameScratch())
	tb := &txBudget{}
	c.emitCtrl(fb, tb, wire.MaxUDPPayload, 0)
	var frames [wire.MaxFrames]wire.Frame
	fs, _, err := wire.ParseFrames(fb.Bytes(), frames[:])
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, f := range fs {
		if f.Type != wire.FrameNewCID {
			continue
		}
		seen = true
		seq, retire, gotCID, gotTok, ok := wire.ParseNewCID(f.Body)
		if !ok {
			t.Fatal("NEW_CID body unparseable")
		}
		if seq != 7 || retire != 0 || gotCID != cid || gotTok != tok {
			t.Fatalf("NEW_CID fields = seq %d retire %d cid %#x, want seq 7 retire 0 cid %#x",
				seq, retire, gotCID, cid)
		}
	}
	if !seen {
		t.Fatal("no NEW_CID emitted")
	}
}

// TestFragBitmapLifecycle: CRYPTO_FRAGMENT tracks outstanding vs
// in-flight fragments separately; an ACK clears only its fragment, and
// loss returns only that fragment to the pending set.
func TestFragBitmapLifecycle(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	// Three fragments: a=0b111 outstanding, a2 starts empty.
	msg := make([]byte, 2*wire.CryptoFragChunk+1)
	c.queueFrags([16]byte{9}, 1, msg) // kind 1 = OFFER
	it := &c.core.ctrl[0]
	if it.a != 0b111 {
		t.Fatalf("outstanding mask = %#b, want 0b111", it.a)
	}
	// Fragments 0 and 2 emitted: in-flight mask.
	it.a2 = 0b101
	c.ackCtrlItem(&lossItem{kind: itemCtrl, ctrlID: 0, ctrlGen: it.agen})
	if !it.used || it.a != 0b110 || it.a2 != 0b100 {
		t.Fatalf("after ack(0): used=%v a=%#b a2=%#b, want a=0b110 a2=0b100",
			it.used, it.a, it.a2)
	}
	// Lose the packet carrying fragment 2: returns to outstanding-only.
	c.lossCtrlItem(&lossItem{kind: itemCtrl, ctrlID: 0, ctrlGen: it.agen, lo: 2})
	if !it.used || it.a != 0b110 || it.a2 != 0 {
		t.Fatalf("after loss(2): a=%#b a2=%#b, want a=0b110 a2=0", it.a, it.a2)
	}
	c.ackCtrlItem(&lossItem{kind: itemCtrl, ctrlID: 0, ctrlGen: it.agen, lo: 1})
	c.ackCtrlItem(&lossItem{kind: itemCtrl, ctrlID: 0, ctrlGen: it.agen, lo: 2})
	if it.used {
		t.Fatal("frag item still live after all fragments acked")
	}
}

// TestZeroLenStreamEOF: a stream whose FIN arrives with no data must
// still deliver EOF to the application — and keep delivering it on every
// later read (section 12.6 terminal states are persistent results, not
// one-shot notifications).
func TestZeroLenStreamEOF(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	// Peer-initiated (server-initiated for a client conn) bidi stream.
	sid := wire.MakeStreamID(0, 0, false, wire.StreamInitiatorServer)
	s := core.slots.ensureSlot(sid.Class(), sid.Slot())
	s.state = slotActive
	s.initiator = sid.Initiator()
	s.recv = &recvHalf{state: recvOpen, limit: 1 << 20}
	c.applyStreamData(sid, 0, nil, true, nil)
	var buf [8]byte
	n, err := c.appConsume(uint64(sid), buf[:])
	if n != 0 || err != errEOF {
		t.Fatalf("appConsume = (%d, %v), want (0, EOF)", n, err)
	}
	// A read after EOF must not block: the terminal result persists.
	for i := 0; i < 3; i++ {
		if n, err = c.appConsume(uint64(sid), buf[:]); n != 0 || err != errEOF {
			t.Fatalf("post-EOF appConsume = (%d, %v), want (0, EOF)", n, err)
		}
	}
}

// TestEmitFailureRollback: a send failure must requeue logical items and
// leave the ACK obligation outstanding.
func TestEmitFailureRollback(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9"), fail: true}
	c := unitConn(t, sock)
	core := c.core
	if !c.queueCtrl(ctrlMaxData, 0, 0, 1<<20, 0, [16]byte{}) {
		t.Fatal("queueCtrl failed")
	}
	id := uint32(len(core.ctrl) - 1)
	fb := wire.NewFrameBuilder(c.frameScratch())
	tb := &txBudget{}
	c.emitCtrl(fb, tb, wire.MaxUDPPayload, 0)
	if !fb.Eliciting() {
		t.Fatal("control packet not eliciting")
	}
	slot := core.keys.current()
	pn0 := core.nextPN
	if c.emit(slot, 0, fb, tb, nowUS(), 0) {
		t.Fatal("emit succeeded on failing socket")
	}
	// PN was burned; item restored to pending.
	if got := core.ledger.status(pn0); got != sentBurned {
		t.Fatalf("pn status = %v, want burned", got)
	}
	if core.ctrl[id].live {
		t.Fatal("control item not returned to pending on send failure")
	}
	// Retry succeeds once the socket recovers.
	sock.fail = false
	core.ctrl[id].live = false
	fb2 := wire.NewFrameBuilder(c.frameScratch())
	tb2 := &txBudget{}
	c.emitCtrl(fb2, tb2, wire.MaxUDPPayload, 0)
	if !c.emit(slot, 0, fb2, tb2, nowUS(), 0) {
		t.Fatal("emit failed on healthy socket")
	}
	if len(sock.sent) != 1 {
		t.Fatalf("sent %d packets", len(sock.sent))
	}
}

// TestQueueSocketEmitCommit: a QueueSocket send commits as emitted at
// dispatch and a nil report leaves the ledger untouched.
func TestQueueSocketEmitCommit(t *testing.T) {
	sock := &queueSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	c.qsend = sock
	core := c.core
	if !c.queueCtrl(ctrlMaxData, 0, 0, 1<<20, 0, [16]byte{}) {
		t.Fatal("queueCtrl failed")
	}
	fb := wire.NewFrameBuilder(c.frameScratch())
	tb := &txBudget{}
	c.emitCtrl(fb, tb, wire.MaxUDPPayload, 0)
	slot := core.keys.current()
	pn0 := core.nextPN
	p0 := &core.paths[0]
	inFlight0 := p0.inFlight
	if !c.emit(slot, 0, fb, tb, nowUS(), 0) {
		t.Fatal("emit failed")
	}
	if len(sock.sent) != 1 || sock.tags[0] != pn0 {
		t.Fatalf("queued %d packets, tags %v", len(sock.sent), sock.tags)
	}
	if got := core.ledger.status(pn0); got != sentEmitted {
		t.Fatalf("pn status = %v, want emitted", got)
	}
	if p0.inFlight == inFlight0 {
		t.Fatal("eliciting queued packet not counted in flight")
	}
	c.sendReport(pn0, nil)
	if got := core.ledger.status(pn0); got != sentEmitted {
		t.Fatalf("status after nil report = %v, want emitted", got)
	}
	if core.ledger.get(pn0) == nil {
		t.Fatal("live entry dropped on success report")
	}
}

// TestQueueSocketSendReportFailure: a definitive send failure burns the
// PN and returns the payload items to pending, undoing the in-flight
// and per-path accounting of the dispatch-time commit.
func TestQueueSocketSendReportFailure(t *testing.T) {
	sock := &queueSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	c.qsend = sock
	core := c.core
	if !c.queueCtrl(ctrlMaxData, 0, 0, 1<<20, 0, [16]byte{}) {
		t.Fatal("queueCtrl failed")
	}
	id := uint32(len(core.ctrl) - 1)
	fb := wire.NewFrameBuilder(c.frameScratch())
	tb := &txBudget{}
	c.emitCtrl(fb, tb, wire.MaxUDPPayload, 0)
	slot := core.keys.current()
	pn0 := core.nextPN
	p0 := &core.paths[0]
	if !c.emit(slot, 0, fb, tb, nowUS(), 0) {
		t.Fatal("emit failed")
	}
	c.sendReport(sock.tags[0], errors.New("queue: transmit failed"))
	if got := core.ledger.status(pn0); got != sentBurned {
		t.Fatalf("pn status = %v, want burned", got)
	}
	if core.ctrl[id].live {
		t.Fatal("control item not returned to pending on report failure")
	}
	if p0.inFlight != 0 {
		t.Fatalf("inFlight %d after rollback, want 0", p0.inFlight)
	}
	if p0.txPkts != 0 || p0.txBytes != 0 {
		t.Fatalf("tx accounting not reverted: pkts=%d bytes=%d", p0.txPkts, p0.txBytes)
	}
}

// TestQueueSocketReportAfterResolve: a failure report arriving after the
// packet already resolved (ACK, loss, retransmission) must not corrupt
// the ledger — the outcome is ignored, not double-burned.
func TestQueueSocketReportAfterResolve(t *testing.T) {
	sock := &queueSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	c.qsend = sock
	core := c.core
	if !c.queueCtrl(ctrlMaxData, 0, 0, 1<<20, 0, [16]byte{}) {
		t.Fatal("queueCtrl failed")
	}
	id := uint32(len(core.ctrl) - 1)
	fb := wire.NewFrameBuilder(c.frameScratch())
	tb := &txBudget{}
	c.emitCtrl(fb, tb, wire.MaxUDPPayload, 0)
	slot := core.keys.current()
	pn0 := core.nextPN
	if !c.emit(slot, 0, fb, tb, nowUS(), 0) {
		t.Fatal("emit failed")
	}
	core.ledger.release(pn0, sentAcked)
	core.ctrl[id].live = false // item consumed by the ACK resolution
	c.sendReport(sock.tags[0], errors.New("queue: transmit failed"))
	if got := core.ledger.status(pn0); got != sentAcked {
		t.Fatalf("status = %v after late report, want acked", got)
	}
	if core.ctrl[id].live {
		t.Fatal("resolved item resurrected by late failure report")
	}
}

// TestMinPlaintextPadding: a sub-minimum non-eliciting packet gains a
// PING and PADDING up to MinDataPlaintext — a PADDING-only packet is not
// legal to ship (section 11).
func TestMinPlaintextPadding(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	fb := wire.NewFrameBuilder(c.frameScratch())
	// A lone empty PADDING frame: 8 bytes of plaintext, non-eliciting.
	fb.Add(wire.FramePadding, 0, 0, nil)
	slot := core.keys.current()
	if !c.emit(slot, 0, fb, tb0(), nowUS(), 0) {
		t.Fatal("emit failed")
	}
	pkt := sock.sent[len(sock.sent)-1]
	h, _ := wire.ParseHeader(pkt)
	pt, err := slot.rx.Open(h.PacketNumber, pkt[:wire.HeaderBytes],
		pkt[wire.HeaderBytes:], nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pt) < wire.MinDataPlaintext {
		t.Fatalf("plaintext %d < minimum %d", len(pt), wire.MinDataPlaintext)
	}
	var frames [wire.MaxFrames]wire.Frame
	fs, eliciting, err := wire.ParseFrames(pt, frames[:])
	if err != nil {
		t.Fatal(err)
	}
	if !eliciting {
		t.Fatal("PADDING-only packet emitted without an eliciting sibling")
	}
	var pad bool
	for _, f := range fs {
		if f.Type == wire.FramePadding {
			pad = true
		}
	}
	if !pad {
		t.Fatal("sub-minimum packet lacks PADDING")
	}
}

func tb0() *txBudget { return &txBudget{} }

// TestStatelessResetRecognition: an undecryptable datagram ending in a
// live peer-issued CID's reset token — active or a rotated table entry —
// is recognized; unknown tokens and short datagrams are ignored.
func TestStatelessResetRecognition(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	var tok, tok2 [16]byte
	rand.Read(tok[:])
	rand.Read(tok2[:])
	c.core.peerCID.token = tok2
	c.core.peerCIDs[0] = cidRec{cid: 99, seq: 1, token: tok, valid: true}
	pkt := append([]byte("some garbage datagram that failed decryption"), tok[:]...)
	if !c.checkStatelessReset(pkt) {
		t.Fatal("rotated peer CID token not recognized")
	}
	pkt[len(pkt)-16+5] ^= 0xff
	if c.checkStatelessReset(pkt) {
		t.Fatal("corrupted token recognized")
	}
	if c.checkStatelessReset([]byte{1, 2, 3}) {
		t.Fatal("short datagram recognized")
	}
	pktA := append([]byte("another garbage datagram........"), tok2[:]...)
	if !c.checkStatelessReset(pktA) {
		t.Fatal("active peer CID token not recognized")
	}
}

// TestStreamGenerationLifecycle: a RETIRED_WAIT_OPEN slot ignores every
// same-generation frame (they are stale, never errors), accepts only the
// next-generation OPEN (section 21.1), and commit-time application can
// never mutate a slot for a stale generation.
func TestStreamGenerationLifecycle(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer // peer initiates
	class := classIdx(false, wire.StreamInitiatorClient)
	core.slots.localLimit[class] = 4
	s := core.slots.ensureSlot(class, 0)
	s.state = slotRetiredWaitOpen
	s.gen = 0
	s.initiator = wire.StreamInitiatorClient

	sidG0 := wire.MakeStreamID(0, 0, false, wire.StreamInitiatorClient)
	sidG1 := wire.MakeStreamID(1, 0, false, wire.StreamInitiatorClient)
	sidG2 := wire.MakeStreamID(2, 0, false, wire.StreamInitiatorClient)

	// Stale same-generation OPEN: ignored, must not resurrect the slot.
	var pf rxPreflight
	if code := c.preflightOpen(sidG0, &pf); code != codeNoError {
		t.Fatalf("stale OPEN = %v, want ignored", code)
	}
	if pf.nOpened != 0 {
		t.Fatal("stale OPEN was admitted as a reopen")
	}
	c.applyOpen(sidG0)
	if s.state != slotRetiredWaitOpen || s.gen != 0 {
		t.Fatalf("stale OPEN mutated the slot: state=%v gen=%d", s.state, s.gen)
	}

	// Same-generation STREAM on the retired generation: stale, ignored.
	if code := c.preflightStreamData(sidG0, 0, 5, false, &pf); code != codeNoError {
		t.Fatalf("stale STREAM = %v, want ignored", code)
	}
	c.applyStreamData(sidG0, 0, []byte("stale"), false, nil)
	if s.recv != nil {
		t.Fatal("stale STREAM created receive state on a retired slot")
	}

	// Generation+2 is a future generation: protocol error.
	if code := c.preflightOpen(sidG2, &pf); code != codeStreamState {
		t.Fatalf("future OPEN = %v, want STREAM_STATE_ERROR", code)
	}

	// The generation+1 OPEN reopens with fresh halves.
	var pf2 rxPreflight
	if code := c.preflightOpen(sidG1, &pf2); code != codeNoError || pf2.nOpened != 1 {
		t.Fatalf("gen+1 OPEN = %v opened=%d", code, pf2.nOpened)
	}
	c.applyOpen(sidG1)
	if s.state != slotActive || s.gen != 1 || s.recv == nil || s.send == nil {
		t.Fatalf("reopen failed: state=%v gen=%d recv=%v send=%v",
			s.state, s.gen, s.recv != nil, s.send != nil)
	}

	// Stale generation-0 data cannot contaminate generation 1.
	c.applyStreamData(sidG0, 0, []byte("stale-bytes"), false, nil)
	if s.recv.high != 0 {
		t.Fatal("stale-generation bytes entered the live generation")
	}
	c.applyStreamData(sidG1, 0, []byte("live"), false, nil)
	if s.recv.high != 4 {
		t.Fatalf("live bytes not applied: high=%d", s.recv.high)
	}

	// A stale RESET must not kill generation 1; a live one does.
	c.applyReset(sidG0, 0, 1)
	if s.recv.state == recvReset {
		t.Fatal("stale RESET terminated the live generation")
	}
	c.applyReset(sidG1, 4, 7)
	if s.recv.state != recvReset || s.recv.resetCode != 7 {
		t.Fatal("live RESET not applied")
	}
}

// TestRetireAckAfterAppRead: retirement arriving while the application
// still holds unread bytes parks in RETIRE_PENDING; the final appConsume
// is what discharges the RETIRE_ACK and moves the slot to
// RETIRED_WAIT_OPEN (section 21.1: unread bytes prevent the step).
func TestRetireAckAfterAppRead(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer
	class := classIdx(true, wire.StreamInitiatorClient)
	core.slots.localLimit[class] = 4
	s := core.slots.ensureSlot(class, 0)
	s.state = slotActive
	s.gen = 0
	s.uni = true
	s.initiator = wire.StreamInitiatorClient
	s.recv = &recvHalf{state: recvOpen, limit: 1 << 20}

	sid := wire.MakeStreamID(0, 0, true, wire.StreamInitiatorClient)
	c.applyStreamData(sid, 0, []byte("payload"), true, nil)
	if code := c.preflightRetire(sid); code != codeNoError {
		t.Fatalf("RETIRE = %v", code)
	}
	c.applyRetire(sid)
	if s.state != slotRetirePending {
		t.Fatalf("state = %v, want RETIRE_PENDING while bytes unread", s.state)
	}

	// Application drains the stream: that is what discharges the ACK.
	var buf [16]byte
	for {
		_, err := c.appConsume(uint64(sid), buf[:])
		if err == errEOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if s.state != slotRetiredWaitOpen {
		t.Fatalf("state = %v, want RETIRED_WAIT_OPEN after drain", s.state)
	}
	var ackItem bool
	for i := range core.ctrl {
		if core.ctrl[i].used && core.ctrl[i].kind == ctrlRetireAck {
			ackItem = true
		}
	}
	if !ackItem {
		t.Fatal("no RETIRE_ACK queued after halves went terminal")
	}
}

// TestAckSchedulingTriggers: the ACK deadline uses the receiver's own
// advertised max_ack_delay, and immediate triggers fire on newly observed
// gaps, newly filled gaps / out-of-order arrivals, and CE-marked commits
// (section 13.2).
func TestAckSchedulingTriggers(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.local.MaxAckDelayUS = 500
	slot := &core.keys.slots[0]
	src := core.paths[0].remote

	ev := func(ecn uint8) *connEvent {
		return &connEvent{kind: evRX, pkt: make([]byte, 64), src: src, ecn: ecn}
	}
	hdr := func(pn uint64) wire.Header { return wire.Header{PacketNumber: pn} }
	ping := []wire.Frame{{Type: wire.FramePing}}

	// First committed eliciting packet arms the deadline with our own
	// advertised delay; in-order arrival is not an immediate trigger.
	before := nowUS()
	c.commitData(ev(0), hdr(0), ping, true, slot, nil)
	after := nowUS()
	if !core.acker.dueValid {
		t.Fatal("first eliciting commit did not arm the ACK deadline")
	}
	if core.acker.dueUS < before+500 || core.acker.dueUS > after+500 {
		t.Fatalf("deadline %d not in [%d,%d]", core.acker.dueUS, before+500, after+500)
	}
	if core.acker.immediate {
		t.Fatal("in-order first packet set the immediate trigger")
	}
	c.commitData(ev(0), hdr(1), ping, true, slot, nil)
	if core.acker.immediate {
		t.Fatal("sequential commit set the immediate trigger")
	}
	// New gap: packet 3 commits while largest is 1.
	c.commitData(ev(0), hdr(3), ping, true, slot, nil)
	if !core.acker.immediate {
		t.Fatal("newly observed gap did not force an immediate ACK")
	}
	core.acker.reset()
	// Filling the gap (2 commits below largest 3): immediate.
	c.commitData(ev(0), hdr(2), ping, true, slot, nil)
	if !core.acker.immediate {
		t.Fatal("gap fill did not force an immediate ACK")
	}
	core.acker.reset()
	// CE-marked in-order eliciting packet: immediate.
	c.commitData(ev(3), hdr(4), ping, true, slot, nil)
	if !core.acker.immediate {
		t.Fatal("CE-marked commit did not force an immediate ACK")
	}
}

// TestRetryRateLimited: a fragment-zero flood from one spoofed source is
// bounded by the per-source packet bucket; distinct sources are bounded
// independently under the global bucket (section 6.3).
func TestRetryRateLimited(t *testing.T) {
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	cliEP2 := n.endpoint(netip.MustParseAddrPort("10.0.0.3:5000"))
	_, scfg := testConfigs(t)
	l := NewListener(scfg, srvEP, []byte("test-domain"))

	var attempt [16]byte
	attempt[0] = 0xa5
	// Minimal structurally valid tokenless client message: the checks
	// sendRetry applies are msg/version, offer count, embedded attempt id
	// and a nonzero client_initial_cid.
	msg := make([]byte, 96)
	msg[0] = wire.MsgClient
	msg[1] = wire.Version
	msg[4] = 1
	copy(msg[12:28], attempt[:])
	binary.BigEndian.PutUint64(msg[60:], 0xdd)
	frag := wire.BuildFragments(msg, wire.ClassClientFlight, 0xbeef,
		attempt, [wire.TokenBytes]byte{})[0]
	dst := srvEP.addr

	for i := 0; i < 64; i++ {
		l.OnDatagram(frag, cliEP.addr, dst, 0)
	}
	if got := len(cliEP.ch); got == 0 || got > int(retrySrcPktBurst) {
		t.Fatalf("per-source retries = %d, want 1..%d", got, int(retrySrcPktBurst))
	}
	for i := 0; i < 64; i++ {
		l.OnDatagram(frag, cliEP2.addr, dst, 0)
	}
	if got := len(cliEP2.ch); got == 0 || got > int(retrySrcPktBurst) {
		t.Fatalf("second-source retries = %d, want 1..%d", got, int(retrySrcPktBurst))
	}
	if total := len(cliEP.ch) + len(cliEP2.ch); total > int(retryGlobalPktBurst) {
		t.Fatalf("retry total %d exceeds the global burst", total)
	}
}

// TestParkWakeDeadline: a wake on a not-yet-ready ingress path enters
// WAKE_PENDING and starts route validation; control ticks must not
// silently abandon the wake while the route is unproven (section 35.9:
// WAKE_PENDING is bounded by the lease or ordinary-idle deadline, not a
// local cap). Once the path validates, PARK_AWAKE is staged.
func TestParkWakeDeadline(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer
	core.parkSt = parkLive
	core.parkID = 7
	core.peerWakeSeq = 0
	core.parkLeaseUS = nowUS() + 600_000_000
	core.paths[1] = pathRec{
		role:   pathCandidate,
		remote: netip.MustParseAddrPort("10.0.0.5:7777"),
		local:  sock.LocalAddr(),
		m:      wire.BaseUDPPayload,
	}
	c.applyParkWake(7, 1, 1)
	if core.parkSt != parkWaking {
		t.Fatalf("parkSt = %v, want WAKE_PENDING", core.parkSt)
	}
	if !core.paths[1].validating {
		t.Fatal("wake ingress path was not put into validation")
	}
	for i := 0; i < 3; i++ {
		c.onParkCtrl(nowUS())
		if core.parkSt != parkWaking {
			t.Fatalf("wake abandoned during path validation: parkSt=%v", core.parkSt)
		}
	}
	// A stale ACTIVE wake path must not emit AWAKE before re-proof.
	core.paths[1].role = pathActive
	core.paths[1].pstate = psActive
	core.paths[1].stale = true
	core.paths[1].validating = false
	c.wakeAdvance(nowUS())
	if core.parkSt != parkWaking {
		t.Fatalf("stale active path emitted AWAKE: parkSt=%v", core.parkSt)
	}
	// Fresh proof (READY, non-stale) releases the result.
	core.paths[1].stale = false
	core.paths[1].pstate = psReady
	c.wakeAdvance(nowUS())
	if core.parkSt != parkAwakePend {
		t.Fatalf("ready path did not stage AWAKE: parkSt=%v", core.parkSt)
	}
}

// TestAckBelowFloorLive: a retained live sent entry below the compact
// history floor remains verifiable proof of emission — its ACK is
// resolved by direct lookup rather than dropped as unverifiable
// (spec section 13.3).
func TestAckBelowFloorLive(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	if !c.queueCtrl(ctrlPing, 0, 0, 0, 0, [16]byte{}) {
		t.Fatal("queueCtrl failed")
	}
	idx := uint32(len(core.ctrl) - 1)
	agen := core.ctrl[idx].agen
	// Move the history floor past PN 5.
	for pn := uint64(0); pn < 1000; pn++ {
		core.ledger.reserve(pn)
		core.ledger.set(pn, sentEmitted)
	}
	core.nextPN = 1000
	// Retain a live below-floor entry carrying the ping.
	sp := &sentPkt{pn: 5, sentUS: nowUS() - 1000, bytes: 100, path: 0,
		ordinal: 1, eliciting: true, inFlight: true,
		items: []lossItem{{kind: itemCtrl, ctrlID: idx, ctrlGen: agen}}}
	core.paths[0].inFlight += uint64(sp.bytes)
	core.ledger.track(sp)
	var a wire.AckFrame
	a.Count = 1
	a.Ranges[0] = [2]uint64{5, 5}
	c.applyAck(&a, nowUS(), 0)
	if int(idx) < len(core.ctrl) && core.ctrl[idx].used {
		t.Fatal("below-floor live packet's control item not resolved")
	}
	if core.ledger.get(5) != nil {
		t.Fatal("acked live entry still retained")
	}
	if core.paths[0].inFlight != 0 {
		t.Fatalf("in-flight bytes not released: %d", core.paths[0].inFlight)
	}
}

// TestCtrlGenerationIsolation: a loss/ACK reference taken before a ctrl
// slot was recycled must resolve nothing — allocation generation plus
// the carried value gate stale references (spec section 36.5).
func TestCtrlGenerationIsolation(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	c.queueCtrl(ctrlPing, 0, 0, 0, 0, [16]byte{})
	it := &core.ctrl[0]
	oldGen := it.agen
	it.live = true
	// Resolve the first obligation, then requeue the same slot.
	c.ackCtrlItem(&lossItem{kind: itemCtrl, ctrlID: 0, ctrlGen: oldGen})
	if it.used {
		t.Fatal("ping not resolved by its own ACK")
	}
	c.queueCtrl(ctrlPing, 0, 0, 0, 0, [16]byte{})
	it = &core.ctrl[0]
	if !it.used || it.agen == oldGen {
		t.Fatalf("slot not recycled with fresh generation (agen %d)", it.agen)
	}
	it.live = true // pretend emitted
	// A stale ACK/loss naming the old generation must not touch it.
	c.ackCtrlItem(&lossItem{kind: itemCtrl, ctrlID: 0, ctrlGen: oldGen})
	if !it.used {
		t.Fatal("stale ACK resolved a recycled control item")
	}
	c.lossCtrlItem(&lossItem{kind: itemCtrl, ctrlID: 0, ctrlGen: oldGen})
	if !it.live {
		t.Fatal("stale loss reference requeued a recycled control item")
	}
	// The current generation still resolves normally.
	c.lossCtrlItem(&lossItem{kind: itemCtrl, ctrlID: 0, ctrlGen: it.agen})
	if it.live {
		t.Fatal("same-generation loss did not requeue the item")
	}
}

// TestMaxPendingCtrlBound: the local admission limit caps used control
// obligations below the wire table bound.
func TestMaxPendingCtrlBound(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	c.core.cfg.MaxPendingCtrl = 2
	sid := uint64(wire.MakeStreamID(0, 0, false, wire.StreamInitiatorClient))
	if !c.queueCtrl(ctrlMaxStreamData, 0, sid, 100, 0, [16]byte{}) {
		t.Fatal("first control refused")
	}
	if !c.queueCtrl(ctrlMaxStreamData, 1, sid, 100, 0, [16]byte{}) {
		t.Fatal("second control refused")
	}
	if c.queueCtrl(ctrlMaxStreamData, 2, sid, 100, 0, [16]byte{}) {
		t.Fatal("third control admitted past MaxPendingCtrl")
	}
}

// TestProbeBucketBound: the persistent locally initiated probe bucket
// admits three datagrams then throttles to one per second (spec 17.6).
func TestProbeBucketBound(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	now := nowUS()
	for i := 0; i < 3; i++ {
		if !c.probeSpend(now) {
			t.Fatalf("probe %d denied inside burst capacity", i)
		}
	}
	if c.probeSpend(now) {
		t.Fatal("fourth probe granted with an empty bucket")
	}
	if w := c.probeWaitUS(now); w == 0 {
		t.Fatal("empty bucket reported no wait")
	}
	if !c.probeSpend(now + 1_000_000) {
		t.Fatal("bucket did not refill one token after one second")
	}
}

// TestCIDSequenceZero: issued CIDs begin at sequence zero (spec section
// 18); the handshake-issued initial CID lives outside the sequence space.
func TestCIDSequenceZero(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.localCID = cidRec{cid: 3, seq: cidSeqHandshake, valid: true}
	core.peer.ActiveCIDLimit = 4
	c.issueCID()
	r0 := &core.localCIDs[0]
	if !r0.valid || r0.seq != 0 {
		t.Fatalf("first issued CID seq = %d, want 0", r0.seq)
	}
	// The queued NEW_CID carries sequence 0.
	var it *ctrlItem
	for i := range core.ctrl {
		if core.ctrl[i].used && core.ctrl[i].kind == ctrlNewCID {
			it = &core.ctrl[i]
		}
	}
	if it == nil || it.b != 0 {
		t.Fatal("queued NEW_CID does not carry sequence 0")
	}
	// RETIRE_CID(0) retires the seq-0 record and triggers a seq-1 reissue
	// (the table slot may be reused immediately).
	c.applyRetireCID(0)
	if r0.valid && r0.seq == 0 {
		t.Fatal("RETIRE_CID(0) left the seq-0 record live")
	}
	if core.localCIDSeq != 2 {
		t.Fatalf("localCIDSeq = %d after issue+reissue, want 2", core.localCIDSeq)
	}
	var found bool
	for i := range core.localCIDs {
		if core.localCIDs[i].valid && core.localCIDs[i].seq == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("retirement did not reissue sequence 1")
	}
	// Retiring a never-issued sequence is CONNECTION_ID_ERROR.
	c.applyRetireCID(99)
	te, ok := core.termErr.(*TransportError)
	if !ok || te.Code != codeConnectionID {
		t.Fatalf("RETIRE_CID(99) termErr = %v, want CONNECTION_ID_ERROR", core.termErr)
	}
}

// TestCloseCodeWide: application close codes travel in the u32 wire
// field without truncation (spec section 11).
func TestCloseCodeWide(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	c.core.state = stClosing
	c.core.closeCode = closeCode(0x1FF)
	c.txClose(nowUS())
	if len(sock.sent) != 1 {
		t.Fatalf("close emitted %d packets", len(sock.sent))
	}
	pkt := sock.sent[0]
	h, ok := wire.ParseHeader(pkt)
	if !ok {
		t.Fatal("close packet header unparseable")
	}
	slot := c.core.keys.current()
	pt, err := slot.rx.Open(h.PacketNumber, pkt[:wire.HeaderBytes],
		pkt[wire.HeaderBytes:], nil)
	if err != nil {
		t.Fatal(err)
	}
	var frames [wire.MaxFrames]wire.Frame
	fs, _, err := wire.ParseFrames(pt, frames[:])
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Type != wire.FrameClose {
			continue
		}
		code, _, _, ok := wire.ParseClose(f.Body)
		if !ok {
			t.Fatal("CLOSE body unparseable")
		}
		if code != 0x1FF {
			t.Fatalf("CLOSE code = %#x, want 0x1FF", code)
		}
		return
	}
	t.Fatal("no CLOSE frame emitted")
}

// TestErrPeerStop: a peer STOP_SENDING surfaces ErrPeerStop on writes
// and FIN to the affected send half (spec section 12.5).
func TestErrPeerStop(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	sid := wire.MakeStreamID(0, 0, false, wire.StreamInitiatorClient)
	s := core.slots.ensureSlot(sid.Class(), sid.Slot())
	s.state = slotActive
	s.gen = 0
	s.initiator = wire.StreamInitiatorClient
	s.send = &sendHalf{state: sendOpen, limit: 1 << 20}
	c.applyStopSending(sid, 5)
	if !s.send.stopReq {
		t.Fatal("stop request not recorded")
	}
	if _, err := c.appWrite(uint64(sid), []byte("x")); err != errPeerStop {
		t.Fatalf("appWrite after STOP_SENDING = %v, want ErrPeerStop", err)
	}
	if err := c.appFin(uint64(sid)); err != errPeerStop {
		t.Fatalf("appFin after STOP_SENDING = %v, want ErrPeerStop", err)
	}
}

// TestRetentionAbandoned: reclaiming an OLD path detaches its live
// packets as ABANDONED — logical items requeue, in-flight bytes release,
// and no congestion response runs (spec section 17.5/17.7).
func TestRetentionAbandoned(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.paths[1] = pathRec{
		role:      pathOld,
		validated: true,
		remote:    netip.MustParseAddrPort("10.0.0.1:4433"),
		local:     sock.LocalAddr(),
		m:         wire.BaseUDPPayload,
		cwnd:      1 << 20,
		ssthresh:  1 << 20,
	}
	c.queueCtrl(ctrlPing, 0, 0, 0, 0, [16]byte{})
	idx := uint32(len(core.ctrl) - 1)
	agen := core.ctrl[idx].agen
	core.ctrl[idx].live = true
	sp := &sentPkt{pn: 10, sentUS: nowUS() - 1000, bytes: 200, path: 1,
		ordinal: 1, eliciting: true, inFlight: true,
		items: []lossItem{{kind: itemCtrl, ctrlID: idx, ctrlGen: agen}}}
	core.paths[1].inFlight = 200
	core.ledger.reserve(10)
	core.ledger.track(sp)
	c.onRetentionDeadline(nowUS())
	if core.paths[1].role != pathFree {
		t.Fatalf("OLD path role = %v after retention", core.paths[1].role)
	}
	if core.ledger.get(10) != nil {
		t.Fatal("detached packet still live")
	}
	if core.ledger.status(10) != sentAbandoned {
		t.Fatalf("pn 10 status = %v, want ABANDONED", core.ledger.status(10))
	}
	if core.paths[1].inFlight != 0 {
		t.Fatal("abandoned bytes still charged in-flight")
	}
	if core.ctrl[idx].live {
		t.Fatal("abandoned packet's item not requeued")
	}
	if core.paths[1].cwnd != 1<<20 {
		t.Fatal("ABANDONED detachment triggered a congestion response")
	}
}

// TestParkRequestDeadlineNormalizes: after all three request sends the
// fixed request deadline still fires — an uncertain result becomes a
// wake attempt, never a silent wedge (spec section 35.8).
func TestParkRequestDeadlineNormalizes(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleClient
	core.parked = true
	core.parkSt = parkReqWait
	core.parkID = 4
	core.parkSends = 3 // all three transmissions spent
	core.ctrlPTO = 100000
	now := nowUS()
	core.parkBaseUS = now - 700000
	core.parkNextUS = now - 1
	core.parkDL = now - 1 // deadline reached
	c.onParkCtrl(now)
	if core.parkSt != parkWaking {
		t.Fatalf("request deadline did not normalize to wake: parkSt=%v",
			core.parkSt)
	}
	if core.wakeSeq == 0 {
		t.Fatal("wake attempt started without a wake_seq")
	}
	var found bool
	for i := range core.ctrl {
		if core.ctrl[i].used && core.ctrl[i].kind == ctrlParkWake {
			found = true
		}
	}
	if !found {
		t.Fatal("no PARK_WAKE obligation queued after normalization")
	}
}

// TestDupClientFlightSourceBound: a repeated client flight reemits the
// cached server flight only to the attempt's own source tuple, at most
// once per retransmit round — never to an arbitrary source (spec 20.1).
func TestDupClientFlightSourceBound(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer
	core.state = stSFinishing
	var attempt [wire.AttemptIDBytes]byte
	rand.Read(attempt[:])
	msg := make([]byte, 200)
	rand.Read(msg)
	frags := wire.BuildFragments(msg, wire.ClassClientFlight, 99, attempt,
		[wire.TokenBytes]byte{})
	pkt := frags[0]
	f0, ok := wire.ParseFragment(pkt[wire.HeaderBytes:])
	if !ok {
		t.Fatal("built fragment unparseable")
	}
	core.hs.attemptID = attempt
	core.hs.flightHash = f0.MessageHash
	core.hs.serverFlight = [][]byte{[]byte("server-flight")}
	legit := netip.MustParseAddrPort("10.0.0.7:7777")
	core.paths[0].remote = legit
	h, _ := wire.ParseHeader(pkt)
	// A spoofed source carrying the attempt's identifiers earns nothing.
	e := &connEvent{kind: evRX, pkt: pkt, src: netip.MustParseAddrPort("9.9.9.9:9"),
		dst: sock.LocalAddr(), rxUS: nowUS()}
	c.rxClientFlightDup(e, h)
	if len(sock.sent) != 0 {
		t.Fatal("duplicate flight reemitted to a foreign source")
	}
	// The attempt's own source gets one reemission per retransmit round,
	// emitted through the paced flight emitter (section 6.4).
	e.src = legit
	c.rxClientFlightDup(e, h)
	c.txServerFlight(nowUS())
	if len(sock.sent) != 1 {
		t.Fatalf("legit duplicate reemitted %d packets, want 1", len(sock.sent))
	}
	c.rxClientFlightDup(e, h)
	c.txServerFlight(nowUS())
	if len(sock.sent) != 1 {
		t.Fatal("second in-round duplicate reemitted")
	}
}

// --- migration intent FSM (sections 17.4-17.6) ---

// intentFrame serializes a PATH_INTENT for commitData tests.
func intentFrame(seq, cutover uint64, flags uint32) wire.Frame {
	b := make([]byte, 24)
	wire.BuildPathIntent(b, seq, cutover, flags)
	return wire.Frame{Type: wire.FramePathIntent, Body: b}
}

// selectedQueued reports whether a used PATH_SELECTED item with the given
// status is queued for path pi.
func selectedQueued(c *Conn, pi int, status uint32) bool {
	for i := range c.core.ctrl {
		it := &c.core.ctrl[i]
		if it.used && it.kind == ctrlPathSelected &&
			int(it.path) == pi && it.c == status {
			return true
		}
	}
	return false
}

// TestPathIntentProbeOnlyNotEvidence: a probe-only PATH_INTENT (SELECT=0)
// is probing traffic — it must not advance the promotion evidence
// watermark and must not implicitly promote a READY candidate (section
// 17.4). A subsequent non-probing PING on the same tuple does promote.
func TestPathIntentProbeOnlyNotEvidence(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	local := sock.LocalAddr()
	act := core.paths[0].remote

	// Seed the active path's committed-PN watermark.
	c.commitData(&connEvent{kind: evRX, pkt: make([]byte, 64),
		src: act, dst: local},
		wire.Header{PacketNumber: 1}, []wire.Frame{{Type: wire.FramePing}},
		true, &core.keys.slots[0], nil)
	if core.paths[0].peerPN != 1 {
		t.Fatalf("active peerPN %d, want 1", core.paths[0].peerPN)
	}

	// READY peer-discovered candidate on a second tuple.
	core.paths[1] = pathRec{
		role:             pathCandidate,
		pstate:           psReady,
		validated:        true,
		remote:           netip.MustParseAddrPort("10.0.0.1:5555"),
		local:            local,
		m:                wire.BaseUDPPayload,
		cwnd:             1 << 20,
		ssthresh:         1 << 20,
		createdWatermark: 1,
	}
	cand := &core.paths[1]
	ev := &connEvent{kind: evRX, pkt: make([]byte, 64),
		src: cand.remote, dst: local}

	// Probe-only intent on the candidate: no evidence, no promotion,
	// no selection result owed.
	c.commitData(ev, wire.Header{PacketNumber: 2},
		[]wire.Frame{intentFrame(7, 0, 0)}, true, &core.keys.slots[0], nil)
	if cand.evidencePN != 0 {
		t.Fatalf("probe intent advanced evidencePN to %d", cand.evidencePN)
	}
	if core.activePth != 0 {
		t.Fatal("probe-only intent implicitly promoted the candidate")
	}
	if !core.peerIntentRec || core.peerIntentSel || core.peerIntentSeq != 7 {
		t.Fatal("probe intent was not recorded")
	}
	for i := range core.ctrl {
		if core.ctrl[i].used && core.ctrl[i].kind == ctrlPathSelected {
			t.Fatal("probe intent queued a PATH_SELECTED result")
		}
	}

	// A non-probing PING on the same tuple is implicit-promotion
	// evidence: fresher than the creation watermark and cutover floor.
	c.commitData(ev, wire.Header{PacketNumber: 3},
		[]wire.Frame{{Type: wire.FramePing}}, true, &core.keys.slots[0], nil)
	if cand.evidencePN != 3 {
		t.Fatalf("PING did not set evidencePN, got %d", cand.evidencePN)
	}
	if core.activePth != 1 {
		t.Fatal("fresh non-probing evidence did not promote the READY candidate")
	}
}

// TestPathIntentSelectOnActivePath: a SELECT landing on the already-active
// path is ACCEPTED and installs the cutover barrier without revoking the
// route or re-promoting (section 17.4).
func TestPathIntentSelectOnActivePath(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	local := sock.LocalAddr()
	act := core.paths[0].remote

	c.commitData(&connEvent{kind: evRX, pkt: make([]byte, 64),
		src: act, dst: local},
		wire.Header{PacketNumber: 5}, []wire.Frame{{Type: wire.FramePing}},
		true, &core.keys.slots[0], nil)

	promotedUS := core.paths[0].promotedUS
	c.applyPathIntent(9, 5, 1, 0)
	if !core.selOK || core.selStatus != 0 || core.selSeq != 9 {
		t.Fatalf("SELECT on active path: sel=%v status=%d seq=%d",
			core.selOK, core.selStatus, core.selSeq)
	}
	if !selectedQueued(c, 0, 0) {
		t.Fatal("ACCEPTED PATH_SELECTED was not queued on the active path")
	}
	if core.activePth != 0 {
		t.Fatal("SELECT on the active path moved routing")
	}
	if core.peerCutoverFloor != 5 {
		t.Fatalf("cutover barrier %d, want 5", core.peerCutoverFloor)
	}
	if core.paths[0].promotedUS != promotedUS {
		t.Fatal("matching SELECT re-promoted the active path")
	}
}

// TestPathIntentRepeatRegeneratesCached: a repeated SELECT for the
// recorded sequence regenerates the cached result without a second
// promotion; an inconsistent repeat on another path is a protocol error
// (section 17.4).
func TestPathIntentRepeatRegeneratesCached(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	local := sock.LocalAddr()
	act := core.paths[0].remote

	c.commitData(&connEvent{kind: evRX, pkt: make([]byte, 64),
		src: act, dst: local},
		wire.Header{PacketNumber: 4}, []wire.Frame{{Type: wire.FramePing}},
		true, &core.keys.slots[0], nil)

	c.applyPathIntent(11, 4, 1, 0)
	if !core.selOK || core.selStatus != 0 {
		t.Fatal("initial SELECT was not accepted")
	}
	// Free the emitted result; the repeat must regenerate it.
	for i := range core.ctrl {
		if core.ctrl[i].used && core.ctrl[i].kind == ctrlPathSelected {
			core.ctrl[i].used = false
		}
	}
	promotedUS := core.paths[0].promotedUS
	c.applyPathIntent(11, 4, 1, 0)
	if !selectedQueued(c, 0, 0) {
		t.Fatal("repeated SELECT did not regenerate the cached result")
	}
	if core.paths[0].promotedUS != promotedUS || core.activePth != 0 {
		t.Fatal("repeated SELECT triggered a second promotion")
	}
}

// TestPathIntentBusyDeclinedPreserveActive: SELECT on a still-validating
// candidate answers BUSY and on an unvalidated candidate answers
// DECLINED; neither disturbs the active route (section 17.4).
func TestPathIntentBusyDeclinedPreserveActive(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	local := sock.LocalAddr()
	act := core.paths[0].remote

	c.commitData(&connEvent{kind: evRX, pkt: make([]byte, 64),
		src: act, dst: local},
		wire.Header{PacketNumber: 2}, []wire.Frame{{Type: wire.FramePing}},
		true, &core.keys.slots[0], nil)

	// Candidate mid-validation: BUSY, active preserved.
	core.paths[1] = pathRec{
		role:       pathCandidate,
		pstate:     psValidating,
		validating: true,
		remote:     netip.MustParseAddrPort("10.0.0.1:5555"),
		local:      local,
		m:          wire.BaseUDPPayload,
		cwnd:       1 << 20,
		ssthresh:   1 << 20,
	}
	c.applyPathIntent(13, 2, 1, 1)
	if !core.selOK || core.selStatus != 2 {
		t.Fatalf("validating candidate: status=%d, want BUSY", core.selStatus)
	}
	if !selectedQueued(c, 1, 2) {
		t.Fatal("BUSY result not queued on the candidate path")
	}
	if core.activePth != 0 || core.paths[1].role != pathCandidate {
		t.Fatal("BUSY disturbed routing")
	}

	// Unvalidated candidate: DECLINED, active preserved.
	core.paths[2] = pathRec{
		role:     pathCandidate,
		pstate:   psReachable,
		remote:   netip.MustParseAddrPort("10.0.0.1:6666"),
		local:    local,
		m:        wire.BaseUDPPayload,
		cwnd:     1 << 20,
		ssthresh: 1 << 20,
	}
	c.applyPathIntent(17, 2, 1, 2)
	if !core.selOK || core.selStatus != 2 {
		t.Fatalf("reachable candidate: status=%d, want BUSY", core.selStatus)
	}
	core.paths[2].pstate = psFailed // validation exhausted: decline
	c.applyPathIntent(19, 2, 1, 2)
	if !core.selOK || core.selStatus != 1 {
		t.Fatalf("unvalidated candidate: status=%d, want DECLINED", core.selStatus)
	}
	if !selectedQueued(c, 2, 1) {
		t.Fatal("DECLINED result not queued on the candidate path")
	}
	if core.activePth != 0 {
		t.Fatal("DECLINED revoked the active path")
	}
}

// TestPathIntentSelectOldPathRevalidates: a SELECT on a retained OLD
// tuple starts peer-driven revalidation and answers BUSY; once the path
// reaches READY a retried SELECT is ACCEPTED and promotes it back
// (section 17.4: deliberate return to OLD requires a newer intent and
// revalidation).
func TestPathIntentSelectOldPathRevalidates(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	local := sock.LocalAddr()
	act := core.paths[0].remote

	c.commitData(&connEvent{kind: evRX, pkt: make([]byte, 64),
		src: act, dst: local},
		wire.Header{PacketNumber: 3}, []wire.Frame{{Type: wire.FramePing}},
		true, &core.keys.slots[0], nil)

	// Retained OLD path from an earlier promotion.
	core.paths[1] = pathRec{
		role:      pathOld,
		pstate:    psOld,
		validated: true,
		remote:    netip.MustParseAddrPort("10.0.0.1:5555"),
		local:     local,
		m:         wire.BaseUDPPayload,
		cwnd:      1 << 20,
		ssthresh:  1 << 20,
	}
	old := &core.paths[1]

	// SELECT on the OLD tuple: revalidation opens, answer is BUSY.
	c.applyPathIntent(21, 3, 1, 1)
	if !core.selOK || core.selStatus != 2 {
		t.Fatalf("OLD-path SELECT: status=%d, want BUSY", core.selStatus)
	}
	if !old.validating {
		t.Fatal("SELECT on OLD did not open revalidation")
	}
	if core.activePth != 0 {
		t.Fatal("BUSY moved routing")
	}

	// Validation completes with full-sized evidence in both directions.
	old.challengeBytes = wire.BaseUDPPayload
	old.challengeSentUS = nowUS()
	c.applyPathResponse(old.challenge, 1, wire.BaseUDPPayload)
	if old.pstate != psReady {
		t.Fatalf("OLD revalidation ended in pstate=%d, want READY", old.pstate)
	}
	if core.activePth != 0 {
		t.Fatal("READY OLD auto-promoted — OLD is never implicit evidence")
	}

	// The peer's retried SELECT — same sequence, immutable fields —
	// re-evaluates to ACCEPTED and promotes the path.
	c.applyPathIntent(21, 3, 1, 1)
	if !core.selOK || core.selStatus != 0 {
		t.Fatalf("retried SELECT on READY OLD: status=%d, want ACCEPTED",
			core.selStatus)
	}
	if core.activePth != 1 || old.role != pathActive {
		t.Fatal("accepted return-SELECT did not promote the OLD path")
	}
	if core.paths[0].role != pathOld {
		t.Fatal("former active path was not retained as OLD")
	}
}

// TestSamePacketOpenCredit: a credit frame naming a stream opened earlier
// in the SAME packet is legal (section 12.4: "unless the same packet
// opened it"). The slot table only grows at commit, so preflight must
// consult the transaction's opened set rather than the table.
func TestSamePacketOpenCredit(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer // peer initiates
	class := classIdx(false, wire.StreamInitiatorClient)
	core.slots.localLimit[class] = 4

	sid := wire.MakeStreamID(0, 0, false, wire.StreamInitiatorClient)
	var pf rxPreflight
	if code := c.preflightOpen(sid, &pf); code != codeNoError {
		t.Fatalf("OPEN = %v", code)
	}
	if pf.nOpened != 1 {
		t.Fatalf("OPEN not recorded in transaction: %d", pf.nOpened)
	}
	// Slot table is still ungrown: the credit must be admitted anyway.
	if code := c.preflightMaxStreamData(sid, &pf); code != codeNoError {
		t.Fatalf("same-packet MAX_STREAM_DATA = %v, want admitted", code)
	}
	if code := c.preflightStreamData(sid, 0, 8, false, &pf); code != codeNoError {
		t.Fatalf("same-packet STREAM = %v, want admitted", code)
	}
	if code := c.preflightReset(sid, 8, &pf); code != codeNoError {
		t.Fatalf("same-packet RESET = %v, want admitted", code)
	}
	// Without a same-packet OPEN the credit stays an error.
	var pf2 rxPreflight
	if code := c.preflightMaxStreamData(sid, &pf2); code != codeStreamState {
		t.Fatalf("unopened MAX_STREAM_DATA = %v, want STREAM_STATE_ERROR", code)
	}
}

// TestCtrlItemsUnpinned: body-carrying control items (NEW_TICKET,
// NEW_TOKEN, CRYPTO_FRAGMENT) must be emitted unpinned (path=-1) so they
// still leave after migration off path slot 0. A zero path value would
// pin them to slot 0 (ctrlForPath), breaking section-9/19 progress on
// the post-migration path.
func TestCtrlItemsUnpinned(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	c.queueCtrlBody(ctrlNewTicket, []byte("ticket-body"))
	c.queueCtrlBody(ctrlNewToken, []byte("token-body"))
	c.queueFrags([16]byte{7}, 1, []byte("crypto-message"))
	found := 0
	for i := range c.core.ctrl {
		it := &c.core.ctrl[i]
		if !it.used {
			continue
		}
		found++
		if it.path != -1 {
			t.Fatalf("ctrl kind=%d pinned to path %d", it.kind, it.path)
		}
		// Unpinned items must be emittable on any path, including a
		// post-migration active path that is not slot 0.
		if !c.ctrlForPath(uint32(i), 2) {
			t.Fatalf("ctrl kind=%d not emittable on path 2", it.kind)
		}
	}
	if found != 3 {
		t.Fatalf("queued %d items, want 3", found)
	}

	// Building a packet for non-zero path slot 2 must actually carry the
	// ticket/token bodies; crypto fragments flow via emitProgressFrames.
	fb := wire.NewFrameBuilder(c.frameScratch())
	tb := &txBudget{}
	c.emitProgressFrames(fb, tb, wire.MaxUDPPayload, 2)
	c.emitCtrl(fb, tb, wire.MaxUDPPayload, 2)
	fs, _, err := wire.ParseFrames(fb.Bytes(), make([]wire.Frame, wire.MaxFrames))
	if err != nil {
		t.Fatal(err)
	}
	var ticket, token, frag bool
	for _, f := range fs {
		switch f.Type {
		case wire.FrameNewTicket:
			ticket = true
		case wire.FrameNewToken:
			token = true
		case wire.FrameCryptoFragment:
			frag = true
		}
	}
	if !ticket || !token || !frag {
		t.Fatalf("path-2 packet missing frames: ticket=%v token=%v frag=%v",
			ticket, token, frag)
	}
}

// TestMaxSlotsGrantCeiling: replenishing a retired peer slot must never
// grow the grant past the authenticated maximum we advertised
// (section 12.1: "never exceed the authenticated maximum").
func TestMaxSlotsGrantCeiling(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer
	class := classIdx(false, wire.StreamInitiatorClient)
	core.local.MaximumBidiSlots = 4
	core.slots.localLimit[class] = 4 // already at the advertised ceiling
	s := core.slots.ensureSlot(class, 0)
	s.state = slotRetirePending
	s.gen = 0
	s.initiator = wire.StreamInitiatorClient
	s.send = &sendHalf{state: sendDone}
	s.recv = &recvHalf{state: recvDone}
	sid := wire.MakeStreamID(0, 0, false, wire.StreamInitiatorClient)

	c.maybeRetireAck(s, sid)
	if core.slots.localLimit[class] != 4 {
		t.Fatalf("grant grew past advertised maximum: %d",
			core.slots.localLimit[class])
	}
	for i := range core.ctrl {
		if core.ctrl[i].used && core.ctrl[i].kind == ctrlMaxSlots {
			t.Fatal("MAX_SLOTS queued beyond the authenticated maximum")
		}
	}

	// Below the ceiling the grant still replenishes.
	s.state = slotRetirePending
	core.slots.localLimit[class] = 3
	c.maybeRetireAck(s, sid)
	if core.slots.localLimit[class] != 4 {
		t.Fatalf("grant did not replenish: %d", core.slots.localLimit[class])
	}

	// The uni class uses its own parameter.
	uc := classIdx(true, wire.StreamInitiatorClient)
	core.local.MaximumUniSlots = 2
	core.slots.localLimit[uc] = 2
	us := core.slots.ensureSlot(uc, 0)
	us.state = slotRetirePending
	us.gen = 0
	us.uni = true
	us.initiator = wire.StreamInitiatorClient
	us.recv = &recvHalf{state: recvDone}
	usid := wire.MakeStreamID(0, 0, true, wire.StreamInitiatorClient)
	c.maybeRetireAck(us, usid)
	if core.slots.localLimit[uc] != 2 {
		t.Fatalf("uni grant grew past its maximum: %d",
			core.slots.localLimit[uc])
	}
}

// TestDRRStreamFairness: emitStreamData must serve eligible send halves
// in deficit round robin order (section 14.5) — a continuously writing
// low-slot stream cannot starve higher slots.
func TestDRRStreamFairness(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleClient
	core.peer.InitialStreamWindow = 1 << 20
	core.connTxLim = 1 << 24
	core.slots.peerLimit[classIdx(false, wire.StreamInitiatorClient)] = 8

	sidA, err := c.openStream(false)
	if err != nil {
		t.Fatal(err)
	}
	sidB, err := c.openStream(false)
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 32<<10)
	if _, err := c.appWrite(sidA, blob); err != nil {
		t.Fatal(err)
	}
	if _, err := c.appWrite(sidB, blob); err != nil {
		t.Fatal(err)
	}

	maxPlain := int(core.paths[0].m) - wire.HeaderBytes - wire.TagBytes
	served := map[uint64]uint64{}
	// A fills a packet within one quantum, so under any fair discipline B
	// must appear within a small bounded number of packets — sequential
	// slot order would leave B at zero until A's 32 KiB drains.
	bSeen := -1
	for i := 0; i < 64; i++ {
		core.txBuildID++
		fb := wire.NewFrameBuilder(make([]byte, 0, wire.BaseUDPPayload))
		var tb txBudget
		c.emitStreamData(fb, &tb, maxPlain)
		for _, li := range tb.items {
			if li.kind == itemStream {
				served[li.sid] += li.hi - li.lo
				if li.sid == uint64(sidB) && bSeen < 0 {
					bSeen = i
				}
			}
		}
	}
	a, b := served[uint64(sidA)], served[uint64(sidB)]
	if a == 0 || b == 0 {
		t.Fatalf("stream starved: A=%d B=%d bytes", a, b)
	}
	if bSeen < 0 || bSeen > 3 {
		t.Fatalf("stream B first served at packet %d, want <= 3", bSeen)
	}
	// Both carried 32 KiB of payload: neither may finish long before the
	// other starts. A round-robin service bound keeps them in step.
	if a > 4*b || b > 4*a {
		t.Fatalf("unfair service: A=%d B=%d bytes", a, b)
	}
}

// TestReassemblySparseBound: adversarial one-byte fragments must not grow
// the retained run table without bound (section 12.3 bounded reassembly);
// merged holes still deliver correct bytes once filled.
func TestReassemblySparseBound(t *testing.T) {
	r := &recvHalf{state: recvOpen, limit: 1 << 20}
	if !r.insert(0, []byte("base"), nil) {
		t.Fatal("seed insert rejected")
	}
	var buf [4]byte
	if n := r.read(buf[:]); n != 4 || string(buf[:]) != "base" {
		t.Fatalf("seed read = %d %q", n, buf[:])
	}

	const total = 2 * maxRxSegs
	for i := 1; i <= total; i++ {
		if !r.insert(uint64(4+4*i), []byte{0xAB}, nil) {
			t.Fatalf("sparse insert %d rejected", i)
		}
	}
	if len(r.segs) > maxRxSegs {
		t.Fatalf("run table grew to %d > %d", len(r.segs), maxRxSegs)
	}
	if r.pending != total {
		t.Fatalf("pending = %d, want %d", r.pending, total)
	}
	if r.readable() {
		t.Fatal("hole-covered runs reported readable")
	}
	// A conflicting byte at a retained offset is still a data error.
	if r.insert(8, []byte{0xCD}, nil) {
		t.Fatal("conflicting retransmit accepted")
	}
	// Cover every gap; island positions must match their retained byte.
	cover := make([]byte, 4*total+1)
	for i := 1; i <= total; i++ {
		cover[4*i] = 0xAB
	}
	if !r.insert(4, cover, nil) {
		t.Fatal("covering insert rejected")
	}
	if len(r.segs) != 1 {
		t.Fatalf("covering insert left %d runs, want 1", len(r.segs))
	}
	r.high = 5 + 4*total
	got := make([]byte, 4*total+1)
	if n := r.read(got); n != len(got) {
		t.Fatalf("read %d of %d", n, len(got))
	}
	for i := 1; i <= total; i++ {
		if got[4*i] != 0xAB {
			t.Fatalf("island byte %d = %#x", i, got[4*i])
		}
	}
	if r.pending != 0 || !r.allConsumed() {
		t.Fatalf("pending=%d consumed=%v after drain", r.pending,
			r.allConsumed())
	}
}

// TestMatchPathWildcardPin: a path whose local endpoint is still
// unspecified (wildcard-bound socket) matches an authenticated datagram
// arriving on the bound port and pins the observed destination; a later
// datagram on a different local address names a different path (section
// 17.1).
func TestMatchPathWildcardPin(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("0.0.0.0:9999")}
	c := unitConn(t, sock)
	core := c.core
	if !core.paths[0].local.Addr().IsUnspecified() {
		t.Fatalf("expected wildcard local, got %v", core.paths[0].local)
	}
	remote := core.paths[0].remote
	dst := netip.MustParseAddrPort("192.0.2.8:9999")
	if pi := c.matchPath(remote, dst); pi != 0 {
		t.Fatalf("wildcard path did not match inbound tuple: %d", pi)
	}
	if core.paths[0].local != dst {
		t.Fatalf("local not pinned: %v", core.paths[0].local)
	}
	if pi := c.matchPath(remote, dst); pi != 0 {
		t.Fatal("pinned tuple no longer matches")
	}
	other := netip.MustParseAddrPort("192.0.2.9:9999")
	if pi := c.matchPath(remote, other); pi != -1 {
		t.Fatalf("changed local tuple matched path %d", pi)
	}
}

// TestMigrateIntentWildcardLocal: an elective-migration candidate on a
// wildcard-bound socket pins its local endpoint from the first
// authenticated response, so the intent validation completes and the
// path promotes (section 17.5).
func TestMigrateIntentWildcardLocal(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("0.0.0.0:9999")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleClient
	newRemote := netip.MustParseAddrPort("10.0.0.2:4433")
	if err := c.appMigrate(&appCmd{migrateTo: newRemote}); err != nil {
		t.Fatalf("appMigrate: %v", err)
	}
	pi := core.localIntentPath
	if pi < 0 {
		t.Fatal("no intent candidate")
	}
	p := &core.paths[pi]
	if !p.local.Addr().IsUnspecified() {
		t.Fatalf("intent local not wildcard: %v", p.local)
	}
	dst := netip.MustParseAddrPort("192.0.2.8:9999")
	if got := c.matchPath(newRemote, dst); got != pi {
		t.Fatalf("response tuple matched path %d, want %d", got, pi)
	}
	if p.local != dst {
		t.Fatalf("intent local not pinned: %v", p.local)
	}
	p.challengeBytes = wire.BaseUDPPayload
	c.applyPathResponse(p.challenge, pi, wire.BaseUDPPayload)
	if core.activePth != pi || p.pstate != psActive {
		t.Fatalf("candidate not promoted: active=%d state=%v",
			core.activePth, p.pstate)
	}
}

// TestDupClientFlightAmpGate: the duplicate-flight reemission is bound
// to the client's authenticated tuple and charged against the
// anti-amplification budget while the path is unvalidated (section
// 7.1); a flight larger than 3x received bytes does not emit.
func TestDupClientFlightAmpGate(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer
	core.state = stSFinishing
	var attempt [wire.AttemptIDBytes]byte
	rand.Read(attempt[:])
	msg := make([]byte, 200)
	rand.Read(msg)
	frags := wire.BuildFragments(msg, wire.ClassClientFlight, 99, attempt,
		[wire.TokenBytes]byte{})
	pkt := frags[0]
	f0, _ := wire.ParseFragment(pkt[wire.HeaderBytes:])
	core.hs.attemptID = attempt
	core.hs.flightHash = f0.MessageHash
	legit := netip.MustParseAddrPort("10.0.0.7:7777")
	core.paths[0].remote = legit
	core.paths[0].validated = false
	core.paths[0].rxBytes = 0
	core.paths[0].rxPkts = 0
	core.paths[0].txBytes = 0
	core.paths[0].txPkts = 0
	h, _ := wire.ParseHeader(pkt)
	e := &connEvent{kind: evRX, pkt: pkt, src: legit,
		dst: sock.LocalAddr(), rxUS: nowUS()}
	big := make([]byte, 1200)
	core.hs.serverFlight = [][]byte{big, big, big, big}
	// The 1200-byte duplicate datagram credits 3*1200 bytes; with most
	// of that budget already spent the flight cannot emit.
	core.paths[0].txBytes = 3300
	c.rxClientFlightDup(e, h)
	c.txServerFlight(nowUS())
	if len(sock.sent) != 0 {
		t.Fatalf("reemission bypassed amplification budget: %d packets",
			len(sock.sent))
	}
	// The second duplicate credits another datagram: the 3x budget is
	// now 7200 bytes (and 6 packets), so six of eight 1200-byte
	// fragments emit and the gate stops the flight mid-way. Emission is
	// paced (section 6.4): the burst serves two datagrams immediately
	// and each further fragment after its paced interval.
	core.hs.serverFlight = [][]byte{big, big, big, big, big, big, big, big}
	core.paths[0].txBytes = 0
	core.hs.dupNextUS = 0
	c.rxClientFlightDup(e, h)
	c.txServerFlight(nowUS()) // burst: fragments 0-1
	if len(sock.sent) != 2 {
		t.Fatalf("paced burst emitted %d packets, want 2", len(sock.sent))
	}
	for i := 0; i < 6; i++ {
		core.hs.flight.nextUS = 0 // simulate the paced interval elapsing
		c.txServerFlight(nowUS())
	}
	if len(sock.sent) != 6 {
		t.Fatalf("credited reemission emitted %d packets, want 6",
			len(sock.sent))
	}
	if core.paths[0].txBytes != 7200 {
		t.Fatalf("reemitted bytes charged %d, want 7200",
			core.paths[0].txBytes)
	}
	// A datagram from a different source never triggers reemission.
	core.hs.dupNextUS = 0
	sent := len(sock.sent)
	e.src = netip.MustParseAddrPort("10.0.0.66:7777")
	c.rxClientFlightDup(e, h)
	if len(sock.sent) != sent {
		t.Fatal("spoofed source triggered reemission")
	}
}

// TestTicketPolicyAndDomain: issued tickets carry the configured
// authentication-policy digest and replay domain in both the sealed
// plaintext and the public metadata, and checkTicket rejects tickets
// minted under a different policy or domain (section 9.1).
func TestTicketPolicyAndDomain(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer
	core.cipher = wire.CipherXChaCha20Poly1305
	rand.Read(core.t1[:])
	rand.Read(core.cfg.TicketKey[:])
	core.cfg.TicketKeyID = 7
	var issuerCtr uint64
	core.cfg.TicketIssuer = func() ([16]byte, uint64) {
		var p [16]byte
		rand.Read(p[:])
		issuerCtr++
		return p, issuerCtr
	}
	core.cfg.PolicyHash = sha256.Sum256([]byte("policy-v1"))
	core.cfg.ReplayDomain = [16]byte{0x52, 0x45, 0x50}
	core.resumptionMaster = [32]byte{0xAA}

	c.issueTicket()
	var body []byte
	for i := range core.ctrl {
		if it := &core.ctrl[i]; it.used && it.kind == ctrlNewTicket {
			body = it.body
		}
	}
	if len(body) != wire.TicketMetaBytes+wire.TicketBytes {
		t.Fatalf("no NEW_TICKET queued (body %d bytes)", len(body))
	}
	meta := gc.DecodeTicketMeta(body[:wire.TicketMetaBytes])
	tp, err := c.openTicket(body[wire.TicketMetaBytes:])
	if err != nil {
		t.Fatal(err)
	}
	if tp.PolicyHash != core.cfg.PolicyHash {
		t.Fatal("ticket plaintext missing policy hash")
	}
	if tp.ReplayDomain != core.cfg.ReplayDomain {
		t.Fatal("ticket plaintext missing replay domain")
	}
	if meta.ReplayDomain != core.cfg.ReplayDomain {
		t.Fatal("ticket metadata missing replay domain")
	}

	core.hs.cliMsg = []byte("client flight transcript")
	ch0End := len(core.hs.cliMsg)
	noBinder := len(core.hs.cliMsg)
	cm := &wire.ClientMessage{
		Ticket:         body[wire.TicketMetaBytes:],
		TicketMetadata: [wire.TicketMetaBytes]byte(body[:wire.TicketMetaBytes]),
	}
	ek := gc.DeriveEarly(tp.PSK, core.hs.cliMsg[:ch0End])
	cm.Binder = gc.EarlyBinder(&ek, core.hs.cliMsg[:noBinder])
	var psk [32]byte
	if !c.checkTicket(cm, ch0End, noBinder, &psk) {
		t.Fatal("ticket rejected under its own policy/domain")
	}
	if psk != tp.PSK {
		t.Fatal("checkTicket did not return the ticket PSK")
	}
	oldPolicy := core.cfg.PolicyHash
	core.cfg.PolicyHash = sha256.Sum256([]byte("policy-v2"))
	if c.checkTicket(cm, ch0End, noBinder, &psk) {
		t.Fatal("ticket accepted after policy rotation")
	}
	core.cfg.PolicyHash = oldPolicy
	core.cfg.ReplayDomain = [16]byte{0x99}
	if c.checkTicket(cm, ch0End, noBinder, &psk) {
		t.Fatal("ticket accepted under a foreign replay domain")
	}
}

// feedCryptoMsg reassembles msg through applyCryptoFrag as kind under
// refresh transaction id.
func feedCryptoMsg(c *Conn, id [16]byte, kind uint8, msg []byte) {
	hash := sha256.Sum256(msg)
	nfrag := (len(msg) + wire.CryptoFragChunk - 1) / wire.CryptoFragChunk
	for i := 0; i < nfrag; i++ {
		lo := i * wire.CryptoFragChunk
		hi := min(lo+wire.CryptoFragChunk, len(msg))
		c.applyCryptoFrag(&wire.CryptoFrag{
			RefreshID:   id,
			Kind:        kind,
			TotalLength: uint32(len(msg)),
			MessageHash: hash,
			Index:       uint16(i),
			Count:       uint16(nfrag),
			Data:        msg[lo:hi],
		})
	}
}

// TestRefreshDupOfferReserved: a retransmitted OFFER that reassembles to
// the answered message is re-served the cached ANSWER; it must not
// re-derive or fault the connection (section 19.3).
func TestRefreshDupOfferReserved(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer
	core.cipher = wire.CipherXChaCha20Poly1305
	rt := &core.refresh
	rt.id = [16]byte{1, 2, 3}
	xC, err := gc.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	qC, err := gc.GenerateMLKEM768()
	if err != nil {
		t.Fatal(err)
	}
	offer := make([]byte, 1272)
	cur := core.keys.current()
	binary.BigEndian.PutUint32(offer[0:], cur.epoch+1)
	binary.BigEndian.PutUint16(offer[4:], core.cipher)
	copy(offer[8:], rt.id[:])
	rand.Read(offer[24:56])
	copy(offer[56:], xC.Pub[:])
	copy(offer[88:], qC.EK[:])

	feedCryptoMsg(c, rt.id, 1, offer)
	if rt.answer == nil {
		t.Fatal("OFFER not answered")
	}
	if core.state != stEstablished {
		t.Fatalf("conn died answering OFFER: %v", core.state)
	}
	for i := range core.ctrl {
		if it := &core.ctrl[i]; it.used && it.kind == ctrlCryptoFrag {
			it.used = false
		}
	}
	feedCryptoMsg(c, rt.id, 1, offer)
	if core.state != stEstablished {
		t.Fatalf("retransmitted OFFER killed the connection: %v", core.state)
	}
	owed := false
	for i := range core.ctrl {
		it := &core.ctrl[i]
		if it.used && it.kind == ctrlCryptoFrag && it.tok == rt.id &&
			uint8(it.b) == 2 && it.a != 0 {
			owed = true
		}
	}
	if !owed {
		t.Fatal("duplicate OFFER did not re-queue the ANSWER")
	}
	// A straggler OFFER arriving after the transaction finished must not
	// open a bogus reassembly (its epoch is now current).
	id := rt.id
	c.finishRefresh()
	feedCryptoMsg(c, id, 1, offer)
	if core.state != stEstablished {
		t.Fatalf("straggler OFFER after finish killed the connection: %v",
			core.state)
	}
	if rt.active || rt.rxHave != 0 {
		t.Fatal("straggler OFFER started a new reassembly")
	}
}

// TestRefreshDupAnswerIgnored: a retransmitted ANSWER reassembling after
// the staged keys were derived is ignored; the transaction continues
// (section 19.3).
func TestRefreshDupAnswerIgnored(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleClient
	rt := &core.refresh
	rt.active = true
	rt.id = [16]byte{9, 9}
	rt.commitSent = true // ANSWER already accepted: keys staged
	ans := make([]byte, 1208)
	rand.Read(ans)
	feedCryptoMsg(c, rt.id, 2, ans)
	if core.state != stEstablished {
		t.Fatalf("retransmitted ANSWER killed the connection: %v",
			core.state)
	}
}

// TestCtrlOnlyPacketAllowance: while application traffic is eligible a
// packet carrying no application frames may emit at most once per four
// emitted DATA packets unless it serves an excepted purpose (section
// 16).
func TestCtrlOnlyPacketAllowance(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleClient

	// Application traffic eligible but unemittable in this build: OPEN
	// already acknowledged, unsent bytes present, zero stream credit.
	core.slots.peerLimit[classIdx(false, wire.StreamInitiatorClient)] = 8
	sid, err := c.openStream(false)
	if err != nil {
		t.Fatal(err)
	}
	s := core.slots.slotOf(wire.StreamID(sid).Class(), wire.StreamID(sid).Slot())
	s.openAcked = true
	if _, err := c.appWrite(sid, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	s.send.limit = 0

	if !c.queueCtrl(ctrlPing, 0, 0, 0, 0, [16]byte{}) {
		t.Fatal("queueCtrl failed")
	}
	// Zero credit: the control-only build must be aborted and the work
	// restored, nothing emitted.
	if c.txDataPacket(nowUS()) {
		t.Fatal("control-only packet emitted with zero credit")
	}
	if len(sock.sent) != 0 {
		t.Fatalf("denied packet reached the socket: %d", len(sock.sent))
	}
	pingPending := false
	for i := range core.ctrl {
		if it := &core.ctrl[i]; it.used && it.kind == ctrlPing {
			pingPending = true
		}
	}
	if !pingPending {
		t.Fatal("denied PING was not restored to the control queue")
	}
	// Four emitted packets of credit admit one control-only packet;
	// emission then spends them.
	core.ctrlCredit = 4
	if !c.txDataPacket(nowUS()) {
		t.Fatal("credited control-only packet did not emit")
	}
	if len(sock.sent) != 1 {
		t.Fatalf("emitted %d packets, want 1", len(sock.sent))
	}
	if core.ctrlCredit != 1 {
		t.Fatalf("credit %d after emit, want 1", core.ctrlCredit)
	}
	// An excepted packet (handshake FINISH) emits even at zero credit.
	core.ctrlCredit = 0
	if !c.queueCtrl(ctrlFinish, 0, 0, 0, 0, [16]byte{}) {
		t.Fatal("queueCtrl FINISH failed")
	}
	if !c.txDataPacket(nowUS()) {
		t.Fatal("exempt FINISH packet did not emit")
	}
	if core.ctrlCredit != 1 {
		t.Fatalf("credit %d after exempt emit, want 1", core.ctrlCredit)
	}
}

// --- replay window slide (section 13.1) ---

// TestReplayWindowSlideClears: bits vacated by a sliding window must be
// cleared. A stale bit aliases the unseen in-window PN that reuses its
// position — reporting a never-received packet as committed forges an
// ACK, releases undelivered stream data, and drops the real packet as a
// duplicate.
func TestReplayWindowSlideClears(t *testing.T) {
	w := &replayWin{width: wire.MinReplayWindow}
	// Commit PN 4, then slide the window so PN 4 vacates it. The new
	// floor is 106; the vacated bit at position 4 must be cleared.
	w.commit(4)
	w.commit(4200)
	// PN 4100 reuses vacated PN 4's bit position (4100 = 4 + 4096) and
	// was never received.
	if w.seen(4100) {
		t.Fatal("stale vacated bit reports an unseen PN as committed")
	}
	if !w.fresh(4100) {
		t.Fatal("stale vacated bit rejects a fresh in-window PN")
	}
	// The bitmap still records the packet when it genuinely arrives.
	w.commit(4100)
	if !w.seen(4100) {
		t.Fatal("in-window commit not recorded")
	}
	if w.fresh(4100) {
		t.Fatal("committed PN still reports fresh")
	}
}

// TestReplayWindowFullWrap: sliding must clear exactly the vacated
// positions — still-in-window commits stay seen, never-received
// positions report fresh, and a slide of at least one width clears the
// whole bitmap.
func TestReplayWindowFullWrap(t *testing.T) {
	w := &replayWin{width: wire.MinReplayWindow}
	const W = wire.MinReplayWindow
	for pn := uint64(0); pn < W+1000; pn++ {
		w.commit(pn)
	}
	// Committed span is [0, W+999]; the window is [1000, W+999].
	for pn := uint64(1000); pn < W+1000; pn++ {
		if !w.seen(pn) {
			t.Fatalf("committed PN %d not seen", pn)
		}
	}
	// Slide one width: floor moves to 4097. Committed PNs 4097..5095
	// stay seen; everything newer was never received.
	w.commit(2 * W)
	for pn := uint64(4097); pn < W+1000; pn++ {
		if !w.seen(pn) {
			t.Fatalf("still-in-window committed PN %d lost its bit", pn)
		}
	}
	for pn := uint64(W + 1000); pn < 2*W; pn++ {
		if w.seen(pn) {
			t.Fatalf("unseen PN %d reports committed after wrap", pn)
		}
		if !w.fresh(pn) {
			t.Fatalf("fresh PN %d rejected after wrap", pn)
		}
	}
	if w.seen(4096) {
		t.Fatal("vacated PN below the new floor reports seen")
	}
	// Slide past the whole window: the bitmap is fully cleared.
	w.commit(3 * W)
	for pn := uint64(2*W + 1); pn < 3*W; pn++ {
		if w.seen(pn) {
			t.Fatalf("unseen PN %d reports committed after full clear", pn)
		}
		if !w.fresh(pn) {
			t.Fatalf("fresh PN %d rejected after full clear", pn)
		}
	}
	// Sequential multi-wrap commits still record every bit.
	for pn := uint64(3*W + 1); pn <= 3*W+50; pn++ {
		w.commit(pn)
	}
	for pn := uint64(3*W + 1); pn <= 3*W+50; pn++ {
		if !w.seen(pn) {
			t.Fatalf("committed PN %d not seen after multi-wrap", pn)
		}
	}
}

// TestReplayWindowAckRangesHonest: ACK ranges must cover only genuinely
// committed PNs — never positions held by vacated packets.
func TestReplayWindowAckRangesHonest(t *testing.T) {
	w := &replayWin{width: wire.MinReplayWindow}
	const W = wire.MinReplayWindow
	w.commit(0)
	w.commit(W + 10) // slide: window [11, W+10]
	w.commit(100)
	w.commit(200)
	var ranges [wire.MaxAckRanges][2]uint64
	n := w.ackRanges(&ranges, 8)
	if n != 3 {
		t.Fatalf("ackRanges returned %d ranges, want 3", n)
	}
	want := [][2]uint64{{W + 10, W + 10}, {200, 200}, {100, 100}}
	for i, r := range want {
		if ranges[i][0] != r[0] || ranges[i][1] != r[1] {
			t.Fatalf("range %d = %v, want %v", i, ranges[i], r)
		}
	}
	for i := 0; i < n; i++ {
		if ranges[i][0] <= 4 && 4 <= ranges[i][1] {
			t.Fatal("ACK range covers a vacated stale position")
		}
	}
}

// TestReplayWindowNegotiatedWidth: the bitmap honors a non-minimum
// negotiated receive_packet_history width; positions alias at the
// negotiated width, not the minimum.
func TestReplayWindowNegotiatedWidth(t *testing.T) {
	const W = 8192
	w := &replayWin{width: W}
	w.commit(4)
	w.commit(4 + W + 1) // slide: floor=6, vacates PN 4
	// PN 4+W reuses vacated PN 4's position (aliasing at the negotiated
	// width) and was never received.
	if w.seen(4 + W) {
		t.Fatal("stale bit in negotiated-width window reports seen")
	}
	if !w.fresh(4 + W) {
		t.Fatal("fresh in-window PN rejected in negotiated-width window")
	}
	// A smaller width would have aliased 4+4096 instead: that position
	// must be untouched here.
	if w.seen(4 + 4096) {
		t.Fatal("position outside negotiated aliasing reports seen")
	}
}

// TestReplayWindowBelowFloorCommitIgnored: a commit below the floor is a
// contract violation; it must be ignored rather than pollute the
// position an in-window PN will reuse.
func TestReplayWindowBelowFloorCommitIgnored(t *testing.T) {
	w := &replayWin{width: wire.MinReplayWindow}
	w.commit(5000) // floor = 905
	w.commit(10)   // below floor: must be ignored
	if w.seen(10 + wire.MinReplayWindow) {
		t.Fatal("below-floor commit polluted an in-window position")
	}
}

// --- FINISH path validation (section 6) ---

// TestFinishForeignTupleNotValidated: a valid client FINISH establishes
// server-side address validation only on the admitted handshake path.
// Arriving on any other tuple it must not lift that candidate's
// anti-amplification cap — a client could otherwise un-cap a spoofed
// address without a PATH_CHALLENGE exchange.
func TestFinishForeignTupleNotValidated(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer
	core.state = stSFinishing
	rand.Read(core.finKey[:])
	rand.Read(core.t1[:])
	// paths[0] is the admitted handshake tuple (tokenless: unproven);
	// paths[1] is a peer-discovered candidate carrying the FINISH.
	core.paths[0].validated = false
	core.paths[1] = pathRec{
		role:   pathCandidate,
		remote: netip.MustParseAddrPort("10.9.9.9:9"),
		local:  sock.LocalAddr(),
		pstate: psValidating,
	}
	var tok [16]byte
	rand.Read(tok[:])
	mac := gc.ClientFinishMAC(core.finKey, core.t1, tok)
	if !c.applyFinish(mac, tok, 1) {
		t.Fatal("valid FINISH rejected")
	}
	if core.paths[1].validated {
		t.Fatal("FINISH on a foreign tuple validated the candidate")
	}
	if !c.applyFinish(mac, tok, 0) {
		t.Fatal("valid FINISH rejected")
	}
	if !core.paths[0].validated {
		t.Fatal("FINISH on the admitted handshake path did not validate it")
	}
}

// --- persistent congestion interval (section 15.2) ---

// TestPersistentCongestionOutstandingDisqualifies: the loss interval may
// only contain declared-lost ack-eliciting transmissions. A merely
// outstanding packet inside the span disqualifies it; once that packet
// is also declared lost the interval qualifies.
func TestPersistentCongestionOutstandingDisqualifies(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	p := &core.paths[0]
	mk := func(pn, ord, sent uint64) *sentPkt {
		sp := &sentPkt{pn: pn, ordinal: ord, sentUS: sent,
			eliciting: true, path: 0}
		core.ledger.track(sp)
		return sp
	}
	mk(1, 1, 0)
	mid := mk(3, 3, 500000)
	mk(5, 5, 1000000)
	c.declareLost(core.ledger.get(1))
	c.declareLost(core.ledger.get(5))
	// Ordinal 3 is merely outstanding inside [1,5] — the interval is
	// not yet an all-lost span.
	if c.persistentCongestion(0, p) {
		t.Fatal("outstanding packet inside span declared congestion")
	}
	// Declare it lost as well: every transmission inside is now lost.
	c.declareLost(mid)
	if !c.persistentCongestion(0, p) {
		t.Fatal("all-lost span over 3*base_pto not declared congestion")
	}
	// The interval was consumed; a lone later loss cannot redeclare it.
	c.declareLost(mk(7, 7, 2000000))
	if c.persistentCongestion(0, p) {
		t.Fatal("single loss redeclared a consumed congestion interval")
	}
}

// TestPersistentCongestionAckBreaksInterval: an acknowledged packet
// inside or past the run's start breaks the interval; an acknowledgment
// below the run leaves it intact.
func TestPersistentCongestionAckBreaksInterval(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	p := &core.paths[0]
	mk := func(pn, ord, sent uint64) *sentPkt {
		return &sentPkt{pn: pn, ordinal: ord, sentUS: sent,
			eliciting: true, path: 0}
	}
	l1 := mk(1, 1, 0)
	l5 := mk(5, 5, 1000000)
	c.pcRun(p, l1)
	// An ack below the run's start does not disturb it.
	c.pcBreak(p, mk(2, 0, 0))
	c.pcRun(p, l5)
	if p.pcN != 2 {
		t.Fatalf("ack below run broke it: pcN=%d", p.pcN)
	}
	if !c.persistentCongestion(0, p) {
		t.Fatal("intact all-lost span not declared congestion")
	}
	// Rebuild: an ack inside the interval breaks it.
	c.pcRun(p, l1)
	c.pcBreak(p, mk(3, 3, 100))
	c.pcRun(p, l5)
	if p.pcN != 1 {
		t.Fatalf("ack inside span did not restart run: pcN=%d", p.pcN)
	}
	if c.persistentCongestion(0, p) {
		t.Fatal("broken interval declared congestion")
	}
}

// mkStreamFrame builds a wire.Frame carrying STREAM data (16-byte head +
// payload); fin sets the frame's FIN flag.
func mkStreamFrame(sid wire.StreamID, off uint64, data []byte, fin bool) wire.Frame {
	body := make([]byte, 16+len(data))
	wire.BuildStreamHead(body, uint64(sid), off)
	copy(body[16:], data)
	var flags uint16
	if fin {
		flags = wire.FrameFlagStreamFin
	}
	return wire.Frame{Type: wire.FrameStream, Flags: flags, Body: body}
}

// openRecvStream commits an OPEN for a peer-initiated bidirectional stream
// and returns the sid; the recv half then sits active at high-water zero.
func openRecvStream(t *testing.T, c *Conn, slot uint16) wire.StreamID {
	t.Helper()
	core := c.core
	core.role = roleServer
	class := classIdx(false, wire.StreamInitiatorClient)
	if core.slots.localLimit[class] <= uint32(slot) {
		core.slots.localLimit[class] = uint32(slot) + 1
	}
	sid := wire.MakeStreamID(0, slot, false, wire.StreamInitiatorClient)
	var pf rxPreflight
	if code := c.preflightOpen(sid, &pf); code != codeNoError {
		t.Fatalf("OPEN = %v", code)
	}
	c.applyOpen(sid)
	return sid
}

// TestSamePacketFlowCharge: multiple STREAM/RESET frames on one stream in a
// single packet must charge only the unique high-water increase against
// connection flow control (section 12.4). The transaction's tentative
// per-stream high-water follows frame order, so contiguous, overlapping,
// and retransmitted same-packet ranges are never double-charged.
func TestSamePacketFlowCharge(t *testing.T) {
	mk := func(t *testing.T) (*Conn, wire.StreamID) {
		sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
		c := unitConn(t, sock)
		return c, openRecvStream(t, c, 0)
	}

	t.Run("contiguous", func(t *testing.T) {
		c, sid := mk(t)
		var pf rxPreflight
		if code := c.preflightStreamData(sid, 0, 100, false, &pf); code != codeNoError {
			t.Fatalf("first STREAM = %v", code)
		}
		if code := c.preflightStreamData(sid, 100, 100, false, &pf); code != codeNoError {
			t.Fatalf("second STREAM = %v", code)
		}
		if pf.connInc != 200 {
			t.Fatalf("connInc = %d, want 200 (unique bytes only)", pf.connInc)
		}
	})

	t.Run("overlap", func(t *testing.T) {
		c, sid := mk(t)
		var pf rxPreflight
		_ = c.preflightStreamData(sid, 0, 100, false, &pf)
		_ = c.preflightStreamData(sid, 50, 100, false, &pf)
		if pf.connInc != 150 {
			t.Fatalf("connInc = %d, want 150", pf.connInc)
		}
	})

	t.Run("reversed", func(t *testing.T) {
		c, sid := mk(t)
		var pf rxPreflight
		_ = c.preflightStreamData(sid, 100, 100, false, &pf)
		_ = c.preflightStreamData(sid, 0, 100, false, &pf)
		if pf.connInc != 200 {
			t.Fatalf("connInc = %d, want 200", pf.connInc)
		}
	})

	t.Run("streamThenReset", func(t *testing.T) {
		c, sid := mk(t)
		var pf rxPreflight
		_ = c.preflightStreamData(sid, 0, 100, false, &pf)
		if code := c.preflightReset(sid, 200, &pf); code != codeNoError {
			t.Fatalf("RESET = %v", code)
		}
		if pf.connInc != 200 {
			t.Fatalf("connInc = %d, want 200", pf.connInc)
		}
	})

	t.Run("resetThenReset", func(t *testing.T) {
		c, sid := mk(t)
		var pf rxPreflight
		if code := c.preflightReset(sid, 200, &pf); code != codeNoError {
			t.Fatalf("first RESET = %v", code)
		}
		if code := c.preflightReset(sid, 200, &pf); code != codeNoError {
			t.Fatalf("repeat RESET = %v", code)
		}
		if pf.connInc != 200 {
			t.Fatalf("connInc = %d, want 200", pf.connInc)
		}
	})

	// A RESET final below same-packet staged bytes is FINAL_SIZE_ERROR:
	// the declared final contradicts bytes already admitted in-frame-order.
	t.Run("resetBelowStaged", func(t *testing.T) {
		c, sid := mk(t)
		var pf rxPreflight
		_ = c.preflightStreamData(sid, 0, 300, false, &pf)
		if code := c.preflightReset(sid, 200, &pf); code != codeFinalSize {
			t.Fatalf("RESET = %v, want FINAL_SIZE_ERROR", code)
		}
	})

	// Data beyond a same-packet RESET's declared final is FINAL_SIZE_ERROR.
	t.Run("streamBeyondReset", func(t *testing.T) {
		c, sid := mk(t)
		var pf rxPreflight
		if code := c.preflightReset(sid, 200, &pf); code != codeNoError {
			t.Fatalf("RESET = %v", code)
		}
		if code := c.preflightStreamData(sid, 200, 100, false, &pf); code != codeFinalSize {
			t.Fatalf("STREAM beyond final = %v, want FINAL_SIZE_ERROR", code)
		}
	})

	// A FIN staged in-packet pins the final: a later RESET must agree.
	t.Run("finThenReset", func(t *testing.T) {
		c, sid := mk(t)
		var pf rxPreflight
		if code := c.preflightStreamData(sid, 0, 200, true, &pf); code != codeNoError {
			t.Fatalf("FIN STREAM = %v", code)
		}
		if code := c.preflightReset(sid, 300, &pf); code != codeFinalSize {
			t.Fatalf("RESET != fin = %v, want FINAL_SIZE_ERROR", code)
		}
		if pf.connInc != 200 {
			t.Fatalf("connInc = %d, want 200", pf.connInc)
		}
	})

	// A FIN declaring a final below same-packet staged bytes is
	// FINAL_SIZE_ERROR.
	t.Run("finBelowStaged", func(t *testing.T) {
		c, sid := mk(t)
		var pf rxPreflight
		_ = c.preflightStreamData(sid, 0, 300, false, &pf)
		if code := c.preflightStreamData(sid, 0, 200, true, &pf); code != codeFinalSize {
			t.Fatalf("FIN = %v, want FINAL_SIZE_ERROR", code)
		}
	})
}

// TestSamePacketFlowAdmission: the section-12.4 admission gate must charge
// the packet's unique high-water increase — a packet carrying two frames
// totalling 200 unique bytes fits a 250-byte connection window, while a
// packet genuinely exceeding the window is still refused.
func TestSamePacketFlowAdmission(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	sid := openRecvStream(t, c, 0)
	core := c.core
	core.connRxLim = 250
	core.connRxUsed = 0

	frames := []wire.Frame{
		mkStreamFrame(sid, 0, make([]byte, 100), false),
		mkStreamFrame(sid, 100, make([]byte, 100), false),
	}
	var pf rxPreflight
	if code := c.preflight(frames, &pf, true); code != codeNoError {
		t.Fatalf("preflight = %v, want admitted (unique charge 200 <= 250)", code)
	}
	if pf.connInc != 200 {
		t.Fatalf("connInc = %d, want 200", pf.connInc)
	}

	// The same packet past the real limit stays FLOW_CONTROL_ERROR.
	core.connRxLim = 150
	pf = rxPreflight{}
	if code := c.preflight(frames, &pf, true); code != codeFlowControl {
		t.Fatalf("preflight = %v, want FLOW_CONTROL_ERROR", code)
	}
}

// TestCandidateGateHandshake: migration is not processed before handshake
// confirmation (section 6). A committed packet from an unmatched remote
// tuple may only seed a candidate once the connection is established —
// during handshake finishing states the evidence is dropped instead.
func TestCandidateGateHandshake(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	e := &connEvent{src: netip.MustParseAddrPort("10.9.9.9:9"),
		dst: netip.MustParseAddrPort("10.0.0.9:9")}

	for _, st := range []connState{stCFinishing, stSFinishing, stSCollect, stSBuild} {
		core.state = st
		if c.createCandidate(e) {
			t.Fatalf("candidate created in state %v before confirmation", st)
		}
		for i := range core.paths {
			if core.paths[i].role == pathCandidate {
				t.Fatalf("candidate slot retained in state %v", st)
			}
		}
	}
	core.state = stEstablished
	if !c.createCandidate(e) {
		t.Fatal("candidate refused after handshake confirmation")
	}
	if core.paths[1].role != pathCandidate || !core.paths[1].validating {
		t.Fatal("candidate not validating after confirmation")
	}
}

// TestBodyCtrlBound: body-carrying control items (CRYPTO_FRAGMENT,
// NEW_TICKET, NEW_TOKEN) respect the same MaxPendingCtrl admission bound
// as scalar obligations, and every admitted item owns a fresh allocation
// generation so a recycled slot can never resolve a stale loss entry.
func TestBodyCtrlBound(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	c.core.cfg.MaxPendingCtrl = 2
	if !c.queueFrags([16]byte{1}, 1, []byte("offer")) {
		t.Fatal("first fragment item refused")
	}
	if !c.queueCtrlBody(ctrlNewTicket, []byte("ticket")) {
		t.Fatal("second body item refused")
	}
	if c.queueCtrlBody(ctrlNewToken, []byte("token")) {
		t.Fatal("third body item admitted past MaxPendingCtrl")
	}
	if c.queueFrags([16]byte{2}, 2, []byte("answer")) {
		t.Fatal("third fragment item admitted past MaxPendingCtrl")
	}
	seen := make(map[uint32]bool)
	for i := range c.core.ctrl {
		it := &c.core.ctrl[i]
		if !it.used {
			continue
		}
		if it.agen == 0 || seen[it.agen] {
			t.Fatalf("item %d has duplicate/zero agen %d", i, it.agen)
		}
		seen[it.agen] = true
	}
}

// TestCwndGrowthEligibility: section 15.1 growth applies only to newly
// acknowledged in-flight bytes emitted after the recovery boundary while
// the sender was neither application- nor flow-control-limited.
func TestCwndGrowthEligibility(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	p := &core.paths[0]
	p.cwnd, p.ssthresh, p.m = 12000, 1<<20, 1200
	p.recBoundary = 0
	ack := func(sp *sentPkt) {
		t.Helper()
		p.inFlight = uint64(sp.bytes)
		core.ledger.reserve(sp.pn)
		core.ledger.track(sp)
		c.ackOne(sp, nowUS())
	}

	// A packet sent while limited cannot grow cwnd.
	sp := &sentPkt{pn: 1, bytes: 1000, path: 0, ordinal: 1,
		eliciting: true, inFlight: true, limited: true}
	ack(sp)
	if p.cwnd != 12000 {
		t.Fatalf("limited ack grew cwnd to %d", p.cwnd)
	}

	// An eligible packet grows it in slow start.
	sp = &sentPkt{pn: 2, bytes: 1000, path: 0, ordinal: 2,
		eliciting: true, inFlight: true}
	ack(sp)
	if p.cwnd != 13000 {
		t.Fatalf("eligible ack left cwnd at %d, want 13000", p.cwnd)
	}

	// A non-eliciting packet never grows it.
	sp = &sentPkt{pn: 3, bytes: 1000, path: 0, ordinal: 3,
		eliciting: false, inFlight: true}
	ack(sp)
	if p.cwnd != 13000 {
		t.Fatalf("non-eliciting ack grew cwnd to %d", p.cwnd)
	}

	// A packet emitted before the recovery boundary is excluded.
	p.recBoundary = 10
	sp = &sentPkt{pn: 4, bytes: 1000, path: 0, ordinal: 4,
		eliciting: true, inFlight: true}
	ack(sp)
	if p.cwnd != 13000 {
		t.Fatalf("pre-boundary ack grew cwnd to %d", p.cwnd)
	}
}

// TestReadAfterResetPersistent: RESET makes the receive half terminal;
// every subsequent consume reports the same error instead of waiting on
// a delivery that can never come (section 12.6/21.1 terminal results
// are persistent, not one-shot notifications).
func TestReadAfterResetPersistent(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	sid := wire.MakeStreamID(0, 0, false, wire.StreamInitiatorServer)
	s := core.slots.ensureSlot(sid.Class(), sid.Slot())
	s.state = slotActive
	s.gen = 0
	s.initiator = sid.Initiator()
	s.recv = &recvHalf{state: recvOpen, limit: 1 << 20}
	c.applyReset(sid, 0, 42)
	var buf [8]byte
	for i := 0; i < 3; i++ {
		if n, err := c.appConsume(uint64(sid), buf[:]); n != 0 || err != errStreamReset {
			t.Fatalf("post-reset appConsume %d = (%d, %v), want (0, reset)", i, n, err)
		}
	}
}

// TestReadTerminalStaleHandle: after the retired generation's receive
// half is released, a handle naming that generation still reads the
// recorded EOF terminal through the slot tombstone (section 21.1). A
// fresh generation on the slot clears it, so its own opens are clean.
func TestReadTerminalStaleHandle(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	sid := wire.MakeStreamID(0, 0, false, wire.StreamInitiatorServer)
	s := core.slots.ensureSlot(sid.Class(), sid.Slot())
	s.state = slotActive
	s.gen = 0
	s.initiator = sid.Initiator()
	s.recv = &recvHalf{state: recvOpen, limit: 1 << 20}
	c.applyStreamData(sid, 0, []byte("x"), true, nil)
	var buf [8]byte
	if n, err := c.appConsume(uint64(sid), buf[:]); n != 1 || err != errEOF {
		t.Fatalf("first consume = (%d, %v), want (1, EOF)", n, err)
	}
	// Retirement frees the half; the tombstone keeps the result.
	s.recv = nil
	if _, err := c.appConsume(uint64(sid), buf[:]); err != errEOF {
		t.Fatalf("stale-handle consume = %v, want EOF", err)
	}
	// Peer retires the slot; both halves are already done, so the slot
	// moves to RETIRED_WAIT_OPEN and a new generation can attach.
	c.applyRetire(sid)
	sid2 := wire.MakeStreamID(1, 0, false, wire.StreamInitiatorServer)
	c.applyOpen(sid2)
	if s.state != slotActive || s.gen != 1 || s.readTerm != termNone {
		t.Fatalf("gen-1 open: state=%v gen=%d term=%d", s.state, s.gen, s.readTerm)
	}
	if _, err := c.appConsume(uint64(sid), buf[:]); err != errClosed {
		t.Fatalf("gen-0 handle after gen-1 open = %v, want closed", err)
	}
	if n, err := c.appConsume(uint64(sid2), buf[:]); n != 0 || err != nil {
		t.Fatalf("gen-1 consume = (%d, %v), want (0, nil)", n, err)
	}
}

// TestClientFlightPacing: the bootstrap emitter releases at most a
// two-datagram burst immediately, then paces each further fragment at
// 12000/INITIAL_RTT_US bytes/us (section 6.4). A completed round waits
// the retransmission interval, which doubles for the next round.
func TestClientFlightPacing(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleClient
	core.state = stCWaitReply
	frags := [][]byte{make([]byte, 1200), make([]byte, 1200),
		make([]byte, 1200), make([]byte, 1200)}
	core.hs.flight = flightTx{frags: frags, burst: flightBurst,
		roundUS: nowUS(), rtxUS: bootstrapRtxUS}
	now := nowUS()
	c.txClientFlight(now)
	if len(sock.sent) != 2 {
		t.Fatalf("initial burst emitted %d datagrams, want 2", len(sock.sent))
	}
	c.txClientFlight(now)
	if len(sock.sent) != 2 {
		t.Fatal("third fragment emitted before its pace slot")
	}
	core.hs.flight.nextUS = 0 // pace slot reached
	c.txClientFlight(now)
	core.hs.flight.nextUS = 0
	c.txClientFlight(now)
	if len(sock.sent) != 4 {
		t.Fatalf("paced emission reached %d datagrams, want 4", len(sock.sent))
	}
	c.txClientFlight(now)
	if len(sock.sent) != 4 {
		t.Fatal("retransmission fired before its interval")
	}
	if !core.armed[tkFlight] {
		t.Fatal("flight timer not armed for retransmission")
	}
	// The retransmission round repeats the flight as a fresh burst and
	// doubles the interval (section 6.4).
	core.hs.flight.nextUS = 0
	c.txClientFlight(now)
	if len(sock.sent) != 6 {
		t.Fatalf("RTX round total = %d datagrams, want 6", len(sock.sent))
	}
	if core.hs.flight.rtxUS != 2*bootstrapRtxUS {
		t.Fatalf("RTX interval = %d, want doubled %d",
			core.hs.flight.rtxUS, 2*bootstrapRtxUS)
	}
}

// TestServerFlightPacing: server-flight emission (initial and
// duplicate-triggered) obeys the same section-6.4 burst/pacing envelope.
func TestServerFlightPacing(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleServer
	core.state = stSFinishing
	frags := [][]byte{make([]byte, 1200), make([]byte, 1200),
		make([]byte, 1200), make([]byte, 1200)}
	core.hs.serverFlight = frags
	core.hs.flight = flightTx{frags: frags, burst: flightBurst, roundUS: nowUS()}
	now := nowUS()
	c.txServerFlight(now)
	if len(sock.sent) != 2 {
		t.Fatalf("server burst emitted %d, want 2", len(sock.sent))
	}
	c.txServerFlight(now)
	if len(sock.sent) != 2 {
		t.Fatal("server fragment emitted before its pace slot")
	}
	core.hs.flight.nextUS = 0
	c.txServerFlight(now)
	core.hs.flight.nextUS = 0
	c.txServerFlight(now)
	if len(sock.sent) != 4 {
		t.Fatalf("server paced total = %d, want 4", len(sock.sent))
	}
}

// buildClientAttempt fabricates a syntactically valid client flight for
// scfg (real keypairs and signatures) and returns the message plus the
// private material so tests can finish the server-side PQXDH.
func buildClientAttempt(t *testing.T, scfg *Config, attemptID [wire.AttemptIDBytes]byte, dcid uint64) ([]byte, gc.X25519KeyPair, gc.X25519KeyPair, gc.MLKEM768KeyPair) {
	t.Helper()
	ik, err := gc.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	spk, err := gc.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	pqpk, err := gc.GenerateMLKEM768()
	if err != nil {
		t.Fatal(err)
	}
	var m wire.ClientMessage
	m.Offer = [wire.MaxCipherOffers]uint16{wire.CipherXChaCha20Poly1305, wire.CipherAES256GCM}
	m.AttemptID = attemptID
	if _, err := rand.Read(m.ClientNonce[:]); err != nil {
		t.Fatal(err)
	}
	m.ClientInitialCID = 0xC1
	m.OriginalDCID = dcid
	m.AppID = scfg.AppID
	m.Authority = scfg.ServerName
	if scfg.ServerCert != nil {
		m.ExpectedServerKey = scfg.ServerCert.IdentityPub
	} else {
		m.ExpectedServerKey = scfg.ExpectedKey
	}
	m.IKC = ik.Pub
	m.SPKC = spk.Pub
	var z [64]byte
	rand.Read(z[:])
	enc := gc.EncodeEC(spk.Pub)
	if m.SPKSignature, err = gc.XEdDSASign(ik.Priv[:], enc[:], z[:]); err != nil {
		t.Fatal(err)
	}
	rand.Read(z[:])
	enc2 := gc.EncodeKEM(pqpk.EK)
	if m.PQPKSignature, err = gc.XEdDSASign(ik.Priv[:], enc2[:], z[:]); err != nil {
		t.Fatal(err)
	}
	copy(m.PQPKC[:], pqpk.EK[:])
	wire.EncodeParams(m.Params[:], &scfg.Params)
	msg, err := wire.EncodeClientMessage(&m)
	if err != nil {
		t.Fatal(err)
	}
	return msg, ik, spk, pqpk
}

// TestAttemptPoolBound: the shared attempt pool is a hard admission
// bound (sections 20.1/37.6): a complete flight beyond the bound is
// refused at admission, and a released slot re-opens admission.
func TestAttemptPoolBound(t *testing.T) {
	_, scfg := testConfigs(t)
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.1:4433")}
	l := NewListener(scfg, sock, []byte("test-domain"))
	l.attemptsBound = 1
	src := netip.MustParseAddrPort("10.0.0.2:5000")

	// Occupy the single slot with a held attempt (no goroutine — the
	// release is exercised directly).
	var attemptA [wire.AttemptIDBytes]byte
	rand.Read(attemptA[:])
	hold := newConn(l.cfg, &routeSocket{ps: sock, local: sock.addr}, 1)
	hold.core.listener = l
	hold.core.attemptHeld = true
	hold.core.hs.attemptID = attemptA
	l.mu.Lock()
	l.attempts[attemptA] = hold
	l.attemptsInFlight = 1
	l.mu.Unlock()

	// A complete second flight must be refused while the pool is full.
	var attemptB [wire.AttemptIDBytes]byte
	rand.Read(attemptB[:])
	msg, _, _, _ := buildClientAttempt(t, scfg, attemptB, 0xDC1D)
	f := &wire.Fragment{AttemptID: attemptB, MessageHash: sha256.Sum256(msg)}
	slot := &reasmSlot{rxBytes: 1 << 20, rxPkts: 64}
	l.startServerAttempt(msg, f, slot, src, sock.addr)
	l.mu.Lock()
	full := l.attemptsInFlight
	_, admitted := l.attempts[attemptB]
	l.mu.Unlock()
	if full != 1 || admitted {
		t.Fatalf("pool admitted past bound: inFlight=%d admitted=%v", full, admitted)
	}

	// Releasing the slot re-opens admission; the new attempt holds it.
	hold.releaseAttempt()
	l.startServerAttempt(msg, f, slot, src, sock.addr)
	l.mu.Lock()
	inFlight := l.attemptsInFlight
	cn := l.attempts[attemptB]
	l.mu.Unlock()
	if inFlight != 1 || cn == nil {
		t.Fatalf("released slot did not admit attempt: inFlight=%d conn=%v", inFlight, cn)
	}
	// The live attempt reaches S_FINISHING and still holds its slot;
	// releasing it frees the pool again.
	cn.releaseAttempt()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attemptsInFlight != 0 {
		t.Fatalf("inFlight = %d after release, want 0", l.attemptsInFlight)
	}
}

// TestTokenlessPacketBound: a tokenless attempt is refused when the
// reply would exceed the 3x packet allowance even though the byte
// allowance is ample (section 6 dual bound).
func TestTokenlessPacketBound(t *testing.T) {
	_, scfg := testConfigs(t)
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.1:4433")}
	l := NewListener(scfg, sock, []byte("test-domain"))

	newAttempt := func(rxPkts uint64) *Conn {
		c := newConn(scfg, &routeSocket{ps: sock, local: sock.addr}, 1)
		core := c.core
		core.role = roleServer
		core.state = stSBuild
		var attempt [wire.AttemptIDBytes]byte
		rand.Read(attempt[:])
		msg, _, _, _ := buildClientAttempt(t, scfg, attempt, 0xBEEF)
		core.hs.cliMsg = msg
		core.hs.attemptID = attempt
		core.hs.flightHash = sha256.Sum256(msg)
		core.hs.retryDCID = 0xBEEF
		core.listener = l
		p := &core.paths[0]
		p.remote = netip.MustParseAddrPort("10.0.0.2:5000")
		p.local = sock.LocalAddr()
		p.role = pathActive
		p.rxBytes = 1 << 20
		p.rxPkts = rxPkts
		core.activePth = 0
		return c
	}

	// Learn the flight's packet count under ample credit, then bind the
	// packet budget just below it: 3*rxPkts < len(serverFlight) must
	// refuse the attempt even though bytes are unlimited.
	probe := newAttempt(64)
	probe.serverBuild()
	nf := len(probe.core.hs.serverFlight)
	if nf < 2 {
		t.Fatalf("server flight has only %d packets", nf)
	}
	c := newAttempt(uint64(nf-1) / 3)
	c.serverBuild()
	if c.core.state != stDead {
		t.Fatalf("tokenless attempt past packet bound reached %s", c.core.state)
	}
	foundRetry := false
	for _, d := range sock.sent {
		if h, ok := wire.ParseHeader(d); ok && h.Class == wire.ClassRetry {
			foundRetry = true
		}
	}
	if !foundRetry {
		t.Fatal("no RETRY emitted for refused tokenless attempt")
	}

	// Packet credit covering the whole flight lets the attempt proceed.
	sock.sent = nil
	c2 := newAttempt(uint64(nf+2) / 3)
	c2.serverBuild()
	if c2.core.state != stSFinishing {
		t.Fatalf("credited tokenless attempt reached %s, want S_FINISHING", c2.core.state)
	}
}

// TestAcceptedTokenHashChecked: the client must reject a server flight
// whose accepted_token_hash does not commit to the token it presented
// (section 8.3 rejection obligation), including the all-zero token hash
// on a tokenless attempt.
func TestAcceptedTokenHashChecked(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleClient
	core.state = stCWaitReply
	ccfg := &Config{
		Params:      wire.DefaultParams(),
		ServerName:  "srv.example:4433",
		AppID:       [16]byte{1},
		AuthMode:    0,
		NowUnix:     func() int64 { return time.Now().Unix() },
		MaxLiveSent: 4096,
		DisableAES:  true,
	}
	core.cfg = ccfg

	srv := sharedTestIdentity(t)
	ccfg.ExpectedKey = srv.IdentityPub
	var attemptID [wire.AttemptIDBytes]byte
	rand.Read(attemptID[:])
	msg, ik, spk, pqpk := buildClientAttempt(t, ccfg, attemptID, 0xDC1D)
	core.hs.clientMsg = msg
	core.hs.flightHash = wire.ClientFlightHash(msg)
	core.hs.attemptID = attemptID
	core.hs.ikC = ik
	core.hs.spkC = spk
	core.hs.pqpkC = pqpk
	core.localCID = cidRec{cid: 0xC1}
	core.peerCID = cidRec{cid: 0xDC1D}
	core.hs.retryDCID = 0xDC1D

	buildServerFlight := func(tokenHash [32]byte) []byte {
		ekS, err := gc.GenerateX25519()
		if err != nil {
			t.Fatal(err)
		}
		cts, ss, err := gc.MLKEM768Encaps(pqpk.EK[:])
		if err != nil {
			t.Fatal(err)
		}
		ikS, err := gc.X25519KeyPairFromPriv(srv.IdentityPriv)
		if err != nil {
			t.Fatal(err)
		}
		dh1, err := gc.X25519(ikS.Priv, spk.Pub[:])
		if err != nil {
			t.Fatal(err)
		}
		dh2, err := gc.X25519(ekS.Priv, ik.Pub[:])
		if err != nil {
			t.Fatal(err)
		}
		dh3, err := gc.X25519(ekS.Priv, spk.Pub[:])
		if err != nil {
			t.Fatal(err)
		}
		sk := gc.PQXDHKDF(dh1, dh2, dh3, ss)
		var sm wire.ServerMessage
		sm.SelectedCipher = wire.CipherXChaCha20Poly1305
		sm.ServerCipherMask = 3
		sm.AttemptID = attemptID
		sm.ClientFlightHash = core.hs.flightHash
		rand.Read(sm.ServerNonce[:])
		sm.ClientInitialCID = 0xC1
		sm.ServerInitialCID = 0x5E
		sm.OriginalDCID = 0xDC1D
		sm.AcceptedRoutingCID = 0xDC1D
		sm.AcceptedTokenHash = tokenHash
		sm.IKS = ikS.Pub
		sm.EKS = ekS.Pub
		sm.CTS = cts
		sm.Params = wire.DefaultParams()
		sm.Credentials = srv.Chain
		sb := wire.EncodeServerBody(&sm)
		sm.EncryptedBodyLength = uint32(len(sb) + 32 + wire.TagBytes)
		sp := wire.EncodeServerPrefix(&sm)
		hs := gc.DeriveServerFlight(msg, sp, sk, [32]byte{}, false, wire.CipherXChaCha20Poly1305)
		defer hs.Zeroize()
		fin := gc.ServerFinishedMAC(hs.SfKey, hs.T0, sb)
		plain := append(sb, fin[:]...)
		ct, err := gc.SealOnce(wire.CipherXChaCha20Poly1305,
			hs.HsKey[:], hs.HsIV[:wire.NonceLen(wire.CipherXChaCha20Poly1305)], plain, sp, nil)
		if err != nil {
			t.Fatal(err)
		}
		return append(sp, ct...)
	}

	// Wrong token hash: the flight is dropped before body decryption.
	bad := buildServerFlight(sha256.Sum256([]byte("not my token")))
	c.verifyServerFlight(bad)
	if core.state != stCWaitReply {
		t.Fatalf("bad token hash reached %s", core.state)
	}
	// Correct commitment to the (zero) presented token: accepted.
	good := buildServerFlight(sha256.Sum256(core.hs.token[:]))
	c.verifyServerFlight(good)
	if core.state != stCFinishing {
		t.Fatalf("valid flight rejected, state %s", core.state)
	}
}

// ctrlIndex returns the index of a used control item matching kind and
// scope identity a, or -1.
func ctrlIndex(c *Conn, kind uint8, a uint64) int {
	for i := range c.core.ctrl {
		if c.core.ctrl[i].used && c.core.ctrl[i].kind == kind &&
			c.core.ctrl[i].a == a {
			return i
		}
	}
	return -1
}

// countCtrlUsed returns the number of live obligations of kind.
func countCtrlUsed(c *Conn, kind uint8) int {
	n := 0
	for i := range c.core.ctrl {
		if c.core.ctrl[i].used && c.core.ctrl[i].kind == kind {
			n++
		}
	}
	return n
}

// emitItem serializes control item idx and returns its loss reference.
func emitItem(t *testing.T, c *Conn, idx int) lossItem {
	t.Helper()
	fb := wire.NewFrameBuilder(c.frameScratch())
	tb := &txBudget{}
	c.emitCtrlItem(fb, tb, uint32(idx))
	if len(tb.items) != 1 || tb.items[0].kind != itemCtrl ||
		tb.items[0].ctrlID != uint32(idx) {
		t.Fatalf("emitted %v items, want one ctrl item for %d", tb.items, idx)
	}
	return tb.items[0]
}

// setupPeerStream installs an active peer-initiated receive slot for sid.
func setupPeerStream(c *Conn, sid wire.StreamID, limit uint64) {
	s := c.core.slots.ensureSlot(sid.Class(), sid.Slot())
	s.state = slotActive
	s.initiator = sid.Initiator()
	s.recv = &recvHalf{state: recvOpen, limit: limit}
}

// TestStaleAckKeepsRefreshedStreamCredit: an ACK for a packet carrying an
// older MAX_STREAM_DATA value must not retire a refreshed obligation —
// the highest unsatisfied value is still owed (spec section 12.4).
func TestStaleAckKeepsRefreshedStreamCredit(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	sid := wire.MakeStreamID(0, 2, false, wire.StreamInitiatorServer)
	setupPeerStream(c, sid, 100)

	if !c.queueCtrl(ctrlMaxStreamData, sid.Slot(), uint64(sid), 100, 0, [16]byte{}) {
		t.Fatal("queue refused")
	}
	idx := ctrlIndex(c, ctrlMaxStreamData, uint64(sid))
	if idx < 0 {
		t.Fatal("no queued item")
	}
	li := emitItem(t, c, idx)
	if li.lo != 100 {
		t.Fatalf("loss reference carried lo=%d, want emitted limit 100", li.lo)
	}
	// Refresh the owed limit before the stale emission resolves.
	if !c.queueCtrl(ctrlMaxStreamData, sid.Slot(), uint64(sid), 200, 0, [16]byte{}) {
		t.Fatal("refresh refused")
	}
	c.ackCtrlItem(&li)
	it := &core.ctrl[li.ctrlID]
	if !it.used {
		t.Fatal("stale ACK destroyed the refreshed MAX_STREAM_DATA obligation")
	}
	if it.b != 200 {
		t.Fatalf("pending limit = %d, want 200", it.b)
	}
	if it.live {
		t.Fatal("refreshed item not marked for re-emission")
	}
	// An ACK covering the current emission retires the item.
	li2 := emitItem(t, c, idx)
	if li2.lo != 200 {
		t.Fatalf("re-emission carried lo=%d, want 200", li2.lo)
	}
	c.ackCtrlItem(&li2)
	if it.used {
		t.Fatal("current ACK did not retire the satisfied obligation")
	}
}

// TestStaleAckKeepsRefreshedSlotCredit: MAX_SLOTS is the same reliable
// "highest unsatisfied value" obligation — a stale ACK must not drop a
// refreshed class grant.
func TestStaleAckKeepsRefreshedSlotCredit(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	const class = uint64(2) // peer-initiated uni class
	if !c.queueCtrl(ctrlMaxSlots, 0, class, 10, 0, [16]byte{}) {
		t.Fatal("queue refused")
	}
	idx := ctrlIndex(c, ctrlMaxSlots, class)
	if idx < 0 {
		t.Fatal("no queued item")
	}
	li := emitItem(t, c, idx)
	if li.lo != 10 {
		t.Fatalf("loss reference carried lo=%d, want emitted limit 10", li.lo)
	}
	if !c.queueCtrl(ctrlMaxSlots, 0, class, 20, 0, [16]byte{}) {
		t.Fatal("refresh refused")
	}
	c.ackCtrlItem(&li)
	it := &core.ctrl[li.ctrlID]
	if !it.used || it.b != 20 || it.live {
		t.Fatalf("refreshed MAX_SLOTS lost: used=%v b=%d live=%v",
			it.used, it.b, it.live)
	}
}

// TestStaleAckKeepsRefreshedConnCredit: MAX_DATA follows the same rule.
func TestStaleAckKeepsRefreshedConnCredit(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	if !c.queueCtrl(ctrlMaxData, 0, 0, 1<<20, 0, [16]byte{}) {
		t.Fatal("queue refused")
	}
	idx := ctrlIndex(c, ctrlMaxData, 0)
	if idx < 0 {
		t.Fatal("no queued item")
	}
	li := emitItem(t, c, idx)
	if li.lo != 1<<20 {
		t.Fatalf("loss reference carried lo=%d, want %d", li.lo, 1<<20)
	}
	if !c.queueCtrl(ctrlMaxData, 0, 0, 2<<20, 0, [16]byte{}) {
		t.Fatal("refresh refused")
	}
	c.ackCtrlItem(&li)
	it := &core.ctrl[li.ctrlID]
	if !it.used || it.b != 2<<20 || it.live {
		t.Fatalf("refreshed MAX_DATA lost: used=%v b=%d live=%v",
			it.used, it.b, it.live)
	}
}

// TestCtrlScopeIdentityDistinct: obligations of the same kind but
// different scope identity must not merge — MAX_SLOTS classes, streams
// sharing slot+generation across classes, distinct issued CIDs, distinct
// RETIRE_CID sequences, and distinct park request IDs.
func TestCtrlScopeIdentityDistinct(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	// Two MAX_SLOTS classes.
	c.queueCtrl(ctrlMaxSlots, 0, 0, 10, 0, [16]byte{})
	c.queueCtrl(ctrlMaxSlots, 0, 2, 20, 0, [16]byte{})
	if n := countCtrlUsed(c, ctrlMaxSlots); n != 2 {
		t.Fatalf("MAX_SLOTS classes merged: %d items, want 2", n)
	}
	// Two peer-initiated streams sharing slot 3 gen 0 in classes 1 and 3.
	sidBidi := wire.MakeStreamID(0, 3, false, wire.StreamInitiatorServer)
	sidUni := wire.MakeStreamID(0, 3, true, wire.StreamInitiatorServer)
	if sidBidi.Slot() != sidUni.Slot() || sidBidi.Generation() != sidUni.Generation() ||
		sidBidi.Class() == sidUni.Class() {
		t.Fatal("test sids do not exercise the slot+gen collision")
	}
	setupPeerStream(c, sidBidi, 100)
	setupPeerStream(c, sidUni, 100)
	c.queueCtrl(ctrlMaxStreamData, sidBidi.Slot(), uint64(sidBidi), 100, 0, [16]byte{})
	c.queueCtrl(ctrlMaxStreamData, sidUni.Slot(), uint64(sidUni), 300, 0, [16]byte{})
	if n := countCtrlUsed(c, ctrlMaxStreamData); n != 2 {
		t.Fatalf("cross-class stream credits merged: %d items, want 2", n)
	}
	if got := core.ctrl[ctrlIndex(c, ctrlMaxStreamData, uint64(sidBidi))].b; got != 100 {
		t.Fatalf("bidi stream limit = %d, want 100", got)
	}
	if got := core.ctrl[ctrlIndex(c, ctrlMaxStreamData, uint64(sidUni))].b; got != 300 {
		t.Fatalf("uni stream limit = %d, want 300", got)
	}
	// Two outstanding issued CIDs.
	c.queueCtrl(ctrlNewCID, 0, 1001, 0, 0, [16]byte{1})
	c.queueCtrl(ctrlNewCID, 0, 1002, 1, 0, [16]byte{2})
	if n := countCtrlUsed(c, ctrlNewCID); n != 2 {
		t.Fatalf("issued CIDs merged: %d items, want 2", n)
	}
	// Two RETIRE_CID sequences.
	c.queueCtrl(ctrlRetireCID, 0, 5, 0, 0, [16]byte{})
	c.queueCtrl(ctrlRetireCID, 0, 6, 0, 0, [16]byte{})
	if n := countCtrlUsed(c, ctrlRetireCID); n != 2 {
		t.Fatalf("RETIRE_CID sequences merged: %d items, want 2", n)
	}
	// Two park request IDs.
	c.queueCtrl(ctrlParkRequest, 0, 7, 5000, 0, [16]byte{})
	c.queueCtrl(ctrlParkRequest, 0, 8, 5000, 0, [16]byte{})
	if n := countCtrlUsed(c, ctrlParkRequest); n != 2 {
		t.Fatalf("PARK_REQUEST IDs merged: %d items, want 2", n)
	}
}

// TestSameScopeRefreshUpdatesPending: re-queuing a same-identity
// obligation whose payload advanced must update the pending value and
// re-arm emission — KEY_REQUEST epochs and PARK_WAKE sequences.
func TestSameScopeRefreshUpdatesPending(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	c.queueCtrl(ctrlKeyRequest, 0, 0, 0, 5, [16]byte{})
	idx := ctrlIndex(c, ctrlKeyRequest, 0)
	if idx < 0 {
		t.Fatal("no queued KEY_REQUEST")
	}
	li := emitItem(t, c, idx)
	if li.lo != 5 {
		t.Fatalf("loss reference carried lo=%d, want emitted epoch 5", li.lo)
	}
	it := &core.ctrl[idx]
	c.queueCtrl(ctrlKeyRequest, 0, 0, 0, 6, [16]byte{})
	if it.c != 6 || it.live {
		t.Fatalf("KEY_REQUEST not refreshed to epoch 6: c=%d live=%v", it.c, it.live)
	}
	// A stale ACK of the epoch-5 emission must not retire epoch 6.
	c.ackCtrlItem(&li)
	if !it.used {
		t.Fatal("stale KEY_REQUEST ACK retired the refreshed epoch")
	}
	c.queueCtrl(ctrlParkWake, 0, 7, 11, 0, [16]byte{})
	widx := ctrlIndex(c, ctrlParkWake, 7)
	if widx < 0 {
		t.Fatal("no queued PARK_WAKE")
	}
	wli := emitItem(t, c, widx)
	if wli.lo != 11 {
		t.Fatalf("loss reference carried lo=%d, want emitted wake_seq 11", wli.lo)
	}
	wit := &core.ctrl[widx]
	c.queueCtrl(ctrlParkWake, 0, 7, 12, 0, [16]byte{})
	if wit.b != 12 || wit.live {
		t.Fatalf("PARK_WAKE not refreshed to seq 12: b=%d live=%v", wit.b, wit.live)
	}
	// A stale ACK of the seq-11 emission must not retire seq 12.
	c.ackCtrlItem(&wli)
	if !wit.used {
		t.Fatal("stale wake ACK retired the refreshed sequence")
	}
}

// TestStaleAckKeepsRegeneratedSelected: an ACK of an older
// PATH_SELECTED status (BUSY) must not retire the regenerated result
// (ACCEPTED) — it remains owed until its own ACK (spec section 11).
func TestStaleAckKeepsRegeneratedSelected(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	c.queueCtrlPath(ctrlPathSelected, 0, 0, 9, 0, 2, [16]byte{}) // BUSY
	idx := -1
	for i := range core.ctrl {
		if core.ctrl[i].used && core.ctrl[i].kind == ctrlPathSelected {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no queued PATH_SELECTED")
	}
	li := emitItem(t, c, idx)
	if li.lo != 2 {
		t.Fatalf("loss reference carried lo=%d, want status 2", li.lo)
	}
	// Path became READY: the result regenerates to ACCEPTED.
	c.queueCtrlPath(ctrlPathSelected, 0, 0, 9, 0, 0, [16]byte{})
	it := &core.ctrl[idx]
	if it.c != 0 || it.live {
		t.Fatalf("result not regenerated to ACCEPTED: c=%d live=%v", it.c, it.live)
	}
	c.ackCtrlItem(&li)
	if !it.used {
		t.Fatal("stale-status ACK retired the regenerated PATH_SELECTED")
	}
	// ACK of the ACCEPTED emission resolves it.
	li2 := emitItem(t, c, idx)
	if li2.lo != 0 {
		t.Fatalf("re-emission carried lo=%d, want status 0", li2.lo)
	}
	c.ackCtrlItem(&li2)
	if it.used {
		t.Fatal("current-status ACK did not retire PATH_SELECTED")
	}
}

// TestStaleAckRetiresClosedCommit: an ACK for a KEY_COMMIT packet must
// re-arm the obligation only while its own transaction is live and
// unconfirmed. After finishRefresh clears the transaction, a commit ACK
// arriving late retires the item — otherwise the refreshed-in-place slot
// leaks permanently (no emission path reaches it again).
func TestStaleAckRetiresClosedCommit(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	var txID [16]byte
	txID[0] = 9
	core.refresh = refreshTx{active: true, id: txID, commitSent: true}
	if !c.queueCtrl(ctrlKeyCommit, 0, 1, 0, 0, txID) {
		t.Fatal("queue refused")
	}
	idx := ctrlIndex(c, ctrlKeyCommit, 1)
	if idx < 0 {
		t.Fatal("no queued KEY_COMMIT")
	}
	it := &core.ctrl[idx]
	// While the transaction is open and unconfirmed, an ACK proves
	// delivery: the obligation re-arms for paced retransmission.
	li := emitItem(t, c, idx)
	c.ackCtrlItem(&li)
	if !it.used || it.live {
		t.Fatalf("live-tx commit ACK did not re-arm: used=%v live=%v",
			it.used, it.live)
	}
	if core.refresh.commitNextUS == 0 {
		t.Fatal("paced retransmission was not scheduled")
	}
	// Once the transaction closes, a late ACK of a commit packet
	// retires the item rather than reviving unreachable work.
	li2 := emitItem(t, c, idx)
	c.finishRefresh()
	c.ackCtrlItem(&li2)
	if it.used {
		t.Fatal("commit item survived its transaction's close")
	}
}

// TestAttemptReleaseOnDeath: an admitted attempt frees its pool slot
// when the connection dies before confirmation (section 20.1 bound).
func TestAttemptReleaseOnDeath(t *testing.T) {
	_, scfg := testConfigs(t)
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.1:4433")}
	l := NewListener(scfg, sock, []byte("test-domain"))
	c := newConn(l.cfg, &routeSocket{ps: sock, local: sock.addr}, 1)
	c.core.listener = l
	l.mu.Lock()
	l.attempts[[wire.AttemptIDBytes]byte{7}] = c
	l.attemptsInFlight = 1
	l.mu.Unlock()
	c.core.attemptHeld = true
	c.quiesce(nil)
	// The slot frees at quiesce; the routing entry leaves when the conn's
	// loop exits and the listener removes it.
	l.removeConn(c)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attemptsInFlight != 0 {
		t.Fatalf("dead attempt still holds slot: %d", l.attemptsInFlight)
	}
	if _, ok := l.attempts[[wire.AttemptIDBytes]byte{7}]; ok {
		t.Fatal("dead attempt still routes in the attempt table")
	}
}
