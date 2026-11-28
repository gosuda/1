package certmon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func generateTestCert(t *testing.T, dnsNames ...string) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, der
}

// startFakeDNSServer runs a local UDP DNS server that answers CAA/CNAME queries.
func startFakeDNSServer(t *testing.T, handler func(qname string, qtype uint16) (rcode uint8, answers []fakeRR)) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req, err := decodeDNSMessage(buf[:n])
			if err != nil {
				continue
			}
			rcode, answers := handler(req.qname, req.qtype)
			resp := buildFakeDNSReply(req.id, req.qname, req.qtype, rcode, answers)
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

type fakeRR struct {
	name  string
	rtype uint16
	rdata []byte
}

func encodeNameRaw(name string) []byte {
	var out []byte
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			continue
		}
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0)
}

func encodeCAARData(flags uint8, tag, value string) []byte {
	out := []byte{flags, byte(len(tag))}
	out = append(out, tag...)
	out = append(out, value...)
	return out
}

func buildFakeDNSReply(id uint16, qname string, qtype uint16, rcode uint8, answers []fakeRR) []byte {
	buf := make([]byte, 12)
	binary.BigEndian.PutUint16(buf[0:2], id)
	flags := uint16(0x8180) | uint16(rcode&0x0F) // QR=1, RD=1, RA=1
	binary.BigEndian.PutUint16(buf[2:4], flags)
	binary.BigEndian.PutUint16(buf[4:6], 1)
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(answers)))
	buf = append(buf, encodeNameRaw(qname)...)
	buf = binary.BigEndian.AppendUint16(buf, qtype)
	buf = binary.BigEndian.AppendUint16(buf, dnsClassIN)
	for _, rr := range answers {
		buf = append(buf, encodeNameRaw(rr.name)...)
		buf = binary.BigEndian.AppendUint16(buf, rr.rtype)
		buf = binary.BigEndian.AppendUint16(buf, dnsClassIN)
		buf = binary.BigEndian.AppendUint32(buf, 60)
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(rr.rdata)))
		buf = append(buf, rr.rdata...)
	}
	return buf
}

func TestCAAWalkRFC8659(t *testing.T) {
	// CNAME on tenant.example -> alias.other (empty), so RFC 8659 §3 climbs to example (NOT other).
	resolver := startFakeDNSServer(t, func(qname string, qtype uint16) (uint8, []fakeRR) {
		switch qname {
		case "tenant.example":
			return dnsRcodeOK, []fakeRR{{name: "tenant.example", rtype: dnsTypeCNAME, rdata: encodeNameRaw("alias.other")}}
		case "alias.other":
			return dnsRcodeOK, nil
		case "other":
			return dnsRcodeOK, []fakeRR{{name: "other", rtype: dnsTypeCAA, rdata: encodeCAARData(0, "issue", "wrong.example")}}
		case "example":
			return dnsRcodeOK, []fakeRR{{name: "example", rtype: dnsTypeCAA, rdata: encodeCAARData(0, "ISSUE", "letsencrypt.org")}}
		default:
			return dnsRcodeNXDOM, nil
		}
	})

	m, err := newMonitor("tenant.example", Config{
		StateDir:    t.TempDir(),
		DNSResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	obs := m.observeCAA(context.Background())
	if obs.Error != "" {
		t.Fatalf("observeCAA error: %s", obs.Error)
	}
	if obs.Owner != "example" {
		t.Fatalf("owner = %q, want %q", obs.Owner, "example")
	}
	if len(obs.Records) != 1 || obs.Records[0].Tag != "issue" || obs.Records[0].Value != "letsencrypt.org" {
		t.Fatalf("records = %+v, want letsencrypt.org", obs.Records)
	}
}

func TestMonitorCTFallbackAndUnrecognizedSPKI(t *testing.T) {
	host := "svc.0123456789abcdef0123456789abcdef01234567.relay.example"
	ownCert, ownDER := generateTestCert(t, host)
	rogueCert, rogueDER := generateTestCert(t, host)

	resolver := startFakeDNSServer(t, func(qname string, qtype uint16) (uint8, []fakeRR) {
		if qname == "relay.example" {
			return dnsRcodeOK, []fakeRR{{name: "relay.example", rtype: dnsTypeCAA, rdata: encodeCAARData(0, "issue", "other-ca.example")}}
		}
		return dnsRcodeOK, nil
	})

	// Primary CertSpotter returns 429; fallback crt.sh serves both ownCert and rogueCert.
	csServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer csServer.Close()

	crtShServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("d") {
		case "101":
			_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: ownDER})
			return
		case "102":
			_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: rogueDER})
			return
		}
		_ = json.NewEncoder(w).Encode([]crtShRow{
			{ID: 101},
			{ID: 102},
		})
	}))
	defer crtShServer.Close()

	stateDir := t.TempDir()
	baselinePath := filepath.Join(stateDir, "baseline.json")
	baselineBytes, _ := json.Marshal(Baseline{
		Hostname: host,
		CAA:      []CAARecord{{Flags: 0, Tag: "issue", Value: "letsencrypt.org"}},
	})
	if err := os.WriteFile(baselinePath, baselineBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var warnings []Warning
	done := make(chan struct{}, 2)

	cs := NewCertSpotterSource(csServer.Client(), "tok")
	cs.Endpoint = csServer.URL
	crt := NewCrtShSource(crtShServer.Client())
	crt.Endpoint = crtShServer.URL

	m, err := newMonitor(host, Config{
		StateDir:     stateDir,
		BaselineFile: baselinePath,
		DNSResolver:  resolver,
		CTSources:    []CTSource{cs, crt},
		OnWarning: func(w Warning) {
			mu.Lock()
			warnings = append(warnings, w)
			mu.Unlock()
			select {
			case done <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Register our own local leaf so only rogueCert triggers WarningUnrecognizedSPKI.
	m.rememberEvent(ownDER)
	m.poll(context.Background())

	mu.Lock()
	defer mu.Unlock()
	sawCAADrift := false
	sawRogue := false
	for _, w := range warnings {
		if w.Kind == WarningCAADrift {
			sawCAADrift = true
		}
		if w.Kind == WarningUnrecognizedSPKI {
			if w.SPKISHA256 == sha256Hex(ownCert.RawSubjectPublicKeyInfo) {
				t.Fatalf("unexpected warning for own SPKI %s", w.SPKISHA256)
			}
			if w.SPKISHA256 == sha256Hex(rogueCert.RawSubjectPublicKeyInfo) {
				sawRogue = true
			}
		}
	}
	if !sawCAADrift {
		t.Fatalf("expected WarningCAADrift, got warnings: %+v", warnings)
	}
	if !sawRogue {
		t.Fatalf("expected WarningUnrecognizedSPKI for rogue cert via crt.sh fallback, got warnings: %+v", warnings)
	}
}
