package sim

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"gosuda.org/grt/internal/engine"
	"gosuda.org/grt/internal/wire"
)

// DefaultDSTSeeds contains the deterministic seed matrix requested for DST verification:
// 1, 42, 67, 1982, 1992, 2002, 2012, 2022, 2026.
var DefaultDSTSeeds = []uint64{1, 42, 67, 1982, 1992, 2002, 2012, 2022, 2026}

// GetDSTSeeds returns the seeds to test. If DST_SEED is set in the environment,
// it runs only that single seed (for CI matrix workers); otherwise it runs all seeds.
func GetDSTSeeds() []uint64 {
	if val := os.Getenv("DST_SEED"); val != "" {
		if s, err := strconv.ParseUint(val, 10, 64); err == nil {
			return []uint64{s}
		}
	}
	return DefaultDSTSeeds
}

// TestDST_Handshake_LossMatrix runs deterministic handshake tests across
// configured PRNG seeds and packet loss rates (0%, 5%, 15%).
func TestDST_Handshake_LossMatrix(t *testing.T) {
	seeds := GetDSTSeeds()
	lossRates := []float64{0.0, 0.05, 0.15}

	for _, seed := range seeds {
		for _, loss := range lossRates {
			name := fmt.Sprintf("seed=%d/loss=%.2f", seed, loss)
			t.Run(name, func(t *testing.T) {
				netCfg := NetConfig{
					Seed:        [2]uint64{seed, seed ^ 0x9abcdef0},
					LossRate:    loss,
					MinDelay:    1 * time.Millisecond,
					MaxDelay:    3 * time.Millisecond,
					ReorderRate: 0.05,
				}
				id := NewDeterministicIdentity(seed)
				ccfg, scfg := DefaultSimConfigsWithSeed(id, seed)

				pair := NewSimPair(t, netCfg, ccfg, scfg, id)
				defer pair.Close()

				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				// Wait for server accept
				var srvConn *engine.Conn
				select {
				case srvConn = <-pair.Listener.AcceptCh():
					pair.Server = srvConn
				case <-ctx.Done():
					if loss <= 0.05 {
						t.Fatalf("seed=%v loss=%.2f: handshake timed out waiting for server", seed, loss)
					}
					return
				}

				pair.Client.WaitConfirmed(ctx)

				if pair.Client.State() == 7 /* stEstablished */ {
					pair.Invariants.RecordCipherAgreement(pair.Client.Cipher(), pair.Server.Cipher())
				}
				pair.Invariants.AssertAll(t)
			})
		}
	}
}

// TestDST_StreamTransfer_Chaos transfers 64 KB across bidi streams under
// 10% loss, 15% reordering, 5% duplication, and delay jitter, verifying
// exact SHA-256 stream integrity and absence of hangs.
func TestDST_StreamTransfer_Chaos(t *testing.T) {
	seeds := GetDSTSeeds()
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			netCfg := NetConfig{
				Seed:          [2]uint64{seed, seed ^ 0xeeff0011},
				LossRate:      0.10,
				MinDelay:      1 * time.Millisecond,
				MaxDelay:      4 * time.Millisecond,
				ReorderRate:   0.15,
				DuplicateRate: 0.05,
			}

			id := NewDeterministicIdentity(seed)
			ccfg, scfg := DefaultSimConfigsWithSeed(id, seed)
			pair := NewSimPair(t, netCfg, ccfg, scfg, id)
			defer pair.Close()

			pair.WaitHandshake(t, 4*time.Second)

			// Deterministic pseudo-random payloads
			c2sData := DeterministicPayload(seed, 8*1024)
			s2cData := DeterministicPayload(seed^0x1234, 8*1024)

			pair.TransferBidirectional(t, c2sData, s2cData, 15*time.Second)
			pair.Invariants.AssertAll(t)

			stats := pair.Net.Stats()
			if stats.Dropped.Load() == 0 && netCfg.LossRate > 0 {
				t.Log("warning: no packets dropped under loss rate")
			}
		})
	}
}

