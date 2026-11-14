package engine

import (
	"crypto/rand"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// Section 20.2 rule 5 / 20.3: APP_CLOSE enters CLOSING only from a live
// key-bearing state. A draining endpoint must stay silent, and neither
// CLOSING nor DRAINING may have its deadline or close code changed by a
// later application close.
func TestAppCloseDoesNotReenterClosing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		enter func(c *Conn)
	}{
		{"draining", func(c *Conn) { c.enterDraining() }},
		{"closing", func(c *Conn) { c.closeWith(codeProtocolViolation, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := &stubSock{addr: netip.MustParseAddrPort("127.0.0.1:9000")}
			c := unitConn(t, sock)
			core := c.core
			tc.enter(c)
			wantState, wantCode := core.state, core.closeCode
			wantDeadline := core.deadline[tkClose]
			if !core.armed[tkClose] {
				t.Fatalf("%s: close deadline not armed", core.state)
			}

			c.appCommand(&appCmd{kind: acClose, code: 99, res: make(chan appResult, 1)})

			if core.state != wantState {
				t.Fatalf("APP_CLOSE moved %s to %s", wantState, core.state)
			}
			if core.closeCode != wantCode {
				t.Fatalf("APP_CLOSE overwrote close code %d with %d", wantCode, core.closeCode)
			}
			if core.deadline[tkClose] != wantDeadline {
				t.Fatalf("APP_CLOSE extended the close deadline")
			}
			if wantState == stDraining {
				sent := len(sock.sent)
				c.txSchedule()
				if len(sock.sent) != sent {
					t.Fatalf("DRAINING emitted %d packets", len(sock.sent)-sent)
				}
			}
		})
	}
}

// APP_CLOSE from ESTABLISHED still enters CLOSING with the caller's code.
func TestAppCloseFromEstablished(t *testing.T) {
	c := unitConn(t, &stubSock{addr: netip.MustParseAddrPort("127.0.0.1:9000")})
	c.appCommand(&appCmd{kind: acClose, code: 5, res: make(chan appResult, 1)})
	if c.core.state != stClosing || c.core.closeCode != 5 || !c.core.armed[tkClose] {
		t.Fatalf("state=%s code=%d armed=%v, want CLOSING/5/armed",
			c.core.state, c.core.closeCode, c.core.armed[tkClose])
	}
}

// Config.NowUnix == nil falls back to time.Now().Unix() so ticket and
// credential validity windows are enforced against the system wall clock.
func TestNowUnixNilUsesSystemClock(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("127.0.0.1:9000")}
	c := unitConn(t, sock)
	c.core.cfg.NowUnix = nil
	c.core.cfg.ServerName = "example.com:4433"
	c.core.cfg.AppID = [16]byte{1}
	rand.Read(c.core.cfg.TicketKey[:])
	rand.Read(c.core.resumptionMaster[:])
	c.core.cfg.TicketIssuer = func() ([16]byte, uint64) {
		return [16]byte{2}, 1
	}

	c.issueTicket()
	var body []byte
	for i := range c.core.ctrl {
		if c.core.ctrl[i].used && c.core.ctrl[i].kind == ctrlNewTicket {
			body = c.core.ctrl[i].body
			break
		}
	}
	if len(body) != wire.TicketMetaBytes+wire.TicketBytes {
		t.Fatalf("expected queued ticket of %d bytes, got %d", wire.TicketMetaBytes+wire.TicketBytes, len(body))
	}
	tp, err := c.openTicket(body[wire.TicketMetaBytes:])
	if err != nil {
		t.Fatalf("openTicket with nil NowUnix: %v", err)
	}
	now := uint64(time.Now().Unix())
	if tp.IssuedUnixS < now-5 || tp.IssuedUnixS > now+5 {
		t.Fatalf("IssuedUnixS = %d, want near %d", tp.IssuedUnixS, now)
	}

	// Verify AuthChain credential check also uses the system clock when NowUnix == nil.
	root, err := gc.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := gc.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	svc := c.serviceHash()
	makeCred := func(notBefore, notAfter uint64) [wire.CredentialBytes]byte {
		var b [wire.CredentialBytes]byte
		binary.BigEndian.PutUint16(b[0:], 1)
		binary.BigEndian.PutUint16(b[2:], 1)
		copy(b[4:], root.Pub[:])
		copy(b[36:], leaf.Pub[:])
		copy(b[68:], svc[:])
		binary.BigEndian.PutUint64(b[100:], notBefore)
		binary.BigEndian.PutUint64(b[108:], notAfter)
		sig, err := gc.XEdDSASign(root.Priv[:], gc.CredentialMessage(b[:wire.CredentialPrefix]), make([]byte, 64))
		if err != nil {
			t.Fatal(err)
		}
		copy(b[132:], sig[:])
		return b
	}
	liveCred := makeCred(now-60, now+3600)
	expiredCred := makeCred(now-7200, now-60)
	c.core.cfg.AuthMode = 1
	c.core.cfg.TrustAnchors = [][32]byte{root.Pub}
	if !c.checkServerIdentity(&wire.ServerMessage{IKS: leaf.Pub, Credentials: [][wire.CredentialBytes]byte{liveCred}}) {
		t.Fatal("expected live credential chain to verify with nil NowUnix")
	}
	if c.checkServerIdentity(&wire.ServerMessage{IKS: leaf.Pub, Credentials: [][wire.CredentialBytes]byte{expiredCred}}) {
		t.Fatal("expected expired credential chain to fail with nil NowUnix")
	}
}

