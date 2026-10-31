package crypto

import (
	"crypto/ecdh"
	"crypto/sha512"
	"math/big"
	"slices"
	"testing"

	"filippo.io/edwards25519"
)

// xeddsaTestKey returns a fixed Montgomery key pair and the effective
// signing scalar a with the Edwards sign bit of a·B forced to zero, as in
// XEdDSASign.
func xeddsaTestKey(t *testing.T) (priv, pub []byte, a *edwards25519.Scalar, encA []byte) {
	t.Helper()
	priv = make([]byte, 32)
	for i := range priv {
		priv[i] = byte(i + 1)
	}
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	a, err = edwards25519.NewScalar().SetBytesWithClamping(priv)
	if err != nil {
		t.Fatal(err)
	}
	encA = new(edwards25519.Point).ScalarBaseMult(a).Bytes()
	if encA[31]&0x80 != 0 {
		a.Negate(a)
		encA[31] &= 0x7f
	}
	return priv, k.PublicKey().Bytes(), a, encA
}

// TestXEdDSAVerifyRejectsNonCanonicalR forges signatures whose R is the
// identity point under a non-canonical encoding. With r = 0 and
// s = SHA512(R || A || M)·a, the group equation s·B = R + h·A holds, so
// only the canonical-encoding check can reject them.
func TestXEdDSAVerifyRejectsNonCanonicalR(t *testing.T) {
	_, pub, a, encA := xeddsaTestKey(t)
	msg := []byte("malleability")
	identityYPlusP := [32]byte{0xee} // y = p + 1
	for i := 1; i < 31; i++ {
		identityYPlusP[i] = 0xff
	}
	identityYPlusP[31] = 0x7f
	identityNegZero := [32]byte{0x01} // y = 1 with x sign bit set on x = 0
	identityNegZero[31] = 0x80
	cases := []struct {
		name string
		encR [32]byte
	}{
		{"y encoded as p+1", identityYPlusP},
		{"negative zero x", identityNegZero},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := new(edwards25519.Point).SetBytes(tc.encR[:]); err != nil {
				t.Fatalf("precondition: decoder rejects %x outright: %v", tc.encR, err)
			}
			h := sha512.New()
			h.Write(tc.encR[:])
			h.Write(encA)
			h.Write(msg)
			k, err := edwards25519.NewScalar().SetUniformBytes(h.Sum(nil))
			if err != nil {
				t.Fatal(err)
			}
			s := edwards25519.NewScalar().Multiply(k, a)
			sig := slices.Concat(tc.encR[:], s.Bytes())
			if XEdDSAVerify(pub, msg, sig) {
				t.Fatalf("non-canonical R %x verified", tc.encR)
			}
		})
	}
}

// TestXEdDSAVerifyRejectsUnreducedS adds the group order q to a valid s:
// the group equation still holds, so acceptance would make signatures
// malleable.
func TestXEdDSAVerifyRejectsUnreducedS(t *testing.T) {
	priv, pub, _, _ := xeddsaTestKey(t)
	msg := []byte("malleability")
	sig, err := XEdDSASign(priv, msg, make([]byte, 64))
	if err != nil {
		t.Fatal(err)
	}
	if !XEdDSAVerify(pub, msg, sig[:]) {
		t.Fatal("precondition: valid signature rejected")
	}
	q, _ := new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)
	sLE := slices.Clone(sig[32:])
	slices.Reverse(sLE)
	sPlusQ := new(big.Int).Add(new(big.Int).SetBytes(sLE), q).FillBytes(make([]byte, 32))
	slices.Reverse(sPlusQ)
	if XEdDSAVerify(pub, msg, slices.Concat(sig[:32], sPlusQ)) {
		t.Fatal("s + q verified")
	}
}

func TestXEdDSARejectsMalformedInputs(t *testing.T) {
	priv, pub, _, _ := xeddsaTestKey(t)
	if _, err := XEdDSASign(priv[:31], nil, make([]byte, 64)); err == nil {
		t.Fatal("31-byte private key signed")
	}
	if _, err := XEdDSASign(priv, nil, make([]byte, 63)); err == nil {
		t.Fatal("63-byte random input signed")
	}
	sig, err := XEdDSASign(priv, nil, make([]byte, 64))
	if err != nil {
		t.Fatal(err)
	}
	if XEdDSAVerify(pub, nil, sig[:63]) {
		t.Fatal("63-byte signature verified")
	}
	if XEdDSAVerify(pub[:31], nil, sig[:]) {
		t.Fatal("31-byte public key verified")
	}
	// u = 0 maps to a small-order Edwards point.
	if XEdDSAPublicIsValid(make([]byte, 32)) {
		t.Fatal("u = 0 accepted as identity key")
	}
	if !XEdDSAPublicIsValid(pub) {
		t.Fatal("valid public key rejected")
	}
	if XEdDSAPublicIsValid(pub[:31]) {
		t.Fatal("31-byte public key accepted")
	}
}
