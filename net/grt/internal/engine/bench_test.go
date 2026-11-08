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

func BenchmarkRecvHalfSequential(b *testing.B) {
	const chunkSize = 1200
	const totalBytes = 1024 * 1024 // 1 MB
	data := make([]byte, chunkSize)
	for i := range data {
		data[i] = byte(i)
	}
	readBuf := make([]byte, 4096)

	b.SetBytes(totalBytes)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		r := &recvHalf{
			state: recvOpen,
			limit: 1 << 30,
		}
		for off := uint64(0); off < totalBytes; off += chunkSize {
			n := chunkSize
			if off+uint64(n) > totalBytes {
				n = int(totalBytes - off)
			}
			if !r.insert(off, data[:n], nil) {
				b.Fatalf("insert failed at %d", off)
			}
			// Simulate application reading data as it arrives.
			for r.read(readBuf) > 0 {
			}
		}
		if r.base != totalBytes {
			b.Fatalf("base = %d, want %d", r.base, totalBytes)
		}
	}
}

func BenchmarkRecvHalfAppendOnly(b *testing.B) {
	const chunkSize = 1200
	const totalBytes = 256 * 1024 // 256 KB
	data := make([]byte, chunkSize)
	for i := range data {
		data[i] = byte(i)
	}

	b.SetBytes(totalBytes)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		r := &recvHalf{
			state: recvOpen,
			limit: 1 << 30,
		}
		for off := uint64(0); off < totalBytes; off += chunkSize {
			n := chunkSize
			if off+uint64(n) > totalBytes {
				n = int(totalBytes - off)
			}
			if !r.insert(off, data[:n], nil) {
				b.Fatalf("insert failed at %d", off)
			}
		}
	}
}

func BenchmarkSendHalfAdvanceBase(b *testing.B) {
	const chunkSize = 1200
	const totalBytes = 1024 * 1024 // 1 MB
	payload := make([]byte, totalBytes)

	b.SetBytes(totalBytes)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		sh := &sendHalf{
			state:   sendOpen,
			buf:     append([]byte(nil), payload...),
			bufBase: 0,
			end:     totalBytes,
		}
		sh.unacked.add(0, totalBytes)

		for off := uint64(0); off < totalBytes; off += chunkSize {
			end := off + chunkSize
			if end > totalBytes {
				end = totalBytes
			}
			sh.unacked.remove(off, end)
			sh.advanceBase()
		}
	}
}

func BenchmarkReplayClearBelow(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		w := replayWin{width: wire.MaxReplayWindow}
		w.ensure()
		w.init = true
		w.largest = 1000
		for pn := uint64(1000); pn < 10000; pn += 50 {
			w.clearBelow(pn)
		}
	}
}

func BenchmarkReplayAckRanges(b *testing.B) {
	w := replayWin{width: wire.MaxReplayWindow}
	w.ensure()
	for i := uint64(1); i <= 2000; i++ {
		if i%3 != 0 {
			w.commit(i)
		}
	}
	var ranges [wire.MaxAckRanges][2]uint64
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w.ackRanges(&ranges, 8)
	}
}

func benchConfigs(b *testing.B, p wire.Params) (*Config, *Config) {
	b.Helper()
	kp, err := gc.GenerateX25519()
	if err != nil {
		b.Fatal(err)
	}
	srv := &ServerIdentity{
		IdentityPub:  kp.Pub,
		IdentityPriv: kp.Priv[:],
		ServiceHash:  gc.ServiceHash("srv.example:4433", [16]byte{1}),
	}
	ccfg := &Config{
		Params:      p,
		ServerName:  "srv.example:4433",
		AppID:       [16]byte{1},
		AuthMode:    0,
		ExpectedKey: srv.IdentityPub,
		NowUnix:     func() int64 { return 1000000 },
		MaxLiveSent: 65536,
	}
	scfg := &Config{
		Params:      p,
		ServerName:  "srv.example:4433",
		AppID:       [16]byte{1},
		ServerCert:  srv,
		NowUnix:     func() int64 { return 1000000 },
		MaxLiveSent: 65536,
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

// BenchmarkE2EStreamThroughput measures the steady-state stream path over the
// in-memory PacketConn network: one persistent connection, one stream, and
// timed bulk transfers. Handshake, stream open, PMTU convergence, and pool
// warmup all happen before the timer starts.
func BenchmarkE2EStreamThroughput(b *testing.B) {
	const streamBytes = 256 * 1024 // 256 KB per iteration
	payload := make([]byte, streamBytes)
	for i := range payload {
		payload[i] = byte(i)
	}

	b.SetBytes(streamBytes)
	b.ReportAllocs()

	for _, tc := range []struct {
		name   string
		params func() wire.Params
	}{
		{"default", func() wire.Params {
			p := wire.DefaultParams()
			p.InitialMaxData = 16 << 20
			p.InitialStreamWindow = 16 << 20
			p.MaximumBidiSlots = 100
			return p
		}},
		{"high-throughput", wire.HighThroughputParams},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchE2EThroughput(b, tc.params(), payload)
		})
	}
}

