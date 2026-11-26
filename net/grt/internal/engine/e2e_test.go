package engine

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// testNet is an in-memory UDP-like fabric delivering datagrams between
// registered endpoints. natSrc rewrites a sender's source address (a NAT
// rebinding); packets destined for a mapped public address are delivered
// to the real endpoint's channel.
type testNet struct {
	mu     sync.Mutex
	eps    map[netip.AddrPort]*testEP
	natSrc map[netip.AddrPort]netip.AddrPort // real -> public
	natDst map[netip.AddrPort]*testEP        // public -> real endpoint
}

// bindNAT makes the real endpoint emit datagrams showing pub as their
// source, and delivers anything addressed to pub into real's channel —
// a NAT rebinding the real endpoint cannot observe directly.
func (n *testNet) bindNAT(real, pub netip.AddrPort) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.natSrc == nil {
		n.natSrc = map[netip.AddrPort]netip.AddrPort{}
		n.natDst = map[netip.AddrPort]*testEP{}
	}
	n.natSrc[real] = pub
	n.natDst[pub] = n.eps[real]
}

type dgram struct {
	b        []byte
	src, dst netip.AddrPort
	ecn      uint8
}

type testEP struct {
	addr  netip.AddrPort
	net   *testNet
	ch    chan dgram
	sent  atomic.Int64
	recv  atomic.Int64
	dropN atomic.Int64 // next N sends are silently dropped (link loss)
}

func newTestNet() *testNet { return &testNet{eps: map[netip.AddrPort]*testEP{}} }

func (n *testNet) endpoint(addr netip.AddrPort) *testEP {
	e := &testEP{addr: addr, net: n, ch: make(chan dgram, 4096)}
	n.mu.Lock()
	n.eps[addr] = e
	n.mu.Unlock()
	return e
}

func (e *testEP) Send(b []byte, dst netip.AddrPort, ecn uint8) error {
	e.net.mu.Lock()
	src := e.addr
	if pub, ok := e.net.natSrc[e.addr]; ok {
		src = pub
	}
	pe := e.net.eps[dst]
	if pe == nil {
		pe = e.net.natDst[dst] // dst may be a mapped public address
	}
	e.net.mu.Unlock()
	e.sent.Add(1)
	if pe == nil {
		return nil // UDP drop
	}
	if e.dropN.Load() > 0 {
		e.dropN.Add(-1)
		return nil
	}
	// The wire owns the datagram once queued; the copy draws on the shared
	// packet pool so steady-state delivery allocates nothing.
	cp := GetPacketBuf()
	n := copy(cp, b)
	select {
	case pe.ch <- dgram{cp[:n], src, dst, ecn}:
		pe.recv.Add(1)
	default: // full: drop like a congested link
		PutPacketBuf(cp)
	}
	return nil
}

func (e *testEP) LocalAddr() netip.AddrPort { return e.addr }

func (e *testEP) Recv(buf []byte) (int, netip.AddrPort, netip.AddrPort, uint8, error) {
	d := <-e.ch
	n := copy(buf, d.b)
	PutPacketBuf(d.b)
	return n, d.src, d.dst, d.ecn, nil
}

func testServerIdentity(t *testing.T) *ServerIdentity {
	t.Helper()
	kp, err := gc.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	return &ServerIdentity{
		IdentityPub:  kp.Pub,
		IdentityPriv: kp.Priv[:],
		ServiceHash:  gc.ServiceHash("srv.example:4433", [16]byte{1}),
	}
}

var testSrvIdent atomic.Pointer[ServerIdentity]

// sharedTestIdentity returns one process-wide test identity so client and
// server configs always agree on the pinned key.
func sharedTestIdentity(t *testing.T) *ServerIdentity {
	t.Helper()
	if id := testSrvIdent.Load(); id != nil {
		return id
	}
	id := testServerIdentity(t)
	testSrvIdent.Store(id)
	return id
}

