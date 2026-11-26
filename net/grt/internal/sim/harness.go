package sim

import (
	"context"
	"errors"
	mrand "math/rand/v2"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/engine"
	"gosuda.org/grt/internal/wire"
)

// DefaultAddresses used in simulated test topologies.
var (
	ServerAddr = netip.MustParseAddrPort("10.0.0.1:4433")
	ClientAddr = netip.MustParseAddrPort("10.0.0.2:5000")
	AltNATAddr = netip.MustParseAddrPort("192.168.1.100:6000")
)

// SimPair encapsulates a simulated client-server connection pair over SimNet.
type SimPair struct {
	Net        *SimNet
	ClientEP   *SimEndpoint
	ServerEP   *SimEndpoint
	Client     *engine.Conn
	Server     *engine.Conn
	Listener   *engine.Listener
	Invariants *InvariantTracker
	cliPumping atomic.Bool
	srvPumping atomic.Bool
	pumpWG     sync.WaitGroup
}

// SimIdentity holds cryptographic credentials for simulation tests.
type SimIdentity struct {
	ServerCert *engine.ServerIdentity
	ClientKey  gc.X25519KeyPair
}

// NewDeterministicIdentity creates a 100% deterministic identity derived from seed.
func NewDeterministicIdentity(seed uint64) *SimIdentity {
	r := mrand.New(mrand.NewPCG(seed, seed^0x5555aaaa))
	var privS, privC [32]byte
	for i := range privS {
		privS[i] = byte(r.Uint32())
		privC[i] = byte(r.Uint32())
	}
	kpS, _ := gc.X25519KeyPairFromPriv(privS[:])
	kpC, _ := gc.X25519KeyPairFromPriv(privC[:])
	cert := &engine.ServerIdentity{
		IdentityPub:  kpS.Pub,
		IdentityPriv: kpS.Priv[:],
		ServiceHash:  gc.ServiceHash("srv.example:4433", [16]byte{1}),
	}
	return &SimIdentity{
		ServerCert: cert,
		ClientKey:  kpC,
	}
}

// NewSimIdentity generates deterministic test keys from seed 42.
func NewSimIdentity(t testing.TB) *SimIdentity {
	return NewDeterministicIdentity(42)
}

// DeterministicPayload generates pseudo-random payload bytes deterministically from seed.
func DeterministicPayload(seed uint64, size int) []byte {
	r := mrand.New(mrand.NewPCG(seed, seed^0xdeadbeef))
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	return data
}

// DefaultSimConfigs generates aligned client and server configs.
func DefaultSimConfigs(id *SimIdentity) (*engine.Config, *engine.Config) {
	return DefaultSimConfigsWithSeed(id, 42)
}

// DefaultSimConfigsWithSeed generates aligned client and server configs with deterministic seed.
func DefaultSimConfigsWithSeed(id *SimIdentity, seed uint64) (*engine.Config, *engine.Config) {
	p := wire.DefaultParams()
	p.InitialMaxData = 1 << 22      // 4MB
	p.InitialStreamWindow = 1 << 20 // 1MB
	p.InitialBidiSlots = 128
	p.InitialUniSlots = 128
	p.MaxParkDurationMS = 600000

	ccfg := &engine.Config{
		Params:      p,
		ServerName:  "srv.example:4433",
		AppID:       [16]byte{1},
		AuthMode:    0,
		ExpectedKey: id.ServerCert.IdentityPub,
		NowUnix:     func() int64 { return 1773998400 },
		MaxLiveSent: 8192,
	}

	scfg := &engine.Config{
		Params:      p,
		ServerName:  "srv.example:4433",
		AppID:       [16]byte{1},
		ServerCert:  id.ServerCert,
		NowUnix:     func() int64 { return 1773998400 },
		MaxLiveSent: 8192,
	}
	r := mrand.New(mrand.NewPCG(seed^0x98765432, seed^0x12345678))
	for i := range scfg.TicketKey {
		scfg.TicketKey[i] = byte(r.Uint32())
	}
	var issueCtr uint64
	scfg.TicketIssuer = func() ([16]byte, uint64) {
		issueCtr++
		return [16]byte{1, 2, 3}, issueCtr
	}
	scfg.ReplayLedger = &testReplayLedger{m: make(map[[32]byte]bool)}

	return ccfg, scfg
}