func benchE2EThroughput(b *testing.B, p wire.Params, payload []byte) {
	b.Helper()
	n := newTestNet()
	srvEP := n.endpoint(netip.MustParseAddrPort("10.0.0.1:4433"))
	cliEP := n.endpoint(netip.MustParseAddrPort("10.0.0.2:5000"))
	ccfg, scfg := benchConfigs(b, p)

	l := NewListener(scfg, srvEP, []byte("test-domain"))
	stop := make(chan struct{})
	go func() {
		var batch []Datagram
		for {
			select {
			case d := <-srvEP.ch:
				batch = append(batch[:0], Datagram{Data: d.b, Src: d.src, Dst: d.dst, ECN: d.ecn})
				// Sweep whatever else already queued so the conn takes
				// one mailbox admission for the whole burst.
			sweep:
				for len(batch) < 64 {
					select {
					case d := <-srvEP.ch:
						batch = append(batch, Datagram{Data: d.b, Src: d.src, Dst: d.dst, ECN: d.ecn})
					default:
						break sweep
					}
				}
				l.OnDatagrams(batch)
			case <-stop:
				return
			}
		}
	}()

	id, err := gc.GenerateX25519()
	if err != nil {
		b.Fatal(err)
	}
	cliC := Dial(ccfg, cliEP, srvEP.LocalAddr(), &ClientIdentity{Priv: id.Priv[:]})
	go func() {
		var batch []Datagram
		for {
			select {
			case d := <-cliEP.ch:
				batch = append(batch[:0], Datagram{Data: d.b, Src: d.src, Dst: d.dst, ECN: d.ecn})
			sweep:
				for len(batch) < 64 {
					select {
					case d := <-cliEP.ch:
						batch = append(batch, Datagram{Data: d.b, Src: d.src, Dst: d.dst, ECN: d.ecn})
					default:
						break sweep
					}
				}
				cliC.OnDatagrams(batch)
			case <-stop:
				return
			}
		}
	}()

	// Wait for handshake
	for cliC.State() != stEstablished {
		select {
		case <-cliC.confirmed:
		case <-cliC.stateWake:
		case <-cliC.done:
			b.Fatalf("cli died: %v", cliC.TermError())
		}
	}

	sConn := l.Accept()
	if sConn == nil {
		b.Fatal("server accept returned nil")
	}

	sid, err := cliC.OpenStream()
	if err != nil {
		b.Fatalf("open stream: %v", err)
	}

	var srvSID uint64
	writeAll := func() chan error {
		errCh := make(chan error, 1)
		go func() {
			for off := 0; off < len(payload); {
				nw, werr := cliC.Write(sid, payload[off:])
				if werr == errWouldBlock {
					cliC.WaitWritable(context.Background())
					continue
				}
				if werr != nil {
					errCh <- werr
					return
				}
				off += nw
			}
			errCh <- nil
		}()
		return errCh
	}
	recvBuf := make([]byte, 64*1024)
	watchdog := time.NewTimer(5 * time.Second)
	defer watchdog.Stop()
	readAll := func() {
		for read := 0; read < len(payload); {
			nr, rerr := sConn.Read(srvSID, recvBuf)
			if rerr != nil {
				b.Fatalf("read: %v", rerr)
			}
			read += nr
			if nr == 0 {
				watchdog.Reset(5 * time.Second)
				select {
				case <-sConn.readWake:
				case <-sConn.done:
					b.Fatalf("server conn died: %v", sConn.TermError())
				case <-watchdog.C:
					b.Fatalf("stalled: read %d/%d, cli live=%d pto=%d srv rxPkts=%d",
						read, len(payload), cliC.core.ledger.nLive,
						cliC.core.paths[0].ptoCount, sConn.core.paths[0].rxPkts)
				}
				if !watchdog.Stop() {
					select {
					case <-watchdog.C:
					default:
					}
				}
			}
		}
	}

	// Warmup: the first transfer carries STREAM_OPEN and lets PMTU probing,
	// congestion state, and buffer pools reach steady state before timing.
	errCh := writeAll()
	for {
		s, ok := sConn.AcceptStream()
		if ok {
			srvSID = s
			break
		}
		select {
		case <-sConn.readWake:
		case <-sConn.done:
			b.Fatalf("server conn died: %v", sConn.TermError())
		}
	}
	readAll()
	if err := <-errCh; err != nil {
		b.Fatalf("warmup write: %v", err)
	}
	for i := 0; i < 40; i++ {
		errCh = writeAll()
		readAll()
		if err := <-errCh; err != nil {
			b.Fatalf("warmup write: %v", err)
		}
		if p.MaxUDPPayload <= wire.BaseUDPPayload || cliC.core.paths[0].m >= uint64(p.MaxUDPPayload) {
			break
		}
	}
	if p.MaxUDPPayload > wire.BaseUDPPayload && cliC.core.paths[0].m < uint64(p.MaxUDPPayload) {
		b.Fatalf("pmtu did not converge: got %d want %d", cliC.core.paths[0].m, p.MaxUDPPayload)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		errCh = writeAll()
		readAll()
		if err := <-errCh; err != nil {
			b.Fatalf("write: %v", err)
		}
	}
	b.StopTimer()

	close(stop)
	cliC.Close(0)
	sConn.Close(0)
	l.Close()
}
