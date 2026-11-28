package portalite

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDomainSupportsProtocolListNegotiation(t *testing.T) {
	tests := []struct {
		name string
		resp domainResponse
		want bool
	}{
		{
			name: "omitted list falls back to default",
			resp: domainResponse{ProtocolVersion: "10"},
			want: true,
		},
		{
			name: "explicit list includes 10",
			resp: domainResponse{ProtocolVersion: "10", SupportedProtocolVersions: []string{"9", "10"}},
			want: true,
		},
		{
			name: "explicit empty list supports nothing",
			resp: domainResponse{ProtocolVersion: "10", SupportedProtocolVersions: []string{}},
			want: false,
		},
		{
			name: "explicit list excludes 10 even if default is 10",
			resp: domainResponse{ProtocolVersion: "10", SupportedProtocolVersions: []string{"11"}},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.resp.supportsProtocol(ProtocolVersion); got != tc.want {
				t.Fatalf("supportsProtocol(%q) = %v, want %v", ProtocolVersion, got, tc.want)
			}
		})
	}
}

func TestValidateRegisterSIWEChallenge(t *testing.T) {
	relayURL, _ := url.Parse("https://relay.example:8443")
	now := time.Now().UTC().Truncate(time.Second)
	expires := now.Add(2 * time.Minute)

	validMsg, err := formatSIWEMessage(siweMessage{
		Domain:    "relay.example:8443",
		Address:   scalarOneAddress,
		URI:       "https://relay.example:8443/sdk/register",
		Statement: registerLeaseSIWEStatement,
		Nonce:     "abcd1234efgh",
		RequestID: "rch_123",
		IssuedAt:  now,
		ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("formatSIWEMessage: %v", err)
	}
	validChallenge := challengeResponse{
		ChallengeID: "rch_123",
		ExpiresAt:   expires,
		SIWEMessage: validMsg,
	}
	if err := validateRegisterSIWEChallenge(validChallenge, relayURL, scalarOneAddress, now); err != nil {
		t.Fatalf("valid challenge rejected: %v", err)
	}

	// Tampered statement (e.g. off-protocol phishing message) must be rejected.
	phishMsg, _ := formatSIWEMessage(siweMessage{
		Domain:    "relay.example:8443",
		Address:   scalarOneAddress,
		URI:       "https://relay.example:8443/sdk/register",
		Statement: "Transfer all funds",
		Nonce:     "abcd1234efgh",
		RequestID: "rch_123",
		IssuedAt:  now,
		ExpiresAt: expires,
	})
	if err := validateRegisterSIWEChallenge(challengeResponse{
		ChallengeID: "rch_123",
		ExpiresAt:   expires,
		SIWEMessage: phishMsg,
	}, relayURL, scalarOneAddress, now); err == nil {
		t.Fatal("accepted SIWE challenge with tampered statement")
	}

	// Tampered domain must be rejected.
	wrongDomainURL, _ := url.Parse("https://other.example:8443")
	if err := validateRegisterSIWEChallenge(validChallenge, wrongDomainURL, scalarOneAddress, now); err == nil {
		t.Fatal("accepted SIWE challenge for wrong relay domain")
	}
}

func TestClassifyNewRelayErrorCodesAndApproval(t *testing.T) {
	terminalCodes := []string{"protocol_incompatible", "source_not_allowed", "lease_denied"}
	for _, code := range terminalCodes {
		err := classifyRelayError(&APIError{StatusCode: 403, Code: code, Message: "rejected"}, false)
		if !isTerminalRelayFailure(err) {
			t.Errorf("code %q was not classified as terminal", code)
		}
	}

	retryableCodes := []string{"replica_conflict", "lease_rejected", "approval_queue_full"}
	for _, code := range retryableCodes {
		err := classifyRelayError(&APIError{StatusCode: 409, Code: code, Message: "retry"}, false)
		if isTerminalRelayFailure(err) {
			t.Errorf("code %q was classified as terminal, want retryable", code)
		}
	}

	if got := relayApprovalFromError(&APIError{Code: "lease_rejected"}); got != RelayApprovalPending {
		t.Fatalf("approval for lease_rejected = %q, want %q", got, RelayApprovalPending)
	}
	if got := relayApprovalFromError(&APIError{Code: "lease_denied"}); got != RelayApprovalRefused {
		t.Fatalf("approval for lease_denied = %q, want %q", got, RelayApprovalRefused)
	}
}

func TestDiscoverySSRFAndBounds(t *testing.T) {
	nonPublic := []string{
		"https://localhost",
		"https://foo.localhost",
		"https://127.0.0.1",
		"https://10.0.0.5",
		"https://192.168.1.10",
		"https://169.254.169.254",
	}
	for _, raw := range nonPublic {
		if !isNonPublicRelayURL(raw) {
			t.Errorf("isNonPublicRelayURL(%q) = false, want true", raw)
		}
	}
	if isNonPublicRelayURL("https://gosunuts.xyz") {
		t.Error("isNonPublicRelayURL(public host) = true, want false")
	}

	c := newDiscoveryCandidates()
	now := time.Now()
	entries := make(map[string]discoveryCandidate, maxDiscoveryEntries+50)
	for i := range maxDiscoveryEntries + 50 {
		entries[fmt.Sprintf("https://relay-%d.example", i)] = discoveryCandidate{
			expiresAt:   now.Add(time.Hour),
			supportsTCP: i%2 == 0,
		}
	}
	c.merge(now, entries)
	if len(c.relays) != maxDiscoveryEntries {
		t.Fatalf("retained relays = %d, want cap %d", len(c.relays), maxDiscoveryEntries)
	}
	tcpOnly := c.selectRelays(now, nil, false, true)
	if len(tcpOnly) == 0 || len(tcpOnly) >= len(c.relays) {
		t.Fatalf("expected TCP filtering to select a proper subset, got %d of %d", len(tcpOnly), len(c.relays))
	}
}

func TestExposeConfigValidationForNewModes(t *testing.T) {
	named := newTestIdentity(t)
	unnamed, err := GenerateUnnamedIdentity()
	if err != nil {
		t.Fatalf("GenerateUnnamedIdentity: %v", err)
	}

	tests := []struct {
		name string
		cfg  ExposeConfig
	}{
		{
			name: "unnamed without local TLS",
			cfg:  ExposeConfig{Relays: []string{"https://localhost:443"}, Identity: unnamed},
		},
		{
			name: "replica with UDP",
			cfg:  ExposeConfig{Relays: []string{"https://localhost:443"}, Identity: named, Replica: true, UDPEnabled: true},
		},
		{
			name: "replica with TCP",
			cfg:  ExposeConfig{Relays: []string{"https://localhost:443"}, Identity: named, Replica: true, TCPEnabled: true},
		},
		{
			name: "manual cert without key",
			cfg:  ExposeConfig{Relays: []string{"https://localhost:443"}, Identity: named, TLSCertFile: "cert.pem"},
		},
		{
			name: "auto TLS without state dir",
			cfg:  ExposeConfig{Relays: []string{"https://localhost:443"}, Identity: named, AutomaticTLS: true},
		},
		{
			name: "auto TLS with TCP",
			cfg:  ExposeConfig{Relays: []string{"https://localhost:443"}, Identity: named, AutomaticTLS: true, TLSStateDir: t.TempDir(), TCPEnabled: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Expose(context.Background(), tc.cfg); err == nil {
				t.Fatalf("Expose(%s) succeeded, want validation error", tc.name)
			}
		})
	}
}

func TestReplicaModeNegotiationAndConflictRetry(t *testing.T) {
	identity, err := IdentityFromPrivateKey("alice", scalarOnePrivateKey)
	if err != nil {
		t.Fatalf("IdentityFromPrivateKey: %v", err)
	}

	var mu sync.Mutex
	challengeAttempts := 0
	var relay *protocolTestTLSServer
	relay = newProtocolTestTLSServer(t, []string{"localhost", "alice.localhost"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathDomain:
			protocolTestWriteOK(t, w, domainResponse{
				ProtocolVersion:           ProtocolVersion,
				SupportedProtocolVersions: []string{ProtocolVersion},
				Replicas:                  true,
			})
		case pathRegisterChallenge:
			var req challengeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode challenge: %v", err)
				return
			}
			if !req.Replica {
				t.Errorf("expected challenge.Replica = true")
			}
			mu.Lock()
			challengeAttempts++
			attempt := challengeAttempts
			mu.Unlock()
			if attempt == 1 {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(apiEnvelope{
					OK:    false,
					Error: &apiErrorBody{Code: "replica_conflict", Message: "exclusive lease active"},
				})
				return
			}
			now := time.Now().UTC().Truncate(time.Second)
			exp := now.Add(2 * time.Minute)
			msg, _ := formatSIWEMessage(siweMessage{
				Domain:    strings.TrimPrefix(relay.URL, "https://"),
				Address:   scalarOneAddress,
				URI:       relay.URL + pathRegister,
				Statement: registerLeaseSIWEStatement,
				Nonce:     "replicanonce1",
				RequestID: "rch-replica",
				IssuedAt:  now,
				ExpiresAt: exp,
			})
			protocolTestWriteOK(t, w, challengeResponse{
				Replica:     true,
				ChallengeID: "rch-replica",
				ExpiresAt:   exp,
				SIWEMessage: msg,
			})
		case pathRegister:
			exp := time.Now().Add(5 * time.Minute)
			protocolTestWriteOK(t, w, registerResponse{
				Identity:    identityRef{Name: "alice", Address: scalarOneAddress},
				Hostname:    "alice.localhost",
				AliasEpoch:  "epoch-42",
				ExpiresAt:   exp,
				AccessToken: "replica-token",
				ReverseEndpoint: reverseEndpoint{
					URL:        relay.URL + pathConnect,
					Capability: "cap-replica",
					ExpiresAt:  exp,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))

	supervisor, err := newRelaySupervisor(relay.URL, identity, testRelayTimings())
	if err != nil {
		t.Fatalf("newRelaySupervisor: %v", err)
	}
	supervisor.replica = true
	supervisor.keyless = testKeylessProvider{}
	defer supervisor.resetControl()
	if err := supervisor.initializeControl(context.Background()); err != nil {
		t.Fatalf("initializeControl: %v", err)
	}
	if err := supervisor.registerLeaseWithRetry(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("registerLeaseWithRetry: %v", err)
	}
	if challengeAttempts != 2 {
		t.Fatalf("challengeAttempts = %d, want 2 (1 replica_conflict retry + 1 success)", challengeAttempts)
	}
	info := supervisor.currentReadyInfo()
	if info.aliasEpoch != "epoch-42" {
		t.Fatalf("AliasEpoch = %q, want epoch-42", info.aliasEpoch)
	}
	if tenant := supervisor.clearLease(); tenant != nil {
		_ = tenant.close()
	}
}

func TestLocalTLSAutomaticCSRAndUnnamedIdentity(t *testing.T) {
	identity, err := UnnamedIdentityFromPrivateKey(scalarOnePrivateKey)
	if err != nil {
		t.Fatalf("UnnamedIdentityFromPrivateKey: %v", err)
	}
	canonicalHost, err := LocalTLSHostname("", identity.Address(), "localhost")
	if err != nil {
		t.Fatalf("LocalTLSHostname: %v", err)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Tenant CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	var relay *protocolTestTLSServer
	certIssued := make(chan struct{}, 1)
	relay = newProtocolTestTLSServer(t, []string{"localhost"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathDomain:
			protocolTestWriteOK(t, w, domainResponse{
				ProtocolVersion:           ProtocolVersion,
				SupportedProtocolVersions: []string{ProtocolVersion},
				Namespaces:                []string{LocalTLSNamespaceV1},
			})
		case pathRegisterChallenge:
			var req challengeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode challenge: %v", err)
				return
			}
			if req.Namespace != LocalTLSNamespaceV1 {
				t.Errorf("challenge namespace = %q, want %q", req.Namespace, LocalTLSNamespaceV1)
			}
			now := time.Now().UTC().Truncate(time.Second)
			exp := now.Add(2 * time.Minute)
			msg, _ := formatSIWEMessage(siweMessage{
				Domain:    strings.TrimPrefix(relay.URL, "https://"),
				Address:   identity.Address(),
				URI:       relay.URL + pathRegister,
				Statement: registerLeaseSIWEStatement,
				Nonce:     "localtlsnonce1",
				RequestID: "rch-localtls",
				IssuedAt:  now,
				ExpiresAt: exp,
			})
			protocolTestWriteOK(t, w, challengeResponse{
				ChallengeID: "rch-localtls",
				ExpiresAt:   exp,
				SIWEMessage: msg,
			})
		case pathRegister:
			exp := time.Now().Add(5 * time.Minute)
			protocolTestWriteOK(t, w, registerResponse{
				Namespace:         LocalTLSNamespaceV1,
				Identity:          identityRef{Address: identity.Address()},
				CanonicalHostname: canonicalHost,
				ExpiresAt:         exp,
				AccessToken:       "localtls-token",
				ReverseEndpoint: reverseEndpoint{
					URL:        relay.URL + pathConnect,
					Capability: "cap-localtls",
					ExpiresAt:  exp,
				},
			})
		case pathTenantCertificate:
			if r.Header.Get(accessTokenHeader) != "localtls-token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var csrReq tenantCertificateRequest
			if err := json.NewDecoder(r.Body).Decode(&csrReq); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			block, _ := pem.Decode([]byte(csrReq.CSRPEM))
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			leafTmpl := &x509.Certificate{
				SerialNumber: big.NewInt(2),
				DNSNames:     csr.DNSNames,
				NotBefore:    time.Now().Add(-time.Minute),
				NotAfter:     time.Now().Add(24 * time.Hour),
				KeyUsage:     x509.KeyUsageDigitalSignature,
				ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}
			leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, csr.PublicKey, caKey)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			chainPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})) +
				string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
			protocolTestWriteOK(t, w, tenantCertificateResponse{CertificatePEM: chainPEM})
			select {
			case certIssued <- struct{}{}:
			default:
			}
		default:
			http.NotFound(w, r)
		}
	}))

	stateDir := filepath.Join(t.TempDir(), "certs")
	supervisor, err := newRelaySupervisor(relay.URL, identity, testRelayTimings())
	if err != nil {
		t.Fatalf("newRelaySupervisor: %v", err)
	}
	supervisor.automaticTLS = true
	supervisor.tlsStateDir = stateDir
	supervisor.tlsRequireLocal = true
	defer supervisor.resetControl()

	readyCh := make(chan relayReadyInfo, 1)
	if err := supervisor.initializeControl(context.Background()); err != nil {
		t.Fatalf("initializeControl: %v", err)
	}
	if err := supervisor.registerLeaseWithRetry(context.Background(), func(info relayReadyInfo) {
		select {
		case readyCh <- info:
		default:
		}
	}, nil, nil); err != nil {
		t.Fatalf("registerLeaseWithRetry: %v", err)
	}
	defer func() {
		if tenant := supervisor.clearLease(); tenant != nil {
			_ = tenant.close()
		}
	}()

	select {
	case <-certIssued:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for automatic CSR issuance")
	}
	select {
	case info := <-readyCh:
		if !strings.Contains(info.publicURL, canonicalHost) {
			t.Fatalf("ready PublicURL = %q, want canonical %q", info.publicURL, canonicalHost)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for local TLS ready callback")
	}
	if !supervisor.isLocalTLSReady() {
		t.Fatal("expected supervisor.isLocalTLSReady() = true")
	}

	// Verify bundle file was persisted with private permissions.
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected persisted certificate bundle in %s: %v", stateDir, err)
	}
}