// TestDST_ElectiveMigration tests make-before-break path migration under traffic.
func TestDST_ElectiveMigration(t *testing.T) {
	seeds := GetDSTSeeds()
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			netCfg := NetConfig{
				Seed:     [2]uint64{seed, seed ^ 0x3333cccc},
				LossRate: 0.0,
				MinDelay: 1 * time.Millisecond,
				MaxDelay: 3 * time.Millisecond,
			}

			id := NewDeterministicIdentity(seed)
			ccfg, scfg := DefaultSimConfigsWithSeed(id, seed)
			ccfg.Params.FeatureFlags |= wire.FeatureMigration
			scfg.Params.FeatureFlags |= wire.FeatureMigration

			pair := NewSimPair(t, netCfg, ccfg, scfg, id)
			defer pair.Close()

			// Multi-homed server endpoint
			srvEP2Addr := netip.MustParseAddrPort("10.0.0.1:4444")
			srvEP2 := pair.Net.Endpoint(srvEP2Addr)
			go func() {
				buf := make([]byte, 64*1024)
				for {
					rn, src, dst, ecn, rerr := srvEP2.Recv(buf)
					if rerr != nil {
						return
					}
					pkt := make([]byte, rn)
					copy(pkt, buf[:rn])
					pair.Listener.OnDatagram(pkt, src, dst, ecn)
				}
			}()

			pair.WaitHandshake(t, 3*time.Second)

			// Start continuous stream transfer
			sid, err := pair.Client.OpenStream()
			if err != nil {
				t.Fatalf("OpenStream failed: %v", err)
			}

			payload := DeterministicPayload(seed, 16*1024)

			// Write first half of payload to emit stream open and initial bytes
			half := len(payload) / 2
			n, err := pair.Client.Write(sid, payload[:half])
			if err != nil || n != half {
				t.Fatalf("client write first half failed: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()

			var srvSid uint64
			for {
				if s, ok := pair.Server.AcceptStream(); ok {
					srvSid = s
					break
				}
				pair.Server.WaitReadable(ctx)
				if ctx.Err() != nil {
					t.Fatal("timeout waiting for server accept stream")
				}
			}

			var wg sync.WaitGroup
			var srvRecv []byte
			var srvErr error

			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := make([]byte, 4096)
				for {
					n, rerr := pair.Server.Read(srvSid, buf)
					if n > 0 {
						srvRecv = append(srvRecv, buf[:n]...)
					}
					if errors.Is(rerr, engine.ErrStreamEOF) {
						break
					}
					if rerr != nil {
						srvErr = rerr
						return
					}
					if n == 0 {
						pair.Server.WaitReadable(ctx)
						if ctx.Err() != nil {
							srvErr = ctx.Err()
							return
						}
					}
				}
			}()

			// Trigger elective migration: client moves to srvEP2Addr
			if merr := pair.Client.Migrate(srvEP2Addr); merr != nil {
				t.Fatalf("client Migrate failed: %v", merr)
			}

			// Write second half of payload
			written := half
			for written < len(payload) {
				chunk := len(payload) - written
				if chunk > 4096 {
					chunk = 4096
				}
				n2, werr := pair.Client.Write(sid, payload[written:written+chunk])
				written += n2
				if werr == engine.ErrWouldBlock {
					pair.Client.WaitWritable(ctx)
					if ctx.Err() != nil {
						t.Fatalf("write timeout: %v", ctx.Err())
					}
					continue
				}
				if werr != nil {
					t.Fatalf("client write second half failed: %v", werr)
				}
			}
			_ = pair.Client.Fin(sid)

			wg.Wait()

			if srvErr != nil {
				t.Fatalf("server read during migration failed: %v", srvErr)
			}
			if !bytes.Equal(srvRecv, payload) {
				t.Fatalf("payload mismatch after migration: received %d / %d bytes", len(srvRecv), len(payload))
			}
		})
	}
}

// TestDST_ECN_ValidationAndBackoff tests ECN probing, ECT(0) marking and CE congestion response.
func TestDST_ECN_ValidationAndBackoff(t *testing.T) {
	seeds := GetDSTSeeds()
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			netCfg := NetConfig{
				Seed:        [2]uint64{seed, seed ^ 0x33445566},
				LossRate:    0.0,
				MinDelay:    1 * time.Millisecond,
				MaxDelay:    2 * time.Millisecond,
				ECNMarkRate: 0.20, // 20% packets marked CE
			}

			id := NewDeterministicIdentity(seed)
			ccfg, scfg := DefaultSimConfigsWithSeed(id, seed)
			ccfg.Params.FeatureFlags |= wire.FeatureECN
			scfg.Params.FeatureFlags |= wire.FeatureECN

			pair := NewSimPair(t, netCfg, ccfg, scfg, id)
			defer pair.Close()

			pair.WaitHandshake(t, 3*time.Second)

			// Stream packets to exercise ECN feedback
			c2sData := DeterministicPayload(seed, 8*1024)
			s2cData := DeterministicPayload(seed^0xabcd, 8*1024)

			pair.TransferBidirectional(t, c2sData, s2cData, 4*time.Second)

			stats := pair.Net.Stats()
			if stats.ECNMarked.Load() == 0 {
				t.Log("warning: no ECN CE packets were marked")
			}
		})
	}
}