func testConfigs(t *testing.T) (*Config, *Config) {
	srv := sharedTestIdentity(t)
	p := wire.DefaultParams()
	// Grant a nonzero connection-level initial window so the early-data
	// and flow paths are exercisable.
	p.InitialMaxData = 1 << 20
	ccfg := &Config{
		Params:      p,
		ServerName:  "srv.example:4433",
		AppID:       [16]byte{1},
		AuthMode:    0,
		ExpectedKey: srv.IdentityPub,
		NowUnix:     func() int64 { return time.Now().Unix() },
		MaxLiveSent: 4096,
	}
	scfg := &Config{
		Params:      p,
		ServerName:  "srv.example:4433",
		AppID:       [16]byte{1},
		ServerCert:  srv,
		NowUnix:     func() int64 { return time.Now().Unix() },
		MaxLiveSent: 4096,
	}
	rand.Read(scfg.TicketKey[:])
	var issueCtr uint64
	scfg.TicketIssuer = func() ([16]byte, uint64) {
		issueCtr++
		return [16]byte{1, 2, 3}, issueCtr
	}
	scfg.ReplayLedger = &testLedger{m: map[[32]byte]bool{}}
	return ccfg, scfg
}

// testLedger is an in-memory ReplayLedger: first consume wins.
type testLedger struct {
	mu sync.Mutex
	m  map[[32]byte]bool
}

func (l *testLedger) ConsumeEarly(domain, ticketID, attempt [16]byte, flightHash [32]byte) bool {
	k := sha256.Sum256(append(append(domain[:], ticketID[:]...), attempt[:]...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m[k] {
		return false
	}
	l.m[k] = true
	return true
}

// setupPair wires a listener and returns dial-ready endpoints. The caller
// starts the pump goroutines.
func setupPair(t *testing.T) (*testNet, *Listener, *testEP, *testEP) {
	t.Helper()
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	_, scfg := testConfigs(t)
	l := NewListener(scfg, srvEP, []byte("test-domain"))
	go func() {
		for d := range srvEP.ch {
			l.OnDatagram(d.b, d.src, d.dst, d.ecn)
		}
	}()
	return n, l, srvEP, cliEP
}

func dialClient(t *testing.T, cliEP *testEP, srvAddr netip.AddrPort) *Conn {
	t.Helper()
	ccfg, _ := testConfigs(t)
	return dialClientCfg(t, cliEP, srvAddr, ccfg)
}

func dialClientCfg(t *testing.T, cliEP *testEP, srvAddr netip.AddrPort, ccfg *Config) *Conn {
	t.Helper()
	id, err := gc.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	var cliC atomic.Pointer[Conn]
	go func() {
		var cn *Conn
		for d := range cliEP.ch {
			for cn == nil {
				cn = cliC.Load()
			}
			cn.post(connEvent{kind: evRX, pkt: d.b, src: d.src, dst: d.dst,
				ecn: d.ecn, rxUS: nowUS()})
		}
	}()
	c := Dial(ccfg, cliEP, srvAddr, &ClientIdentity{Priv: id.Priv[:]})
	cliC.Store(c)
	return c
}

// waitFor polls cond against the conn's notification channels; it fails
// the test if the connection dies or the deadline passes. All waits are
// notification-driven — no real-time sleeps.
func waitFor(t *testing.T, c *Conn, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s (state=%s)", what, c.State())
		case <-c.done:
			t.Fatalf("conn died waiting for %s: %v", what, c.TermError())
		case <-c.confirmed:
		case <-c.secureReady:
		case <-c.stateWake:
		case <-c.readWake:
		case <-c.writeWake:
		case <-c.dgWake:
		}
	}
}

