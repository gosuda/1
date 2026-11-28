package grt

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestConfigPresets(t *testing.T) {
	appID := [16]byte{1, 2, 3, 4}
	serverName := "example.com:4433"

	// 1. LowResource Preset
	cliLowRes := NewClientConfig(serverName, appID, PresetLowResource)
	if cliLowRes.Params.InitialMaxData != 0 {
		t.Fatalf("LowResource client should have 0 initial credit, got %d", cliLowRes.Params.InitialMaxData)
	}
	srvLowRes := NewServerConfig(nil, appID, PresetLowResource)
	if srvLowRes.AdmissionMode != AdmissionRetry {
		t.Fatalf("LowResource server should default to AdmissionRetry (2-RTT), got %v", srvLowRes.AdmissionMode)
	}

	// 2. Standard1RTT Preset
	cli1RTT := NewClientConfig(serverName, appID, PresetStandard1RTT)
	if cli1RTT.Params.FeatureFlags&FeatureEarly != 0 {
		t.Fatal("Standard1RTT client should have FeatureEarly disabled")
	}
	if cli1RTT.Params.InitialMaxData == 0 {
		t.Fatal("Standard1RTT client should have positive initial credit")
	}
	srv1RTT := NewServerConfig(nil, appID, PresetStandard1RTT)
	if srv1RTT.AdmissionMode != AdmissionTokenless {
		t.Fatalf("Standard1RTT server should default to AdmissionTokenless (1-RTT), got %v", srv1RTT.AdmissionMode)
	}
	if srv1RTT.Params.FeatureFlags&FeatureEarly != 0 {
		t.Fatal("Standard1RTT server should have FeatureEarly disabled")
	}

	// 3. LowLatency Preset
	cliLowLat := NewClientConfig(serverName, appID, PresetLowLatency)
	if cliLowLat.Params.InitialMaxData != 65536 {
		t.Fatalf("LowLatency client should have 64KB initial credit, got %d", cliLowLat.Params.InitialMaxData)
	}
	if cliLowLat.Params.FeatureFlags&FeatureEarly == 0 {
		t.Fatal("LowLatency client should have FeatureEarly enabled")
	}
	srvLowLat := NewServerConfig(nil, appID, PresetLowLatency)
	if srvLowLat.AdmissionMode != AdmissionTokenless {
		t.Fatalf("LowLatency server should default to AdmissionTokenless (1-RTT), got %v", srvLowLat.AdmissionMode)
	}

	// 4. HighThroughput Preset
	cliHighTP := NewClientConfig(serverName, appID, PresetHighThroughput)
	if cliHighTP.Params.MaxUDPPayload != 4096 {
		t.Fatalf("HighThroughput client should have 4096 MaxUDPPayload, got %d", cliHighTP.Params.MaxUDPPayload)
	}
	srvHighTP := NewServerConfig(nil, appID, PresetHighThroughput)
	if srvHighTP.AdmissionMode != AdmissionTokenless {
		t.Fatalf("HighThroughput server should default to AdmissionTokenless (1-RTT), got %v", srvHighTP.AdmissionMode)
	}
}

func TestConfigOptions(t *testing.T) {
	appID := [16]byte{9}
	key := [32]byte{42}
	srvName := "test.host:8443"

	// Override admission mode
	srv := NewServerConfig(nil, appID, PresetLowResource,
		WithAdmissionMode(AdmissionTokenless),
		WithServerName(srvName),
		WithMaxLiveSent(2048),
	)
	if srv.AdmissionMode != AdmissionTokenless {
		t.Fatalf("WithAdmissionMode failed: got %v", srv.AdmissionMode)
	}
	if srv.ServerName != srvName {
		t.Fatalf("WithServerName failed: got %s", srv.ServerName)
	}
	if srv.MaxLiveSent != 2048 {
		t.Fatalf("WithMaxLiveSent failed: got %d", srv.MaxLiveSent)
	}

	cli := NewClientConfig(srvName, appID, PresetLowLatency,
		WithExpectedKey(key),
		WithAuthMode(AuthPinned),
		WithDisableAES(true),
		WithCipherPreference(CipherXChaCha20Poly1305),
	)
	if cli.ExpectedKey != key {
		t.Fatal("WithExpectedKey failed")
	}
	if cli.AuthMode != AuthPinned {
		t.Fatal("WithAuthMode failed")
	}
	if !cli.DisableAES {
		t.Fatal("WithDisableAES failed")
	}
	if len(cli.CipherPreference) != 1 || cli.CipherPreference[0] != CipherXChaCha20Poly1305 {
		t.Fatal("WithCipherPreference failed")
	}
}

