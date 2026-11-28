//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"gosuda.org/ivnp"
)

func runLiveI2PColdStart(t *testing.T, hops int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	t0 := time.Now()
	makeRouter := func(label string) (*ivnp.Router, error) {
		cfg := ivnp.DefaultRouterConfig()
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
		cfg.Exploratory.Inbound.Hops = hops
		cfg.Exploratory.Outbound.Hops = hops
		cfg.Exploratory.Inbound.Count = 2
		cfg.Exploratory.Outbound.Count = 2
		return ivnp.NewRouter(ctx, cfg)
	}

	type routerRes struct {
		r   *ivnp.Router
		err error
		dur time.Duration
	}
	rCh := make([]chan routerRes, 2)
	for i := range 2 {
		rCh[i] = make(chan routerRes, 1)
		go func(idx int) {
			s := time.Now()
			r, err := makeRouter(fmt.Sprintf("R%d", idx))
			rCh[idx] <- routerRes{r: r, err: err, dur: time.Since(s)}
		}(i)
	}
	resA := <-rCh[0]
	if resA.err != nil {
		t.Fatalf("start router A: %v", resA.err)
	}
	defer resA.r.Close()
	resB := <-rCh[1]
	if resB.err != nil {
		t.Fatalf("start router B: %v", resB.err)
	}
	defer resB.r.Close()
	tStart := time.Now()
	t.Logf("[%d-hop] T+%v NewRouter done (A=%v, B=%v)",
		hops, tStart.Sub(t0).Round(time.Millisecond), resA.dur.Round(time.Millisecond), resB.dur.Round(time.Millisecond))

	// Track reseed completion and WaitReady in parallel for both routers.
	var reseedDur [2]time.Duration
	var readyDur [2]time.Duration
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var readyErr error
	routers := []*ivnp.Router{resA.r, resB.r}
	for i, r := range routers {
		wg.Add(1)
		go func(idx int, router *ivnp.Router) {
			defer wg.Done()
			// Poll reseed metric until first reseed succeeds or router becomes ready.
			stopPoll := make(chan struct{})
			pollDone := make(chan struct{})
			go func() {
				defer close(pollDone)
				ticker := time.NewTicker(25 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopPoll:
						return
					case <-ticker.C:
						snap := router.Node().Default().RegistrySnapshot()
						if snap.Reseed.Successes > 0 || snap.NetDB.Routers > 0 {
							reseedDur[idx] = time.Since(t0)
							t.Logf("[%d-hop] R%d reseed done at T+%v (reseed_ok=%d, routers=%d, floodfills=%d)",
								hops, idx, reseedDur[idx].Round(time.Millisecond), snap.Reseed.Successes, snap.NetDB.Routers, snap.NetDB.Floodfills)
							return
						}
					}
				}
			}()
			err := router.WaitReady(ctx)
			close(stopPoll)
			<-pollDone
			readyDur[idx] = time.Since(t0)
			if err != nil {
				errMu.Lock()
				if readyErr == nil {
					readyErr = fmt.Errorf("router %d WaitReady: %w", idx, err)
				}
				errMu.Unlock()
				return
			}
			snap := router.Node().Default().RegistrySnapshot()
			t.Logf("[%d-hop] R%d WaitReady at T+%v (routers=%d, ff=%d, expIn=%d, expOut=%d, stage=%d, tx_hs_fail=%d, exp_in_to=%d, exp_out_to=%d)",
				hops, idx, readyDur[idx].Round(time.Millisecond), snap.NetDB.Routers, snap.NetDB.Floodfills,
				snap.Tunnel.ExploratoryInboundActive, snap.Tunnel.ExploratoryOutboundActive, snap.Bootstrap.Stage,
				snap.Transport.HandshakeFailures, snap.Tunnel.ExploratoryInboundTimeouts, snap.Tunnel.ExploratoryOutboundTimeouts)
		}(i, r)
	}
	wg.Wait()
	if readyErr != nil {
		t.Fatal(readyErr)
	}
	tReady := time.Now()

	// Create destinations on both routers in parallel.
	destCfg := ivnp.DefaultDestinationConfig()
	destCfg.Tunnels.Inbound.Hops = hops
	destCfg.Tunnels.Outbound.Hops = hops
	destCfg.Tunnels.Inbound.Count = 2
	destCfg.Tunnels.Outbound.Count = 2

	type destRes struct {
		d   *ivnp.Destination
		err error
		dur time.Duration
	}
	dCh := make([]chan destRes, 2)
	for i := range 2 {
		dCh[i] = make(chan destRes, 1)
		go func(idx int) {
			s := time.Now()
			d, err := routers[idx].NewDestination(ctx, destCfg)
			dCh[idx] <- destRes{d: d, err: err, dur: time.Since(s)}
		}(i)
	}
	dA := <-dCh[0]
	if dA.err != nil {
		t.Fatalf("dest A: %v", dA.err)
	}
	defer dA.d.Close()
	dB := <-dCh[1]
	if dB.err != nil {
		t.Fatalf("dest B: %v", dB.err)
	}
	defer dB.d.Close()
	tDest := time.Now()
	t.Logf("[%d-hop] T+%v Both NewDestination done (A=%v, B=%v, wall=%v)",
		hops, tDest.Sub(t0).Round(time.Millisecond), dA.dur.Round(time.Millisecond),
		dB.dur.Round(time.Millisecond), tDest.Sub(tReady).Round(time.Millisecond))

	// Listen on A, dial from B, exchange first P2P bytes.
	ln, err := dA.d.Listen("i2p", ":8080")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 8)
	stopAccept := make(chan struct{})
	defer close(stopAccept)
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			select {
			case accepted <- conn:
			case <-stopAccept:
				_ = conn.Close()
				return
			}
		}
	}()

	var outbound net.Conn
	var dialErr error
	tDialStart := time.Now()
	for attempt := 1; attempt <= 3; attempt++ {
		for len(accepted) > 0 {
			stale := <-accepted
			_ = stale.Close()
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 45*time.Second)
		outbound, dialErr = dB.d.DialContext(attemptCtx, "i2p", net.JoinHostPort(dA.d.B32(), "8080"))
		attemptCancel()
		if dialErr == nil {
			t.Logf("[%d-hop] Dial succeeded on attempt %d in %v", hops, attempt, time.Since(tDialStart).Round(time.Millisecond))
			break
		}
		t.Logf("[%d-hop] Dial attempt %d failed after %v: %v", hops, attempt, time.Since(tDialStart).Round(time.Millisecond), dialErr)
	}
	if dialErr != nil {
		t.Fatalf("dial failed: %v", dialErr)
	}
	defer outbound.Close()
	tDial := time.Now()

	var inbound net.Conn
	acceptDeadline := time.NewTimer(30 * time.Second)
	defer acceptDeadline.Stop()
	for inbound == nil {
		select {
		case conn := <-accepted:
			if conn.RemoteAddr().String() == outbound.LocalAddr().String() {
				inbound = conn
			} else {
				_ = conn.Close()
			}
		case <-acceptDeadline.C:
			t.Fatal("accept timed out")
		}
	}
	defer inbound.Close()

	msg := []byte("live-i2p-coldstart-ping")
	_ = outbound.SetDeadline(time.Now().Add(30 * time.Second))
	_ = inbound.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := outbound.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(inbound, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	tEnd := time.Now()

	t.Logf("=== SUMMARY [%d-hop LIVE I2P] ===", hops)
	t.Logf("  RouterStart:  %v", tStart.Sub(t0).Round(time.Millisecond))
	t.Logf("  Reseed:       R0=%v, R1=%v", reseedDur[0].Round(time.Millisecond), reseedDur[1].Round(time.Millisecond))
	t.Logf("  WaitReady:    R0=%v, R1=%v (wall=%v)", readyDur[0].Round(time.Millisecond), readyDur[1].Round(time.Millisecond), tReady.Sub(tStart).Round(time.Millisecond))
	t.Logf("  NewDest:      A=%v, B=%v (wall=%v)", dA.dur.Round(time.Millisecond), dB.dur.Round(time.Millisecond), tDest.Sub(tReady).Round(time.Millisecond))
	t.Logf("  Dial:         %v", tDial.Sub(tDest).Round(time.Millisecond))
	t.Logf("  FirstByte:    %v", tEnd.Sub(tDial).Round(time.Millisecond))
	t.Logf("  TOTAL P2P:    %v", tEnd.Sub(t0).Round(time.Millisecond))

	for i, r := range routers {
		m := r.Node().Default().RegistrySnapshot()
		t.Logf("  R%d metrics: reseed_ok=%d reseed_fail=%d tx_conns=%d tx_hs_fail=%d ntcp2=%d ssu2=%d exp_in_ok=%d exp_in_to=%d exp_out_ok=%d exp_out_to=%d cli_in_ok=%d cli_in_to=%d cli_out_ok=%d cli_out_to=%d pub_ls_ok=%d pub_to=%d netdb_lookups=%d netdb_fail=%d",
			i, m.Reseed.Successes, m.Reseed.Failures, m.Transport.Connections, m.Transport.HandshakeFailures,
			m.Transport.NTCP2Sessions, m.Transport.SSU2Sessions,
			m.Tunnel.ExploratoryInboundSuccesses, m.Tunnel.ExploratoryInboundTimeouts,
			m.Tunnel.ExploratoryOutboundSuccesses, m.Tunnel.ExploratoryOutboundTimeouts,
			m.Tunnel.ClientInboundSuccesses, m.Tunnel.ClientInboundTimeouts,
			m.Tunnel.ClientOutboundSuccesses, m.Tunnel.ClientOutboundTimeouts,
			m.Publication.LeaseSet2Successes, m.Publication.Timeouts,
			m.NetDB.Lookups, m.NetDB.LookupFailures)
	}
}

func TestLiveI2PColdStart1Hop(t *testing.T) {
	if os.Getenv("IVNP_LIVE_COLDSTART") != "1" {
		t.Skip("set IVNP_LIVE_COLDSTART=1 to run live I2P cold-start measurement")
	}
	runLiveI2PColdStart(t, 1)
}

func TestLiveI2PColdStart2Hop(t *testing.T) {
	if os.Getenv("IVNP_LIVE_COLDSTART") != "1" {
		t.Skip("set IVNP_LIVE_COLDSTART=1 to run live I2P cold-start measurement")
	}
	runLiveI2PColdStart(t, 2)
}