type testReplayLedger struct {
	m map[[32]byte]bool
}

func (l *testReplayLedger) ConsumeEarly(domain, ticketID, attempt [16]byte, flightHash [32]byte) bool {
	var k [32]byte
	copy(k[:16], domain[:])
	copy(k[16:], attempt[:])
	if l.m[k] {
		return false
	}
	l.m[k] = true
	return true
}

// NewSimPair initializes and starts a client-server pair over SimNet.
func NewSimPair(t testing.TB, netCfg NetConfig, ccfg *engine.Config, scfg *engine.Config, id *SimIdentity) *SimPair {
	t.Helper()
	net := NewSimNet(netCfg)
	srvEP := net.Endpoint(ServerAddr)
	cliEP := net.Endpoint(ClientAddr)

	l := engine.NewListener(scfg, srvEP, []byte("sim-domain"))

	pair := &SimPair{
		Net:        net,
		ClientEP:   cliEP,
		ServerEP:   srvEP,
		Listener:   l,
		Invariants: NewInvariantTracker(),
	}

	// Pump server datagrams
	pair.srvPumping.Store(true)
	pair.pumpWG.Add(1)
	go func() {
		defer pair.pumpWG.Done()
		buf := make([]byte, 64*1024)
		for {
			n, src, dst, ecn, err := srvEP.Recv(buf)
			if err != nil {
				return
			}
			pkt := make([]byte, n)
			copy(pkt, buf[:n])
			l.OnDatagram(pkt, src, dst, ecn)
		}
	}()

	// Dial client
	var cliC atomic.Pointer[engine.Conn]
	pair.cliPumping.Store(true)
	pair.pumpWG.Add(1)
	go func() {
		defer pair.pumpWG.Done()
		buf := make([]byte, 64*1024)
		for {
			n, src, dst, ecn, err := cliEP.Recv(buf)
			if err != nil {
				return
			}
			pkt := make([]byte, n)
			copy(pkt, buf[:n])
			var cn *engine.Conn
			for cn == nil {
				cn = cliC.Load()
			}
			cn.OnDatagram(pkt, src, dst, ecn)
		}
	}()

	cli := engine.Dial(ccfg, cliEP, ServerAddr, &engine.ClientIdentity{Priv: id.ClientKey.Priv[:]})
	cliC.Store(cli)
	pair.Client = cli

	return pair
}

// WaitHandshake blocks until both client and server complete the handshake.
func (p *SimPair) WaitHandshake(t testing.TB, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Wait for server to accept
	select {
	case srvConn := <-p.Listener.AcceptCh():
		p.Server = srvConn
	case <-ctx.Done():
		t.Fatalf("sim: handshake timed out waiting for server accept (client state=%s)", p.Client.State())
	}

	// Wait for client to reach CONFIRMED / SECURE_READY
	p.Client.WaitConfirmed(ctx)
	if err := ctx.Err(); err != nil {
		t.Fatalf("sim: handshake timed out waiting for client confirmation: %v", err)
	}

	// Wait for server to reach ESTABLISHED / CONFIRMED
	p.Server.WaitConfirmed(ctx)
	if err := ctx.Err(); err != nil {
		t.Fatalf("sim: handshake timed out waiting for server confirmation: %v", err)
	}

	// Verify cipher determinism
	p.Invariants.RecordCipherAgreement(p.Client.Cipher(), p.Server.Cipher())
}