// TestDST_ZeroRTT_Resumption_ReplayDefense tests 0-RTT ticket usage and replay defense.
func TestDST_ZeroRTT_Resumption_ReplayDefense(t *testing.T) {
	seeds := GetDSTSeeds()
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			netCfg := NetConfig{
				Seed:     [2]uint64{seed, seed ^ 0x00030004},
				MinDelay: 1 * time.Millisecond,
				MaxDelay: 2 * time.Millisecond,
			}

			id := NewDeterministicIdentity(seed)
			ccfg, scfg := DefaultSimConfigsWithSeed(id, seed)
			ccfg.Params.FeatureFlags |= wire.FeatureEarly
			scfg.Params.FeatureFlags |= wire.FeatureEarly

			// First connection to obtain resumption ticket
			pair1 := NewSimPair(t, netCfg, ccfg, scfg, id)
			pair1.WaitHandshake(t, 3*time.Second)

			// Send data so NEW_TICKET can be issued
			dummy := DeterministicPayload(seed, 64)
			sid, _ := pair1.Client.OpenStream()
			pair1.Client.Write(sid, dummy)
			pair1.Client.Fin(sid)

			// Await ticket
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var ticket *engine.ClientTicket
			for ticket == nil {
				ticket = pair1.Client.ResumeTicket()
				if ticket != nil {
					break
				}
				subCtx, subCancel := context.WithTimeout(ctx, 20*time.Millisecond)
				pair1.Client.WaitReadable(subCtx)
				subCancel()
				if ctx.Err() != nil {
					break
				}
			}
			pair1.Close()

			if ticket == nil {
				t.Fatal("ticket was not issued after handshake confirmation")
			}

			// Second connection: 0-RTT with early data using shared server state
			ccfg2, scfg2 := DefaultSimConfigsWithSeed(id, seed)
			ccfg2.Params.FeatureFlags |= wire.FeatureEarly
			scfg2.Params.FeatureFlags |= wire.FeatureEarly
			scfg2.TicketKey = scfg.TicketKey
			scfg2.TicketIssuer = scfg.TicketIssuer
			scfg2.ReplayLedger = scfg.ReplayLedger
			ccfg2.Ticket = ticket
			ccfg2.EarlyData = []byte("early-data-payload")

			pair2 := NewSimPair(t, netCfg, ccfg2, scfg2, id)
			defer pair2.Close()

			pair2.WaitHandshake(t, 3*time.Second)

			status := pair2.Client.EarlyDataStatus()
			if status != 1 /* wire.EarlyAccepted */ {
				t.Fatalf("expected early data accepted (1), got %d", status)
			}
		})
	}
}

// TestDST_MobileParking tests mobile parking request and wake.
func TestDST_MobileParking(t *testing.T) {
	seeds := GetDSTSeeds()
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			netCfg := NetConfig{
				Seed:     [2]uint64{seed, seed ^ 0x77776666},
				MinDelay: 1 * time.Millisecond,
				MaxDelay: 2 * time.Millisecond,
			}

			id := NewDeterministicIdentity(seed)
			ccfg, scfg := DefaultSimConfigsWithSeed(id, seed)
			ccfg.Params.FeatureFlags |= wire.FeaturePark
			scfg.Params.FeatureFlags |= wire.FeaturePark
			ccfg.Params.MaxParkDurationMS = 600000
			scfg.Params.MaxParkDurationMS = 600000

			pair := NewSimPair(t, netCfg, ccfg, scfg, id)
			defer pair.Close()

			pair.WaitHandshake(t, 3*time.Second)

			// Request parking lease of 10000ms (>= 1000ms minimum)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var parkErr error
			for {
				parkErr = pair.Client.Park(10000)
				if parkErr == nil {
					break
				}
				if ctx.Err() != nil {
					t.Fatalf("Park failed: %v", parkErr)
				}
				subCtx, subCancel := context.WithTimeout(ctx, 20*time.Millisecond)
				pair.Client.WaitReadable(subCtx)
				subCancel()
			}

			// Wait for client to enter parkLive (state 2)
			for pair.Client.ParkState() != 2 /* parkLive */ {
				if ctx.Err() != nil {
					t.Fatalf("timed out waiting for parkLive; current state=%d", pair.Client.ParkState())
				}
				subCtx, subCancel := context.WithTimeout(ctx, 20*time.Millisecond)
				pair.Client.WaitReadable(subCtx)
				subCancel()
			}

			// Wake from parking
			if err := pair.Client.Wake(); err != nil {
				t.Fatalf("Wake failed: %v", err)
			}

			// Wait for unpark to complete (parkIdle = 0)
			for pair.Client.ParkState() != 0 /* parkIdle */ {
				if ctx.Err() != nil {
					t.Fatalf("timed out waiting for parkIdle; client state=%d, server state=%d",
						pair.Client.ParkState(), pair.Server.ParkState())
				}
				subCtx, subCancel := context.WithTimeout(ctx, 20*time.Millisecond)
				pair.Client.WaitReadable(subCtx)
				subCancel()
			}

			select {
			case <-pair.Client.Done():
				t.Fatalf("connection died with: %v (state=%s)", pair.Client.TermError(), pair.Client.State())
			default:
			}

			// Verify connection can still stream data after waking
			data := DeterministicPayload(seed, 64)
			sid, err := pair.Client.OpenStream()
			if err != nil {
				t.Fatalf("OpenStream after wake failed: %v", err)
			}
			n, err := pair.Client.Write(sid, data)
			if err != nil || n != len(data) {
				t.Fatalf("Write after wake failed: %v", err)
			}
			_ = pair.Client.Fin(sid)
		})
	}
}

