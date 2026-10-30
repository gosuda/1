package crypto

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"filippo.io/edwards25519"
	"filippo.io/edwards25519/field"

	"net/netip"

	"gosuda.org/grt/internal/wire"
)

func TestXChaChaCachedMatchesDirect(t *testing.T) {
	var key, iv [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(iv[:]); err != nil {
		t.Fatal(err)
	}
	e, err := NewEpochAEAD(CipherXChaCha20Poly1305, key[:], iv[:24])
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("hello grt world, this is a test plaintext")
	aad := make([]byte, 32)
	rand.Read(aad)
	for _, pn := range []uint64{0, 1, 2, 0xdeadbeef, 1 << 40} {
		// Direct construction: nonce = iv ^ (Z(16)||be64(pn))
		var nonce [24]byte
		copy(nonce[:], iv[:24])
		v := uint64(pn)
		for i := 0; i < 8; i++ {
			nonce[16+i] ^= byte(v >> (56 - 8*i))
		}
		ctDirect, err := SealXChaChaDirect(key[:], nonce[:], pt, aad, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctCached := e.Seal(pn, aad, pt, nil)
		if !bytes.Equal(ctDirect, ctCached) {
			t.Fatalf("pn %d: cached and direct XChaCha differ", pn)
		}
		got, err := e.Open(pn, aad, ctCached, nil)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("pn %d: open failed %v", pn, err)
		}
	}
}

func TestAESGCMNonceDerivation(t *testing.T) {
	var key [32]byte
	var iv [12]byte
	rand.Read(key[:])
	rand.Read(iv[:])
	e, err := NewEpochAEAD(CipherAES256GCM, key[:], iv[:])
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("test payload")
	aad := make([]byte, 32)
	ct := e.Seal(42, aad, pt, nil)
	got, err := e.Open(42, aad, ct, nil)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("AES-GCM round trip failed: %v", err)
	}
	if _, err := e.Open(43, aad, ct, nil); err == nil {
		t.Fatal("wrong pn must fail")
	}
}

func TestKDFVectors(t *testing.T) {
	// L() vs ExpandRaw with manually constructed info string.
	secret := bytes.Repeat([]byte{0x0b}, 32)
	label := "root"
	ctx := []byte{1, 2, 3, 4}
	got := L(secret, label, ctx, 32)
	var info []byte
	info = append(info, 0, 32, byte(len("GRT11 "+label)))
	info = append(info, "GRT11 "+label...)
	info = append(info, 0, byte(len(ctx)))
	info = append(info, ctx...)
	want := ExpandRaw(secret, string(info), 32)
	if !bytes.Equal(got, want) {
		t.Fatal("L does not match HKDF-Expand construction")
	}
}

func TestXEdDSARoundTrip(t *testing.T) {
	for i := 0; i < 8; i++ {
		k, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		priv := k.Bytes()
		pub := k.PublicKey().Bytes()
		msg := []byte("test message " + string(rune('0'+i)))
		var z [64]byte
		rand.Read(z[:])
		sig, err := XEdDSASign(priv, msg, z[:])
		if err != nil {
			t.Fatal(err)
		}
		if !XEdDSAVerify(pub, msg, sig[:]) {
			t.Fatalf("iteration %d: signature did not verify", i)
		}
		if XEdDSAVerify(pub, append(msg, 'x'), sig[:]) {
			t.Fatal("wrong message verified")
		}
		// Deterministic given (priv, msg, z)
		sig2, _ := XEdDSASign(priv, msg, z[:])
		if sig != sig2 {
			t.Fatal("XEdDSA not deterministic")
		}
	}
}

// TestXEdDSAKnownVector checks the exact R10 construction against the
// reference vector from curve25519-java's xeddsa_fast_test: private key
// all-zero except byte 8 = 189 (clamped in place by the reference), a
// 200-byte all-zero message, and 64 zero random bytes must produce the
// published signature. This pins the hash1 domain prefix and the raw
// clamped-scalar nonce input.
func TestXEdDSAKnownVector(t *testing.T) {
	var priv [32]byte
	priv[8] = 0xbd
	priv[31] = 0x40 // sc_clamp was applied to the published key
	msg := make([]byte, 200)
	random := make([]byte, 64)
	sig, err := XEdDSASign(priv[:], msg, random)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString("11c7f3e6c4df9e8a5150e1db3b30f92de3a3b3aa438656545fa7390f4bcc7bb2" +
		"6c431d9e90643e4f0eaa0e9c557766fa69ada576d63dcaf2ac326c11d0b97702")
	if !bytes.Equal(sig[:], want) {
		t.Fatalf("signature = %x, want %x", sig, want)
	}
	k, err := ecdh.X25519().NewPrivateKey(priv[:])
	if err != nil {
		t.Fatal(err)
	}
	if !XEdDSAVerify(k.PublicKey().Bytes(), msg, sig[:]) {
		t.Fatal("vector signature failed to verify")
	}
}