func TestPreset1RTT_UDPLoopback(t *testing.T) {
	sock, err := ListenUDP(&net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	authority := sock.LocalAddr().String()
	appID := [16]byte{42}

	id, err := GenerateServerIdentity(authority, appID)
	if err != nil {
		t.Fatal(err)
	}

	// Server configured with PresetStandard1RTT (ALLOW_TOKENLESS 1-RTT admission)
	scfg := NewServerConfig(id, appID, PresetStandard1RTT,
		WithServerName(authority),
		WithMaxLiveSent(4096),
		WithNowUnix(func() int64 { return time.Now().Unix() }),
	)
	l := ListenOver(sock, scfg)
	defer l.Close()

	// Client configured with PresetStandard1RTT
	ccfg := NewClientConfig(authority, appID, PresetStandard1RTT,
		WithExpectedKey(id.IdentityPub),
		WithMaxLiveSent(4096),
		WithNowUnix(func() int64 { return time.Now().Unix() }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli, err := Dial(ctx, l.Addr().String(), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	if err := cli.WaitConfirmed(ctx); err != nil {
		t.Fatalf("client confirm: %v", err)
	}

	srv, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer srv.Close()

	// Bidirectional stream transfer
	go func() {
		s, err := srv.AcceptStream(ctx)
		if err != nil {
			return
		}
		data, _ := io.ReadAll(s)
		s.Write(append([]byte("ack:"), data...))
		s.Close()
	}()

	s, err := cli.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ack:ping" {
		t.Fatalf("got %q, want ack:ping", string(got))
	}
}

func TestPresetLowResource_DefaultIs2RTT(t *testing.T) {
	sock, err := ListenUDP(&net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	authority := sock.LocalAddr().String()
	appID := [16]byte{55}

	id, err := GenerateServerIdentity(authority, appID)
	if err != nil {
		t.Fatal(err)
	}

	// Server configured with PresetLowResource (AdmissionRetry: 2-RTT cookie exchange)
	scfg := NewServerConfig(id, appID, PresetLowResource,
		WithServerName(authority),
		WithMaxLiveSent(4096),
		WithNowUnix(func() int64 { return time.Now().Unix() }),
	)
	l := ListenOver(sock, scfg)
	defer l.Close()

	ccfg := NewClientConfig(authority, appID, PresetLowResource,
		WithExpectedKey(id.IdentityPub),
		WithMaxLiveSent(4096),
		WithNowUnix(func() int64 { return time.Now().Unix() }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli, err := Dial(ctx, l.Addr().String(), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	if err := cli.WaitConfirmed(ctx); err != nil {
		t.Fatalf("client confirm: %v", err)
	}

	srv, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer srv.Close()
}

func TestPresetLowLatency_ImmediateData(t *testing.T) {
	sock, err := ListenUDP(&net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	authority := sock.LocalAddr().String()
	appID := [16]byte{77}

	id, err := GenerateServerIdentity(authority, appID)
	if err != nil {
		t.Fatal(err)
	}

	// LowLatency defaults to AdmissionTokenless (1-RTT) and 64KB credit
	scfg := NewServerConfig(id, appID, PresetLowLatency,
		WithServerName(authority),
		WithMaxLiveSent(4096),
		WithNowUnix(func() int64 { return time.Now().Unix() }),
	)
	l := ListenOver(sock, scfg)
	defer l.Close()

	ccfg := NewClientConfig(authority, appID, PresetLowLatency,
		WithExpectedKey(id.IdentityPub),
		WithMaxLiveSent(4096),
		WithNowUnix(func() int64 { return time.Now().Unix() }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli, err := Dial(ctx, l.Addr().String(), ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	if err := cli.WaitConfirmed(ctx); err != nil {
		t.Fatalf("client confirm: %v", err)
	}

	srv, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer srv.Close()

	// Immediate 16KB payload transfer without waiting for separate MAX_DATA exchange
	payload := make([]byte, 16384)
	for i := range payload {
		payload[i] = byte(i)
	}

	go func() {
		s, err := srv.AcceptStream(ctx)
		if err != nil {
			return
		}
		data, _ := io.ReadAll(s)
		s.Write(data)
		s.Close()
	}()

	s, err := cli.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	echo, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(echo) != len(payload) {
		t.Fatalf("echoed len %d, want %d", len(echo), len(payload))
	}
}
