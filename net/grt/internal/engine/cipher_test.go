package engine

import (
	"context"
	"crypto/rand"
	"net/netip"
	"testing"
	"time"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

func TestCipherNegotiation_Default(t *testing.T) {
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	ccfg, scfg := testConfigs(t)

	l := NewListener(scfg, srvEP, []byte("test-domain"))
	go func() {
		for d := range srvEP.ch {
			l.OnDatagram(d.b, d.src, d.dst, d.ecn)
		}
	}()

	cli := dialClientCfg(t, cliEP, srvEP.addr, ccfg)
	defer cli.Close(0)

	waitFor(t, cli, "client confirmed", func() bool {
		return cli.State() == stEstablished
	})

	if cli.Cipher() != wire.CipherAES256GCM {
		t.Fatalf("expected AES256GCM, got %d", cli.Cipher())
	}
}

func TestCipherNegotiation_Default_DisableAES(t *testing.T) {
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	ccfg, scfg := testConfigs(t)

	// Client disables AES; should negotiate XChaCha20Poly1305
	ccfg.DisableAES = true

	l := NewListener(scfg, srvEP, []byte("test-domain"))
	go func() {
		for d := range srvEP.ch {
			l.OnDatagram(d.b, d.src, d.dst, d.ecn)
		}
	}()

	cli := dialClientCfg(t, cliEP, srvEP.addr, ccfg)
	defer cli.Close(0)

	waitFor(t, cli, "client confirmed", func() bool {
		return cli.State() == stEstablished
	})

	if cli.Cipher() != wire.CipherXChaCha20Poly1305 {
		t.Fatalf("expected XChaCha20Poly1305, got %d", cli.Cipher())
	}
}

func TestCipherNegotiation_ClientPrefersXChaCha(t *testing.T) {
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	ccfg, scfg := testConfigs(t)

	// Client explicitly prefers XChaCha over AES
	ccfg.CipherPreference = []uint16{wire.CipherXChaCha20Poly1305, wire.CipherAES256GCM}

	l := NewListener(scfg, srvEP, []byte("test-domain"))
	go func() {
		for d := range srvEP.ch {
			l.OnDatagram(d.b, d.src, d.dst, d.ecn)
		}
	}()

	cli := dialClientCfg(t, cliEP, srvEP.addr, ccfg)
	defer cli.Close(0)

	waitFor(t, cli, "client confirmed", func() bool {
		return cli.State() == stEstablished
	})

	if cli.Cipher() != wire.CipherXChaCha20Poly1305 {
		t.Fatalf("expected XChaCha20Poly1305, got %d", cli.Cipher())
	}
}

func TestCipherNegotiation_ClientPrefersAES(t *testing.T) {
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	ccfg, scfg := testConfigs(t)

	// Client explicitly prefers AES over XChaCha
	ccfg.CipherPreference = []uint16{wire.CipherAES256GCM, wire.CipherXChaCha20Poly1305}

	l := NewListener(scfg, srvEP, []byte("test-domain"))
	go func() {
		for d := range srvEP.ch {
			l.OnDatagram(d.b, d.src, d.dst, d.ecn)
		}
	}()

	cli := dialClientCfg(t, cliEP, srvEP.addr, ccfg)
	defer cli.Close(0)

	waitFor(t, cli, "client confirmed", func() bool {
		return cli.State() == stEstablished
	})

	if cli.Cipher() != wire.CipherAES256GCM {
		t.Fatalf("expected AES256GCM, got %d", cli.Cipher())
	}
}

func TestCipherNegotiation_ClientPrefersAES_ServerDisablesAES(t *testing.T) {
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	ccfg, scfg := testConfigs(t)

	// Client prefers AES, but server disables AES
	ccfg.CipherPreference = []uint16{wire.CipherAES256GCM, wire.CipherXChaCha20Poly1305}
	scfg.DisableAES = true

	l := NewListener(scfg, srvEP, []byte("test-domain"))
	go func() {
		for d := range srvEP.ch {
			l.OnDatagram(d.b, d.src, d.dst, d.ecn)
		}
	}()

	cli := dialClientCfg(t, cliEP, srvEP.addr, ccfg)
	defer cli.Close(0)

	waitFor(t, cli, "client confirmed", func() bool {
		return cli.State() == stEstablished
	})

	if cli.Cipher() != wire.CipherXChaCha20Poly1305 {
		t.Fatalf("expected XChaCha20Poly1305 fallback, got %d", cli.Cipher())
	}
}

func TestCipherNegotiation_NoMatchingCipher(t *testing.T) {
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	ccfg, scfg := testConfigs(t)

	// Client only offers AES, server disables AES
	ccfg.CipherPreference = []uint16{wire.CipherAES256GCM}
	scfg.DisableAES = true

	l := NewListener(scfg, srvEP, []byte("test-domain"))
	go func() {
		for d := range srvEP.ch {
			l.OnDatagram(d.b, d.src, d.dst, d.ecn)
		}
	}()

	cli := dialClientCfg(t, cliEP, srvEP.addr, ccfg)
	defer cli.Close(0)

	// Client should fail to establish (server rejects with no matching cipher, client times out or errors)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	cli.WaitSecureReady(ctx)

	if cli.State() == stEstablished {
		t.Fatal("handshake should have failed when no ciphers match")
	}
}

func TestCipherNegotiation_DeterministicSelectionViolationRejection(t *testing.T) {
	// Unit test: Client offers [AES, XChaCha]. Server claims mask=3 (both supported),
	// but server selects XChaCha (violating rule that first client offer present in mask must be chosen).
	sock := &stubSock{addr: netip.MustParseAddrPort("10.0.0.9:9")}
	c := unitConn(t, sock)
	core := c.core
	core.role = roleClient
	core.state = stCWaitReply
	ccfg := &Config{
		Params:           wire.DefaultParams(),
		ServerName:       "srv.example:4433",
		AppID:            [16]byte{1},
		AuthMode:         0,
		NowUnix:          func() int64 { return time.Now().Unix() },
		MaxLiveSent:      4096,
		CipherPreference: []uint16{wire.CipherAES256GCM, wire.CipherXChaCha20Poly1305},
	}
	core.cfg = ccfg
	core.hs.offer = [wire.MaxCipherOffers]uint16{wire.CipherAES256GCM, wire.CipherXChaCha20Poly1305}

	srv := sharedTestIdentity(t)
	ccfg.ExpectedKey = srv.IdentityPub
	var attemptID [wire.AttemptIDBytes]byte
	rand.Read(attemptID[:])
	msg, _, spk, pqpk := buildClientAttempt(t, ccfg, attemptID, 0xDC1D)
	core.hs.clientMsg = msg
	core.hs.flightHash = wire.ClientFlightHash(msg)
	core.hs.attemptID = attemptID
	core.localCID = cidRec{cid: 0xC1}
	core.peerCID = cidRec{cid: 0xDC1D}
	core.hs.retryDCID = 0xDC1D

	// Server returns SelectedCipher = XChaCha despite mask = 3 (which has AES bit 1 set, and AES was client's first offer)
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
	dh1, _ := gc.X25519(ikS.Priv, spk.Pub[:])
	dh2, _ := gc.X25519(ekS.Priv, core.hs.ikC.Pub[:])
	dh3, _ := gc.X25519(ekS.Priv, spk.Pub[:])
	sk := gc.PQXDHKDF(dh1, dh2, dh3, ss)

	var sm wire.ServerMessage
	sm.SelectedCipher = wire.CipherXChaCha20Poly1305 // VIOLATION! Should be AES
	sm.ServerCipherMask = 3                          // Both supported
	sm.AttemptID = attemptID
	sm.ClientFlightHash = core.hs.flightHash
	rand.Read(sm.ServerNonce[:])
	sm.ClientInitialCID = 0xC1
	sm.ServerInitialCID = 0x5E
	sm.OriginalDCID = 0xDC1D
	sm.AcceptedRoutingCID = 0xDC1D
	sm.AcceptedTokenHash = [32]byte{}
	sm.IKS = ikS.Pub
	sm.EKS = ekS.Pub
	sm.CTS = cts
	sm.Params = wire.DefaultParams()
	sm.Credentials = srv.Chain
	sb := wire.EncodeServerBody(&sm)
	sm.EncryptedBodyLength = uint32(len(sb) + 32 + wire.TagBytes)
	sp := wire.EncodeServerPrefix(&sm)

	hs := gc.DeriveServerFlight(msg, sp, sk, [32]byte{}, false, wire.CipherXChaCha20Poly1305)
	fin := gc.ServerFinishedMAC(hs.SfKey, hs.T0, sb)
	plain := append(sb, fin[:]...)
	ct, err := gc.SealOnce(wire.CipherXChaCha20Poly1305,
		hs.HsKey[:], hs.HsIV[:wire.NonceLen(wire.CipherXChaCha20Poly1305)], plain, sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	serverFlight := append(sp, ct...)

	c.verifyServerFlight(serverFlight)
	if core.state != stCWaitReply {
		t.Fatalf("client should have rejected violation of deterministic cipher selection rule; state = %s", core.state)
	}
}