// TransferBidirectional streams data in both directions and verifies byte-for-byte integrity.
func (p *SimPair) TransferBidirectional(t testing.TB, c2sPayload, s2cPayload []byte, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 1. Client opens a stream
	sid, err := p.Client.OpenStream()
	if err != nil {
		t.Fatalf("sim: client OpenStream failed: %v", err)
	}

	sr := p.Invariants.StreamRecorderFor(sid)
	sr.RecordSent(c2sPayload)

	// 2. Client writes payload and FIN
	go func() {
		written := 0
		for written < len(c2sPayload) {
			chunk := len(c2sPayload) - written
			if chunk > 4096 {
				chunk = 4096
			}
			n, err := p.Client.Write(sid, c2sPayload[written:written+chunk])
			written += n
			if err == engine.ErrWouldBlock {
				p.Client.WaitWritable(ctx)
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if err != nil {
				return
			}
		}
		_ = p.Client.Fin(sid)
	}()

	// 3. Server accepts stream and reads c2sPayload
	var srvSid uint64
	for {
		if s, ok := p.Server.AcceptStream(); ok {
			srvSid = s
			break
		}
		p.Server.WaitReadable(ctx)
		if ctx.Err() != nil {
			t.Fatalf("sim: timeout waiting for server to accept stream")
		}
	}

	// Server echoes back s2cPayload on the same stream
	srEcho := p.Invariants.StreamRecorderFor(srvSid ^ 0x01000000) // distinct tracker ID
	srEcho.RecordSent(s2cPayload)
	go func() {
		written := 0
		for written < len(s2cPayload) {
			chunk := len(s2cPayload) - written
			if chunk > 4096 {
				chunk = 4096
			}
			n, err := p.Server.Write(srvSid, s2cPayload[written:written+chunk])
			written += n
			if err == engine.ErrWouldBlock {
				p.Server.WaitWritable(ctx)
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if err != nil {
				return
			}
		}
		_ = p.Server.Fin(srvSid)
	}()

	// Read loops for both c2s and s2c concurrently
	var srvRecv, cliRecv []byte
	var srvErr, cliErr error
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := p.Server.Read(srvSid, buf)
			if n > 0 {
				srvRecv = append(srvRecv, buf[:n]...)
			}
			if errors.Is(err, engine.ErrStreamEOF) {
				break
			}
			if err != nil {
				srvErr = err
				return
			}
			if n == 0 {
				p.Server.WaitReadable(ctx)
				if ctx.Err() != nil {
					srvErr = ctx.Err()
					return
				}
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := p.Client.Read(sid, buf)
			if n > 0 {
				cliRecv = append(cliRecv, buf[:n]...)
			}
			if errors.Is(err, engine.ErrStreamEOF) {
				break
			}
			if err != nil {
				cliErr = err
				return
			}
			if n == 0 {
				p.Client.WaitReadable(ctx)
				if ctx.Err() != nil {
					cliErr = ctx.Err()
					return
				}
			}
		}
	}()

	wg.Wait()

	if srvErr != nil || cliErr != nil {
		stats := p.Net.Stats()
		t.Logf("DIAG: srvRecv=%d/%d, cliRecv=%d/%d, srvErr=%v, cliErr=%v",
			len(srvRecv), len(c2sPayload), len(cliRecv), len(s2cPayload), srvErr, cliErr)
		t.Logf("DIAG: netStats: Delivered=%d Dropped=%d ECNMarked=%d Reordered=%d",
			stats.Delivered.Load(), stats.Dropped.Load(), stats.ECNMarked.Load(), stats.Reordered.Load())
		t.Logf("DIAG: cliState=%v cliDebug=[%s]",
			p.Client.State(), p.Client.DebugString())
		t.Logf("DIAG: srvState=%v srvDebug=[%s]",
			p.Server.State(), p.Server.DebugString())
	}

	if srvErr != nil {
		t.Fatalf("sim: server read failed: %v (received %d / %d bytes)", srvErr, len(srvRecv), len(c2sPayload))
	}
	if cliErr != nil {
		t.Fatalf("sim: client read failed: %v (received %d / %d bytes)", cliErr, len(cliRecv), len(s2cPayload))
	}

	sr.RecordReceived(srvRecv)
	srEcho.RecordReceived(cliRecv)

	// Verify invariant assertions
	sr.Verify(t, sid)
	srEcho.Verify(t, srvSid^0x01000000)
}

// Close tears down endpoints and connections.
func (p *SimPair) Close() {
	if p.Net != nil {
		p.Net.Close()
	}
	p.pumpWG.Wait()
	if p.Client != nil {
		p.Client.Close(0)
	}
	if p.Server != nil {
		p.Server.Close(0)
	}
	if p.Listener != nil {
		p.Listener.Close()
	}
}
