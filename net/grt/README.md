# GRT (General Reliable Transport)

`gosuda.org/grt` implements **GRT/1.1** ([`spec/GRT11.md`](spec/GRT11.md)), a reliable, authenticated, post-quantum-secured UDP transport protocol in Go.

## Features

- **Post-Quantum Handshake (PQXDH)**: Hybrid X25519 + ML-KEM-768 key exchange with XEdDSA-signed prekeys, service-bound transcript hashing, and `PINNED` or `CHAIN` (root $\to$ intermediate $\to$ leaf) server authentication.
- **Admission & Anti-Amplification**: Stateless `RETRY` tokens, `NEW_TOKEN` address-validation reuse tokens with automatic key rotation, bounded `ALLOW_TOKENLESS` 1-RTT admission with `RETRY` fallback, and per-source/global token buckets.
- **Resumption & 0-RTT Early Data**: HKDF-rotated ticket keys (24h period + $2^{24}$-seal sub-keys), policy/domain binding, and pluggable `ReplayLedger` for single-use 0-RTT early data.
- **Multiplexed Streams & Unreliable Datagrams**: Bidirectional and unidirectional streams with explicit slot generations (`STREAM_RETIRE` / `RETIRE_ACK`), stream/connection flow control, deficit round-robin (DRR) scheduling, and unreliable datagrams (`SendDatagram` / `ReceiveDatagram`).
- **Connection Migration & Mobile Parking**: Elective and peer-rebinding path migration with challenge/response validation, cutover barriers (`PATH_INTENT` / `PATH_SELECTED`), DPLPMTUD, and mobile parking leases (`PARK_REQUEST` / `PARK_GRANT` / `PARK_WAKE` / `PARK_AWAKE`).
- **In-Band Hybrid Key Refresh**: 3-slot epoch rotation (`PREVIOUS`, `CURRENT`, `STAGED`) using fresh X25519 + ML-KEM-768 material without interrupting traffic.
- **Formal Verification & Code Generation**: Canonical protocol IR (`internal/formal`) verified by an explicit-state BFS model checker and exported to TLA+ (`spec/formal/GRT11.tla`) and zero-allocation Go transition tables (`internal/engine/z_formal_gen.go`).

## Quick Start

```go
package main

import (
	"context"
	"io"
	"log"
	"time"

	"gosuda.org/grt"
)

func main() {
	appID := [16]byte{1}
	srvID, err := grt.GenerateServerIdentity("127.0.0.1:4433", appID)
	if err != nil {
		log.Fatal(err)
	}

	ln, err := grt.Listen("127.0.0.1:4433", &grt.Config{
		Params:         grt.LowLatencyParams(),
		AppID:          appID,
		ServerIdentity: srvID,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		defer conn.Close()
		st, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		_, _ = io.Copy(st, st)
		_ = st.CloseWrite()
	}()

	cli, err := grt.Dial(ctx, ln.Addr().String(), &grt.Config{
		Params:      grt.LowLatencyParams(),
		AppID:       appID,
		ServerName:  "127.0.0.1:4433",
		AuthMode:    grt.AuthPinned,
		ExpectedKey: srvID.IdentityPub,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer cli.Close()

	st, err := cli.OpenStreamSync(ctx)
	if err != nil {
		log.Fatal(err)
	}
	_, _ = st.Write([]byte("hello grt"))
	_ = st.CloseWrite()
	reply, _ := io.ReadAll(st)
	log.Printf("reply: %s", reply)
}
```

## Verification & Code Generation

```bash
go run ./cmd/grtgen
go test -race ./...
```
