package crypto

import (
	"encoding/binary"
	"testing"

	"gosuda.org/grt/internal/wire"
)

type credKey struct {
	priv [32]byte
	pub  [32]byte
}

func newCredKey(t *testing.T, seed byte) credKey {
	t.Helper()
	var priv [32]byte
	for i := range priv {
		priv[i] = seed + byte(i)
	}
	kp, err := X25519KeyPairFromPriv(priv[:])
	if err != nil {
		t.Fatal(err)
	}
	return credKey{priv: kp.Priv, pub: kp.Pub}
}

type credSpec struct {
	version   uint16
	usage     uint16
	issuer    credKey
	subject   [32]byte
	service   [32]byte
	notBefore uint64
	notAfter  uint64
}

func buildCred(t *testing.T, s credSpec) [wire.CredentialBytes]byte {
	t.Helper()
	var b [wire.CredentialBytes]byte
	binary.BigEndian.PutUint16(b[0:], s.version)
	binary.BigEndian.PutUint16(b[2:], s.usage)
	copy(b[4:], s.issuer.pub[:])
	copy(b[36:], s.subject[:])
	copy(b[68:], s.service[:])
	binary.BigEndian.PutUint64(b[100:], s.notBefore)
	binary.BigEndian.PutUint64(b[108:], s.notAfter)
	sig, err := XEdDSASign(s.issuer.priv[:], CredentialMessage(b[:wire.CredentialPrefix]), make([]byte, 64))
	if err != nil {
		t.Fatal(err)
	}
	copy(b[132:], sig[:])
	return b
}

func TestVerifyCredentialChain(t *testing.T) {
	const now = 1_000_000
	anchor, mid, leaf := newCredKey(t, 1), newCredKey(t, 50), newCredKey(t, 100)
	svc := ServiceHash("example.test", [16]byte{1})
	spec := func(usage uint16, issuer credKey, subject [32]byte) credSpec {
		return credSpec{version: 1, usage: usage, issuer: issuer, subject: subject,
			service: svc, notBefore: now - 10, notAfter: now + 10}
	}
	leafByAnchor := buildCred(t, spec(1, anchor, leaf.pub))
	leafByMid := buildCred(t, spec(1, mid, leaf.pub))
	midByAnchor := buildCred(t, spec(2, anchor, mid.pub))
	anchors := [][32]byte{anchor.pub}
	type chain = [][wire.CredentialBytes]byte
	leafWith := func(f func(*credSpec)) [wire.CredentialBytes]byte {
		s := spec(1, anchor, leaf.pub)
		f(&s)
		return buildCred(t, s)
	}
	badSig := leafByAnchor
	badSig[140] ^= 1
	cases := []struct {
		name  string
		creds chain
		now   int64
		want  bool
	}{
		{"leaf signed by anchor", chain{leafByAnchor}, now, true},
		{"leaf via intermediate", chain{leafByMid, midByAnchor}, now, true},
		{"empty chain", chain{}, now, false},
		{"longer than MaxCredentials", chain{leafByMid, midByAnchor, midByAnchor, midByAnchor, midByAnchor}, now, false},
		{"negative clock", chain{leafByAnchor}, -1, false},
		{"before notBefore", chain{leafByAnchor}, now - 11, false},
		{"after notAfter", chain{leafByAnchor}, now + 11, false},
		{"tampered signature", chain{badSig}, now, false},
		{"wrong service hash", chain{leafWith(func(s *credSpec) { s.service[0] ^= 1 })}, now, false},
		{"leaf without leaf usage", chain{leafWith(func(s *credSpec) { s.usage = 2 })}, now, false},
		{"issuer without issue usage", chain{leafByMid, buildCred(t, spec(1, anchor, mid.pub))}, now, false},
		{"unknown usage bit", chain{leafWith(func(s *credSpec) { s.usage = 5 })}, now, false},
		{"record version 2", chain{leafWith(func(s *credSpec) { s.version = 2 })}, now, false},
		{"inverted validity window", chain{leafWith(func(s *credSpec) { s.notBefore, s.notAfter = now+1, now-1 })}, now, false},
		{"leaf subject is not the service key", chain{leafWith(func(s *credSpec) { s.subject = mid.pub })}, now, false},
		{"broken issuer link", chain{leafByAnchor, midByAnchor}, now, false},
		{"untrusted root", chain{leafByMid}, now, false},
		{"duplicate subject", chain{leafByMid, buildCred(t, spec(3, mid, mid.pub)), midByAnchor}, now, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyCredentialChain(tc.creds, leaf.pub, anchors, svc, tc.now); got != tc.want {
				t.Fatalf("VerifyCredentialChain = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseCredentialRejectsWrongLength(t *testing.T) {
	if _, ok := ParseCredential(make([]byte, wire.CredentialBytes-1)); ok {
		t.Fatal("195-byte credential parsed")
	}
}