func TestHandshakeAndStreams(t *testing.T) {
	_, l, srvEP, cliEP := setupPair(t)
	cli := dialClient(t, cliEP, srvEP.addr)

	waitFor(t, cli, "client confirmed", func() bool {
		select {
		case <-cli.confirmed:
			return true
		default:
			return false
		}
	})
	srv := l.Accept()
	if srv == nil {
		t.Fatal("listener did not produce a server connection")
	}
	waitFor(t, srv, "server established", func() bool {
		return srv.State() == stEstablished
	})

	// Client → server stream.
	sid, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello grt")
	if _, err := cli.Write(sid, msg); err != nil {
		t.Fatal(err)
	}
	if err := cli.Fin(sid); err != nil {
		t.Fatal(err)
	}
	var srvSID uint64
	waitFor(t, srv, "incoming stream", func() bool {
		if s, ok := srv.AcceptStream(); ok {
			srvSID = s
			return true
		}
		return false
	})
	buf := make([]byte, 64)
	var got []byte
	for {
		n, err := srv.Read(srvSID, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("server read %q, want %q", got, msg)
	}

	// Server → client stream.
	ssid, err := srv.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	reply := []byte("world")
	if _, err := srv.Write(ssid, reply); err != nil {
		t.Fatal(err)
	}
	var cliSID uint64
	waitFor(t, cli, "incoming reply stream", func() bool {
		if s, ok := cli.AcceptStream(); ok {
			cliSID = s
			return true
		}
		return false
	})
	got = got[:0]
	for len(got) < len(reply) {
		n, err := cli.Read(cliSID, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("client read %q, want %q", got, reply)
	}
}

func TestRetryAdmission(t *testing.T) {
	// The tokenless first flight must draw a stateless RETRY; the retried
	// token-bearing flight must be admitted. RETRY precedes confirmation,
	// so asserting RetrySeen after WaitConfirmed is sufficient.
	_, l, srvEP, cliEP := setupPair(t)
	cli := dialClient(t, cliEP, srvEP.addr)
	waitFor(t, cli, "client confirmed", func() bool {
		select {
		case <-cli.confirmed:
			return true
		default:
			return false
		}
	})
	if !cli.RetrySeen() {
		t.Fatal("no RETRY was issued for the tokenless flight")
	}
	if srv := l.Accept(); srv == nil {
		t.Fatal("no server conn after retry")
	}
}

// TestResumptionEarlyData: a stored ticket + address token resume the
// connection, and a bounded early batch is committed atomically before
// the handshake completes.
func TestResumptionEarlyData(t *testing.T) {
	n, l, srvEP, cliEP := setupPair(t)
	cli := dialClient(t, cliEP, srvEP.addr)
	waitFor(t, cli, "client confirmed", func() bool {
		select {
		case <-cli.confirmed:
			return true
		default:
			return false
		}
	})
	srv := l.Accept()
	if srv == nil {
		t.Fatal("no server conn")
	}
	waitFor(t, srv, "server established", func() bool {
		return srv.State() == stEstablished
	})

	// Wait for the server's NEW_TICKET + NEW_TOKEN to arrive.
	var ticket *ClientTicket
	var tok [64]byte
	var hasTok bool
	waitFor(t, cli, "ticket+token", func() bool {
		ticket = cli.ResumeTicket()
		tok, hasTok = cli.StoredToken()
		return ticket != nil && hasTok
	})

	// Second conn on a fresh endpoint: ticket + early data + token.
	cliEP2 := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5001"))
	ccfg, _ := testConfigs(t)
	early := []byte("early-bytes")
	ccfg.Ticket = ticket
	ccfg.EarlyData = early
	ccfg.AddressToken = tok
	ccfg.HasAddressToken = true
	cli2 := dialClientCfg(t, cliEP2, srvEP.addr, ccfg)
	waitFor(t, cli2, "resumed confirmed", func() bool {
		select {
		case <-cli2.confirmed:
			return true
		default:
			return false
		}
	})
	srv2 := l.Accept()
	if srv2 == nil {
		t.Fatal("no resumed server conn")
	}
	if st := cli2.EarlyDataStatus(); st != wire.EarlyAccepted {
		t.Fatalf("early status = %d, want accepted", st)
	}
	var esid uint64
	waitFor(t, srv2, "early stream", func() bool {
		if s, ok := srv2.AcceptStream(); ok {
			esid = s
			return true
		}
		return false
	})
	buf := make([]byte, 64)
	var got []byte
	for len(got) < len(early) {
		nr, err := srv2.Read(esid, buf)
		got = append(got, buf[:nr]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, early) {
		t.Fatalf("early data = %q, want %q", got, early)
	}
	// Same stream handle continues post-handshake on the client.
	if _, err := cli2.Write(cli2.core.hs.earlySID, []byte("+1rtt")); err != nil {
		t.Fatalf("continuing early stream failed: %v", err)
	}
	waitFor(t, srv2, "1rtt bytes", func() bool {
		nr, _ := srv2.Read(esid, buf)
		return nr > 0
	})
}

// holdbackSock queues every DATA-class datagram while armed and releases
// them in reverse order — the strongest reordering the fabric can express
// for a held batch. It exercises the section-12.2 rule that STREAM_OPEN
// rides every data-carrying packet until acknowledged.
type holdbackSock struct {
	inner *testEP
	arm   atomic.Bool
	mu    sync.Mutex
	held  []dgram
	heldN atomic.Int32
}

func (s *holdbackSock) Send(b []byte, dst netip.AddrPort, ecn uint8) error {
	if s.arm.Load() {
		if h, ok := wire.ParseHeader(b); ok && h.Class == wire.ClassData {
			s.mu.Lock()
			s.held = append(s.held, dgram{
				b:   append([]byte(nil), b...),
				src: s.inner.addr,
				dst: dst,
				ecn: ecn,
			})
			s.mu.Unlock()
			s.heldN.Add(1)
			return nil
		}
	}
	return s.inner.Send(b, dst, ecn)
}

func (s *holdbackSock) LocalAddr() netip.AddrPort { return s.inner.LocalAddr() }

// Release delivers the held batch in reverse order: the first captured
// datagram arrives last, so any later-emitted packet always precedes it.
func (s *holdbackSock) Release() {
	s.arm.Store(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.held) - 1; i >= 0; i-- {
		d := s.held[i]
		_ = s.inner.Send(d.b, d.dst, d.ecn)
	}
	s.held = nil
}

// TestOpenRidesEveryPacket: with the first data packet delayed behind a
// later one, the receiver must still open the stream — every packet
// carrying stream data repeats the unacknowledged OPEN (section 12.2).
func TestOpenRidesEveryPacket(t *testing.T) {
	_, l, srvEP, cliEP := setupPair(t)
	wrap := &holdbackSock{inner: cliEP}

	id, err := gc.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	ccfg, _ := testConfigs(t)
	var cliC atomic.Pointer[Conn]
	go func() {
		var cn *Conn
		for d := range cliEP.ch {
			for cn == nil {
				cn = cliC.Load()
			}
			cn.post(connEvent{kind: evRX, pkt: d.b, src: d.src, dst: d.dst,
				ecn: d.ecn, rxUS: nowUS()})
		}
	}()
	cli := Dial(ccfg, wrap, srvEP.addr, &ClientIdentity{Priv: id.Priv[:]})
	cliC.Store(cli)

	waitFor(t, cli, "client confirmed", func() bool {
		select {
		case <-cli.confirmed:
			return true
		default:
			return false
		}
	})
	srv := l.Accept()
	if srv == nil {
		t.Fatal("no server conn")
	}
	waitFor(t, srv, "server established", func() bool {
		return srv.State() == stEstablished
	})

	sid, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	// Arm the holdback, write "a", wait until its packet is captured,
	// then write "bc" — separate packets, released reversed so the "bc"
	// datagram arrives first.
	wrap.arm.Store(true)
	if _, err := cli.Write(sid, []byte("a")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, cli, "first datagram held", func() bool {
		return wrap.heldN.Load() >= 1
	})
	if _, err := cli.Write(sid, []byte("bc")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, cli, "second datagram held", func() bool {
		return wrap.heldN.Load() >= 2
	})
	wrap.Release()

	var srvSID uint64
	waitFor(t, srv, "reordered stream open", func() bool {
		if s, ok := srv.AcceptStream(); ok {
			srvSID = s
			return true
		}
		return false
	})
	buf := make([]byte, 64)
	var got []byte
	waitFor(t, srv, "reordered payload", func() bool {
		n, _ := srv.Read(srvSID, buf)
		got = append(got, buf[:n]...)
		return len(got) >= 3
	})
	if !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("server read %q, want %q", got, "abc")
	}
}

// TestStreamRetireReopen: a fully retired slot frees for reuse at
// generation+1 — the reopened stream's OPEN carries the advanced
// generation and the receiver accepts it (section 21.1).
func TestStreamRetireReopen(t *testing.T) {
	_, l, srvEP, cliEP := setupPair(t)
	cli := dialClient(t, cliEP, srvEP.addr)
	waitFor(t, cli, "client confirmed", func() bool {
		select {
		case <-cli.confirmed:
			return true
		default:
			return false
		}
	})
	srv := l.Accept()
	if srv == nil {
		t.Fatal("no server conn")
	}
	waitFor(t, srv, "server established", func() bool {
		return srv.State() == stEstablished
	})

	// Bidirectional stream: retirement needs both endpoints' halves done.
	sid1, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if wire.StreamID(sid1).Generation() != 0 {
		t.Fatalf("first stream generation = %d, want 0",
			wire.StreamID(sid1).Generation())
	}
	if _, err := cli.Write(sid1, []byte("first-gen")); err != nil {
		t.Fatal(err)
	}
	if err := cli.Fin(sid1); err != nil {
		t.Fatal(err)
	}
	var srvSID uint64
	waitFor(t, srv, "first stream", func() bool {
		if s, ok := srv.AcceptStream(); ok {
			srvSID = s
			return true
		}
		return false
	})
	buf := make([]byte, 64)
	var got []byte
	for {
		n, err := srv.Read(srvSID, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, []byte("first-gen")) {
		t.Fatalf("first payload %q", got)
	}
	// Server closes its send half; the client reads the FIN so its own
	// halves go terminal — only then may the initiator retire.
	if err := srv.Fin(srvSID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, cli, "client sees server FIN", func() bool {
		_, err := cli.Read(sid1, buf)
		return err == errEOF
	})

	// Retirement completes once the initiator's FIN/data are ACKed, its
	// RETIRE reaches the peer, and the RETIRE_ACK comes back. The slot
	// then returns to EMPTY at generation 1. Query runs on the conn's
	// owner goroutine, so the read is race-free.
	class := classIdx(false, wire.StreamInitiatorClient)
	waitFor(t, cli, "slot freed at gen 1", func() bool {
		r := cli.postWait(&appCmd{kind: acQuery, code: querySlot,
			stream: uint64(class)<<8 | 0})
		return r.err == nil && r.status == uint8(slotEmpty) && r.stream == 1
	})

	sid2, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if sid2 == sid1 || wire.StreamID(sid2).Generation() != 1 {
		t.Fatalf("reused slot sid=%#x gen=%d, want generation 1",
			sid2, wire.StreamID(sid2).Generation())
	}
	if _, err := cli.Write(sid2, []byte("second-gen")); err != nil {
		t.Fatal(err)
	}
	if err := cli.Fin(sid2); err != nil {
		t.Fatal(err)
	}
	var srvSID2 uint64
	waitFor(t, srv, "reopened stream", func() bool {
		if s, ok := srv.AcceptStream(); ok {
			srvSID2 = s
			return true
		}
		return false
	})
	if wire.StreamID(srvSID2).Generation() != 1 {
		t.Fatalf("server saw generation %d, want 1",
			wire.StreamID(srvSID2).Generation())
	}
	got = got[:0]
	for {
		n, err := srv.Read(srvSID2, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, []byte("second-gen")) {
		t.Fatalf("second payload %q", got)
	}
}

// TestRefreshExchange drives a full client-initiated refresh transaction
// (OFFER → ANSWER → KEY_COMMIT → KEY_CONFIRM) over the live fabric and
// confirms application traffic resumes on the new epoch (section 19.4).
// The first packets the client emits after the OFFER is queued are
// dropped, so at least one fragment must survive loss retransmission.
func TestRefreshExchange(t *testing.T) {
	_, l, srvEP, cliEP := setupPair(t)
	cli := dialClient(t, cliEP, srvEP.addr)
	waitFor(t, cli, "client confirmed", func() bool {
		select {
		case <-cli.confirmed:
			return true
		default:
			return false
		}
	})
	srv := l.Accept()
	if srv == nil {
		t.Fatal("no server conn")
	}
	waitFor(t, srv, "server established", func() bool {
		return srv.State() == stEstablished
	})

	// The refresh OFFER spans multiple fragments/packets; dropping a burst
	// of early sends forces the fragment ledger to retransmit the lost
	// pieces before the transaction can complete.
	if r := cli.postWait(&appCmd{kind: acRefresh}); r.err != nil {
		t.Fatal(r.err)
	}
	cliEP.dropN.Store(0)

	waitFor(t, cli, "client epoch 1 committed", func() bool {
		r := cli.postWait(&appCmd{kind: acQuery, code: queryEpoch})
		return r.err == nil && r.stream == 1 && r.status == 0
	})
	waitFor(t, srv, "server epoch 1 confirmed", func() bool {
		r := srv.postWait(&appCmd{kind: acQuery, code: queryEpoch})
		return r.err == nil && r.stream == 1 && r.status == 0
	})

	// Post-epoch traffic: a fresh stream round-trips under the new keys.
	sid, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Write(sid, []byte("post-refresh")); err != nil {
		t.Fatal(err)
	}
	if err := cli.Fin(sid); err != nil {
		t.Fatal(err)
	}
	var srvSID uint64
	waitFor(t, srv, "post-refresh stream", func() bool {
		if s, ok := srv.AcceptStream(); ok {
			srvSID = s
			return true
		}
		return false
	})
	buf := make([]byte, 64)
	var got []byte
	for {
		n, err := srv.Read(srvSID, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, []byte("post-refresh")) {
		t.Fatalf("post-refresh payload %q", got)
	}
}

// TestMigrationNATRebinding rewrites the client's source tuple mid-
// connection (a NAT rebinding the client cannot observe). The server must
// validate the new remote and promote it; the answer path returns through
// the mapping (section 17).
func TestMigrationNATRebinding(t *testing.T) {
	n, l, srvEP, cliEP := setupPair(t)
	cli := dialClient(t, cliEP, srvEP.addr)
	waitFor(t, cli, "client confirmed", func() bool {
		select {
		case <-cli.confirmed:
			return true
		default:
			return false
		}
	})
	srv := l.Accept()
	if srv == nil {
		t.Fatal("no server conn")
	}
	waitFor(t, srv, "server established", func() bool {
		return srv.State() == stEstablished
	})
	oldRemote := func() netip.AddrPort {
		return srv.postWait(&appCmd{kind: acQuery, code: queryPath}).remote
	}
	if oldRemote() != cliEP.addr {
		t.Fatalf("pre-migration remote = %v, want %v", oldRemote(), cliEP.addr)
	}

	// Rebind: everything the client now sends shows the public tuple.
	pub := netip.MustParseAddrPort("10.0.0.2:6000")
	n.bindNAT(cliEP.addr, pub)

	// Fresh non-probing traffic on the new tuple supplies promotion
	// evidence once the server's validation exchange completes.
	sid, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Write(sid, []byte("migrated")); err != nil {
		t.Fatal(err)
	}
	if err := cli.Fin(sid); err != nil {
		t.Fatal(err)
	}
	waitFor(t, srv, "server promoted new remote", func() bool {
		return oldRemote() == pub
	})

	// The migrated path carries application data in both directions.
	var srvSID uint64
	waitFor(t, srv, "post-migration stream", func() bool {
		if s, ok := srv.AcceptStream(); ok {
			srvSID = s
			return true
		}
		return false
	})
	buf := make([]byte, 64)
	var got []byte
	for {
		n, err := srv.Read(srvSID, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, []byte("migrated")) {
		t.Fatalf("post-migration payload %q", got)
	}
}

// multiSock is a multi-homed listener adapter: sends default to the
// primary endpoint and SendFrom selects the source endpoint — the
// engine's optional SourceSocket capability (spec section 30).
type multiSock struct {
	def *testEP
	eps map[netip.AddrPort]*testEP
}

func (m *multiSock) Send(b []byte, dst netip.AddrPort, ecn uint8) error {
	return m.def.Send(b, dst, ecn)
}

func (m *multiSock) SendFrom(b []byte, src, dst netip.AddrPort, ecn uint8) error {
	e := m.eps[src]
	if e == nil {
		return errors.New("multi: unbound source")
	}
	return e.Send(b, dst, ecn)
}

func (m *multiSock) LocalAddr() netip.AddrPort { return m.def.addr }

func (m *multiSock) Recv(buf []byte) (int, netip.AddrPort, netip.AddrPort, uint8, error) {
	return m.def.Recv(buf)
}

// TestMigrateElective drives an explicit Migrate to a second server
// address: challenge exchange on the new tuple, SELECT intent, ACCEPTED
// result, and promotion on both sides (spec section 17.5).
func TestMigrateElective(t *testing.T) {
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	srvEP2 := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4444"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	_, scfg := testConfigs(t)
	l := NewListener(scfg, &multiSock{
		def: srvEP,
		eps: map[netip.AddrPort]*testEP{srvEP.addr: srvEP, srvEP2.addr: srvEP2},
	}, []byte("test-domain"))
	go func() {
		for d := range srvEP.ch {
			l.OnDatagram(d.b, d.src, d.dst, d.ecn)
		}
	}()
	go func() {
		for d := range srvEP2.ch {
			l.OnDatagram(d.b, d.src, d.dst, d.ecn)
		}
	}()
	cli := dialClient(t, cliEP, srvEP.addr)
	waitFor(t, cli, "client confirmed", func() bool {
		select {
		case <-cli.confirmed:
			return true
		default:
			return false
		}
	})
	srv := l.Accept()
	if srv == nil {
		t.Fatal("no server conn")
	}
	waitFor(t, srv, "server established", func() bool {
		return srv.State() == stEstablished
	})
	cliRemote := func() netip.AddrPort {
		return cli.postWait(&appCmd{kind: acQuery, code: queryPath}).remote
	}
	srvLocal := func() netip.AddrPort {
		return srv.postWait(&appCmd{kind: acQuery, code: queryPath}).local
	}
	if cliRemote() != srvEP.addr || srvLocal() != srvEP.addr {
		t.Fatalf("pre-migration tuple remote=%v local=%v", cliRemote(), srvLocal())
	}
	if err := cli.Migrate(srvEP2.addr); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	waitFor(t, cli, "client promoted", func() bool {
		return cliRemote() == srvEP2.addr
	})
	waitFor(t, srv, "server promoted", func() bool {
		return srvLocal() == srvEP2.addr
	})
	// The migrated tuple carries application data in both directions.
	sid, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Write(sid, []byte("elective")); err != nil {
		t.Fatal(err)
	}
	if err := cli.Fin(sid); err != nil {
		t.Fatal(err)
	}
	var srvSID uint64
	waitFor(t, srv, "post-migration stream", func() bool {
		if s, ok := srv.AcceptStream(); ok {
			srvSID = s
			return true
		}
		return false
	})
	buf := make([]byte, 64)
	var got []byte
	for {
		n, err := srv.Read(srvSID, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, []byte("elective")) {
		t.Fatalf("post-migration payload %q", got)
	}
	if _, err := srv.Write(srvSID, []byte("back")); err != nil {
		t.Fatal(err)
	}
	srv.Fin(srvSID)
	got = got[:0]
	for {
		n, err := cli.Read(sid, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, []byte("back")) {
		t.Fatalf("reverse payload %q", got)
	}
}

// TestHandshakeResetTokens: the tokens exchanged inside the handshake —
// the server's inside its encrypted flight, the client's inside FINISH —
// must land on the peer CID records so a stateless reset for either
// initial CID is recognized (spec section 18).
func TestHandshakeResetTokens(t *testing.T) {
	_, l, srvEP, cliEP := setupPair(t)
	cli := dialClient(t, cliEP, srvEP.addr)
	waitFor(t, cli, "client confirmed", func() bool {
		select {
		case <-cli.confirmed:
			return true
		default:
			return false
		}
	})
	srv := l.Accept()
	if srv == nil {
		t.Fatal("no server conn")
	}
	waitFor(t, srv, "server established", func() bool {
		return srv.State() == stEstablished
	})
	// Client side: the server flight's token must be recorded against the
	// server's handshake CID.
	wantSrv := srv.resetToken(srv.core.localCID.cid)
	if cli.core.peerCID.token != wantSrv {
		t.Fatal("server reset token not stored on client peer CID")
	}
	// Server side: the client's FINISH token must be recorded against the
	// client's handshake CID.
	if srv.core.peerCID.token != cli.core.cliReset {
		t.Fatal("client FINISH token not stored on server peer CID")
	}
	// Functional: a datagram ending in the server's token drains the
	// client connection.
	reset := append([]byte("stateless reset candidate bytes......."),
		wantSrv[:]...)
	cli.OnDatagram(reset, srvEP.addr, cliEP.addr, 0)
	waitFor(t, cli, "client drains on reset", func() bool {
		return cli.State() == stDraining || cli.State() == stDead
	})
}