func TestRawTCPMarkerPassthrough(t *testing.T) {
	identity, err := IdentityFromPrivateKey("tcpapp", scalarOnePrivateKey)
	if err != nil {
		t.Fatalf("IdentityFromPrivateKey: %v", err)
	}
	supervisor, err := newRelaySupervisor("https://localhost:8443", identity, testRelayTimings())
	if err != nil {
		t.Fatalf("newRelaySupervisor: %v", err)
	}
	supervisor.tcpEnabled = true

	clientSide, relaySide := net.Pipe()
	defer clientSide.Close()

	accepted := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		transferred, claimed, err := supervisor.runReverseSession(context.Background(), relaySide, func(_ context.Context, c net.Conn) bool {
			accepted <- c
			return true
		})
		if err != nil || !transferred || !claimed {
			errCh <- fmt.Errorf("runReverseSession = (%v, %v, %v)", transferred, claimed, err)
			return
		}
		errCh <- nil
	}()

	go func() {
		_, _ = clientSide.Write([]byte{markerRaw})
		_, _ = clientSide.Write([]byte("hello raw tcp\n"))
	}()

	var rawConn net.Conn
	select {
	case rawConn = <-accepted:
		defer rawConn.Close()
	case err := <-errCh:
		t.Fatalf("unexpected error before accept: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for raw TCP conn")
	}

	line, err := bufio.NewReader(rawConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read raw TCP conn: %v", err)
	}
	if line != "hello raw tcp\n" {
		t.Fatalf("raw TCP payload = %q, want %q", line, "hello raw tcp\n")
	}
	if err := <-errCh; err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("runReverseSession error: %v", err)
	}
}