// When the listener's accept channel is full, admitAccepted drops the
// connection before registering it in l.conns so unaccepted connections
// cannot linger as zombies.
func TestAcceptQueueBackpressurePreventsZombieConns(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("127.0.0.1:9000")}
	l := NewListener(&Config{Params: wire.DefaultParams()}, sock, []byte("dom"))
	l.accept = make(chan *Conn, 1)

	c1 := unitConn(t, sock)
	c1.core.localCID = cidRec{cid: 101, valid: true}
	if !l.admitAccepted(c1) {
		t.Fatal("expected first admitAccepted to succeed")
	}

	c2 := unitConn(t, sock)
	c2.core.localCID = cidRec{cid: 202, valid: true}
	if l.admitAccepted(c2) {
		t.Fatal("expected second admitAccepted to fail when accept queue is full")
	}
	if _, ok := l.conns[202]; ok {
		t.Fatal("rejected connection was registered in l.conns")
	}
	if l.Stats().AdmissionDrops != 1 {
		t.Fatalf("AdmissionDrops = %d, want 1", l.Stats().AdmissionDrops)
	}
}

// When ALLOW_TOKENLESS capacity is saturated or an invalid Retry token arrives
// on fragment 0, the listener falls back to a stateless RETRY instead of
// silently dropping the client flight.
func TestTokenlessSaturationAndInvalidRetryTokenEmitStatelessRetry(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("127.0.0.1:9000")}
	l := NewListener(&Config{
		Params:        wire.DefaultParams(),
		AdmissionMode: AdmissionTokenless,
	}, sock, []byte("dom"))

	// Saturate tokenless spool occupancy.
	l.tokenless = maxTokenlessSlots

	cliAddr := netip.MustParseAddrPort("192.0.2.10:50000")
	srvAddr := sock.addr
	var attemptID [wire.AttemptIDBytes]byte
	attemptID[0] = 0x42
	msg := make([]byte, 128)
	msg[0] = wire.MsgClient
	msg[1] = wire.Version
	msg[4] = 1
	msg[5] = 0
	copy(msg[12:28], attemptID[:])
	binary.BigEndian.PutUint64(msg[60:68], 0x1122334455667788) // ClientInitialCID
	hash := wire.ClientFlightHash(msg)
	frags := wire.BuildFragments(msg, wire.ClassClientFlight, 0x9988776655443322, attemptID, [wire.TokenBytes]byte{})
	h, ok := wire.ParseHeader(frags[0])
	if !ok {
		t.Fatal("ParseHeader failed")
	}
	l.onClientFlight(frags[0], h, cliAddr, srvAddr)
	if len(sock.sent) != 1 {
		t.Fatalf("expected 1 stateless RETRY when tokenless is saturated, got %d", len(sock.sent))
	}

	// Now present a syntactically valid TokenKindRetry with a bad HMAC tag on fragment 0.
	var badTok wire.Token
	badTok.Kind = wire.TokenKindRetry
	badTok.IssuedUnixS = uint64(time.Now().Unix())
	badTok.ExpiresUnixS = badTok.IssuedUnixS + wire.RetryTokenLifetimeS
	badTok.Binding = 0x9988776655443322
	badTok.Tag[0] = 0xff
	var badTokBytes [wire.TokenBytes]byte
	wire.EncodeToken(badTokBytes[:], &badTok)

	attemptID[0] = 0x43
	copy(msg[12:28], attemptID[:])
	_ = hash
	frags2 := wire.BuildFragments(msg, wire.ClassClientFlight, 0x9988776655443322, attemptID, badTokBytes)
	h2, ok := wire.ParseHeader(frags2[0])
	if !ok {
		t.Fatal("ParseHeader failed")
	}
	l.onClientFlight(frags2[0], h2, cliAddr, srvAddr)
	if len(sock.sent) != 2 {
		t.Fatalf("expected stateless RETRY on invalid Retry token on frag 0, got %d sent", len(sock.sent))
	}
}

// Peer STREAM_OPEN leaves send.buf nil until appWrite, and ackStreamRange
// releases send.buf when the send half reaches SEND_DONE.
func TestPeerStreamOpenAllocatesSendBufLazily(t *testing.T) {
	sock := &stubSock{addr: netip.MustParseAddrPort("127.0.0.1:9000")}
	c := unitConn(t, sock)
	c.core.local.InitialBidiSlots = 4
	c.core.local.InitialStreamWindow = 65536
	c.core.peer.InitialStreamWindow = 65536
	c.core.slots.localLimit[classIdx(false, c.peerInitiator())] = 4

	sid := wire.MakeStreamID(0, 0, false, c.peerInitiator())
	c.applyOpen(sid)
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.send == nil {
		t.Fatal("expected bidi slot with sendHalf")
	}
	if s.send.buf != nil {
		t.Fatalf("expected nil send.buf before first appWrite, got cap=%d", cap(s.send.buf))
	}

	if _, err := c.appWrite(uint64(sid), []byte("hello")); err != nil {
		t.Fatalf("appWrite: %v", err)
	}
	if cap(s.send.buf) == 0 {
		t.Fatal("expected send.buf allocated on appWrite")
	}
	if err := c.appFin(uint64(sid)); err != nil {
		t.Fatalf("appFin: %v", err)
	}
	c.ackStreamRange(sid, 0, 5, wire.FrameFlagStreamFin)
	if s.send.state != sendDone || s.send.buf != nil {
		t.Fatalf("state=%v bufCap=%d, want sendDone and nil buf", s.send.state, cap(s.send.buf))
	}
}