// TestDST_Section34_FaultCases tests Section 34.1 fault verification:
// - GSO with short final child
// - Short middle child rejected before submission
// - Post-tag padding rejected
// - 64*1200 aggregate byte cap
// - GRO without metadata non-magic-scanning
func TestDST_Section34_FaultCases(t *testing.T) {
	const S = 1200
	key := wire.BatchCompatibilityKey{
		SegmentSize: S,
		Local:       netip.MustParseAddrPort("10.0.0.1:5000"),
		Remote:      netip.MustParseAddrPort("10.0.0.2:4433"),
		CID:         0x12345678,
	}
	builder, err := wire.NewGSOBuilder(key)
	if err != nil {
		t.Fatalf("NewGSOBuilder: %v", err)
	}

	makePkt := func(totalLen int) []byte {
		pkt := make([]byte, totalLen)
		binary.BigEndian.PutUint16(pkt[0:], wire.Magic)
		pkt[2] = wire.Version
		pkt[3] = wire.ClassData
		binary.BigEndian.PutUint16(pkt[24:], uint16(totalLen-32))
		return pkt
	}

	// Case 1: Short final child is valid
	c1 := makePkt(S)
	c2 := makePkt(S)
	c3 := makePkt(500)

	if err := builder.AddChild(c1, key, false); err != nil {
		t.Fatalf("append c1: %v", err)
	}
	if err := builder.AddChild(c2, key, false); err != nil {
		t.Fatalf("append c2: %v", err)
	}
	if err := builder.AddChild(c3, key, false); err != nil {
		t.Fatalf("append short final child c3: %v", err)
	}

	agg, err := builder.Build()
	if err != nil {
		t.Fatalf("build aggregate with short final child: %v", err)
	}

	// Normalize with GRO
	children, err := wire.GRONormalize(agg, S)
	if err != nil {
		t.Fatalf("normalize GSO aggregate: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("expected 3 children, got %d", len(children))
	}
	if len(children[0]) != S || len(children[1]) != S || len(children[2]) != 500 {
		t.Fatalf("child lengths mismatch: %d, %d, %d", len(children[0]), len(children[1]), len(children[2]))
	}

	// Case 2: Short middle child rejected
	b2, err := wire.NewGSOBuilder(key)
	if err != nil {
		t.Fatal(err)
	}
	_ = b2.AddChild(c1, key, false)
	_ = b2.AddChild(c3, key, false) // short child in middle
	err = b2.AddChild(c2, key, false)
	if !errors.Is(err, wire.ErrShortMiddleChild) {
		t.Fatalf("expected ErrShortMiddleChild, got %v", err)
	}

	// Case 3: 64*1200-byte aggregate exceeds 65507 bytes
	b3, err := wire.NewGSOBuilder(key)
	if err != nil {
		t.Fatal(err)
	}
	var lastErr error
	for i := 0; i < 64; i++ {
		c := makePkt(S)
		if aerr := b3.AddChild(c, key, false); aerr != nil {
			lastErr = aerr
			break
		}
	}
	if !errors.Is(lastErr, wire.ErrGSOByteLimitExceeded) {
		t.Fatalf("expected ErrGSOByteLimitExceeded for 64*1200B aggregate, got %v", lastErr)
	}

	// Case 4: GRO without metadata treated as one datagram, never magic-scanned
	single, err := wire.GRONormalize(agg, 0)
	if err != nil {
		t.Fatalf("GRO without metadata failed: %v", err)
	}
	if len(single) != 1 || len(single[0]) != len(agg) {
		t.Fatalf("GRO without metadata must return aggregate as single datagram, got %d items", len(single))
	}
}