// TestXEdDSAEd25519Interop verifies a signature produced by XEdDSASign
// under the standard library Ed25519 verifier: per R10 section 5 an
// XEd25519 signature is a valid Ed25519 signature once the Montgomery
// public key is mapped to its Edwards image with sign bit zero. This
// independently pins the challenge hash h = SHA512(R || A || M).
func TestXEdDSAEd25519Interop(t *testing.T) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("interoperability check")
	sig, err := XEdDSASignRand(k.Bytes(), msg)
	if err != nil {
		t.Fatal(err)
	}
	u, err := new(field.Element).SetBytes(k.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	one := new(field.Element).One()
	den := new(field.Element).Add(u, one)
	den.Invert(den)
	num := new(field.Element).Subtract(u, one)
	yb := new(field.Element).Multiply(num, den).Bytes()
	yb[31] &= 0x7f
	if _, err := new(edwards25519.Point).SetBytes(yb); err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(yb), msg, sig[:]) {
		t.Fatal("stdlib Ed25519 rejected XEdDSA signature")
	}
}

func TestXEdDSASignCanonicalness(t *testing.T) {
	// Signatures from the same key must verify under the X25519 public key
	// regardless of which Edwards sign the key maps to.
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	msg := []byte("m")
	sig, err := XEdDSASignRand(k.Bytes(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if !XEdDSAVerify(k.PublicKey().Bytes(), msg, sig[:]) {
		t.Fatal("verify failed")
	}
	// Tampered signature must fail.
	sig[0] ^= 1
	if XEdDSAVerify(k.PublicKey().Bytes(), msg, sig[:]) {
		t.Fatal("tampered signature verified")
	}
}

func TestMLKEMRoundTrip(t *testing.T) {
	kp, err := GenerateMLKEM768()
	if err != nil {
		t.Fatal(err)
	}
	ct, ss, err := MLKEM768Encaps(kp.EK[:])
	if err != nil {
		t.Fatal(err)
	}
	ss2 := MLKEM768Decaps(kp.Seed, ct[:])
	if ss != ss2 {
		t.Fatal("ML-KEM shared secrets differ")
	}
	// Corrupted ciphertext must give a different (implicit-rejection) secret.
	ct[0] ^= 1
	ss3 := MLKEM768Decaps(kp.Seed, ct[:])
	if ss3 == ss {
		t.Fatal("corrupted CT produced same secret")
	}
}

func TestTicketRoundTrip(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	var plain [TicketPlainBytes]byte
	rand.Read(plain[:])
	var nonce [24]byte
	rand.Read(nonce[:])
	tkt := SealTicket(key, 7, nonce, plain)
	got, err := OpenTicket(key, tkt[:])
	if err != nil {
		t.Fatal(err)
	}
	dec := EncodeTicketPlain(&got)
	if !bytes.Equal(dec[:], plain[:]) {
		t.Fatal("ticket round trip mismatch")
	}
	// Wrong key must fail.
	key[0] ^= 1
	if _, err := OpenTicket(key, tkt[:]); err == nil {
		t.Fatal("wrong-key ticket opened")
	}
}

func TestTokenServiceDeadKey(t *testing.T) {
	src := netip.MustParseAddrPort("10.0.0.1:1000")
	dst := netip.MustParseAddrPort("10.0.0.2:4433")
	s := NewTokenService(nil, []byte("dom"))
	// A service whose issuing key was never seeded after an entropy
	// failure must fail closed: issuing refuses and a token claiming the
	// unseeded key never verifies — even with a valid zero-key HMAC.
	s.dead = true
	if _, ok := s.IssueRetry(1, 2, [32]byte{}, src, dst); ok {
		t.Fatal("dead service issued a retry token")
	}
	if _, ok := s.IssueReuse(src, dst); ok {
		t.Fatal("dead service issued a reuse token")
	}
	var tok wire.Token
	tok.KeyID = s.curID
	tok.Kind = wire.TokenKindReuse
	tok.IssuedUnixS = s.now()
	tok.ExpiresUnixS = tok.IssuedUnixS + 60
	var head [32]byte
	wire.EncodeToken(head[:], &tok)
	in := wire.TokenCommonInput(nil, head[:], src, dst, s.domain)
	in = append(in, make([]byte, 40)...)
	m := hmac.New(sha256.New, make([]byte, 32)) // forged under the zero key
	m.Write(in)
	copy(tok.Tag[:], m.Sum(nil))
	var tb [wire.TokenBytes]byte
	wire.EncodeToken(tb[:], &tok)
	if s.ValidateToken(tb, src, dst, [32]byte{}, 0, 0) {
		t.Fatal("dead service verified a forged token under the zero key")
	}
}
