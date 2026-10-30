package crypto

import (
	"crypto/rand"
	"crypto/sha512"
	"errors"

	"filippo.io/edwards25519"
	"filippo.io/edwards25519/field"
)

// (See XEdDSAVerify for verifier-side validation; signature failure is
// reported as a boolean so callers cannot branch on internal causes.)

// XEd25519 is the XEdDSA signature scheme over the Curve25519 profile
// (spec section 4.1, [R10]). Keys are ordinary Curve25519 key material: the
// private key is a 32-byte X25519 scalar seed; the public identity is the
// X25519 Montgomery u-coordinate. Signatures are 64-byte Ed25519-shaped
// values that verify against the Edwards point recovered from u with sign
// bit 0.
//
// XEdDSA's internal hash is SHA-512 per the Curve25519 profile; it is not
// replaced by SHA-256 (spec section 4.1).

var (
	errBadPoint  = errors.New("grt: invalid Edwards point image")
	errInputSize = errors.New("grt: XEdDSA input size")
)

// xeddsaHash1Prefix is the XEdDSA domain separator for hash1 (R10
// section 2.5): the b-bit integer 2^b - 1 - i with i = 1, little-endian,
// which for Curve25519 (b = 256) is 0xFE followed by 31 bytes of 0xFF.
var xeddsaHash1Prefix = [32]byte{
	0xFE, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
}

// montgomeryToEdwards converts an X25519 Montgomery u-coordinate to the
// Edwards point with sign bit 0 (XEdDSA convert_mont). Fails if u is
// noncanonical, the point is not on the curve, or the point is of small
// order.
func montgomeryToEdwards(u []byte) (*edwards25519.Point, error) {
	if !CanonicalX25519Public(u) {
		return nil, errBadPoint
	}
	var ub [32]byte
	copy(ub[:], u)
	ufe, err := new(field.Element).SetBytes(ub[:])
	if err != nil {
		return nil, err
	}
	one := new(field.Element).One()
	num := new(field.Element).Subtract(ufe, one)
	den := new(field.Element).Add(ufe, one)
	den.Invert(den)
	y := num.Multiply(num, den)
	yb := y.Bytes()
	// Force sign bit 0 (XEdDSA convention: the non-negative x root).
	yb[31] &= 0x7f
	pt, err := edwards25519.NewIdentityPoint().SetBytes(yb)
	if err != nil {
		return nil, err
	}
	// Reject small-order A' (complete verifier validation, spec 4.2):
	// [8]A' must not be the identity.
	var eight [32]byte
	eight[0] = 8
	es, _ := edwards25519.NewScalar().SetCanonicalBytes(eight[:])
	mul := new(edwards25519.Point).ScalarMult(es, pt)
	if mul.Equal(edwards25519.NewIdentityPoint()) == 1 {
		return nil, errBadPoint
	}
	return pt, nil
}

// XEdDSASign produces an XEd25519 signature over msg with the Curve25519
// private key priv and a fresh 64-byte random input (spec section 4.2).
// The signature is deterministic given (priv, msg, random).
func XEdDSASign(priv []byte, msg, random []byte) ([64]byte, error) {
	var sig [64]byte
	if len(priv) != 32 || len(random) != 64 {
		return sig, errInputSize
	}
	// a = clamped private scalar; A = a*B. Force the Edwards x sign bit to
	// 0 by negating (a, A) when needed; u(A) is unchanged by negation.
	a, err := edwards25519.NewScalar().SetBytesWithClamping(priv)
	if err != nil {
		return sig, err
	}
	A := new(edwards25519.Point).ScalarBaseMult(a)
	encA := A.Bytes()
	// The nonce hash input a is the raw clamped private scalar bytes
	// while the Edwards sign bit is already zero, and the canonical
	// -a (mod q) encoding when the point had to be negated — matching
	// the reference crypto_sign_modified.
	var aKey [32]byte
	copy(aKey[:], priv)
	aKey[0] &= 0xf8
	aKey[31] &= 0x7f
	aKey[31] |= 0x40
	if encA[31]&0x80 != 0 {
		a = new(edwards25519.Scalar).Negate(a)
		copy(aKey[:], a.Bytes())
		encA[31] &= 0x7f
	}
	// r = hash1(a || M || Z) mod q
	h := sha512.New()
	h.Write(xeddsaHash1Prefix[:])
	h.Write(aKey[:])
	h.Write(msg)
	h.Write(random)
	r, err := new(edwards25519.Scalar).SetUniformBytes(h.Sum(nil))
	if err != nil {
		return sig, err
	}
	encR := new(edwards25519.Point).ScalarBaseMult(r).Bytes()
	// h = SHA512(R || A || M) mod q
	h.Reset()
	h.Write(encR)
	h.Write(encA)
	h.Write(msg)
	k, err := new(edwards25519.Scalar).SetUniformBytes(h.Sum(nil))
	if err != nil {
		return sig, err
	}
	// s = r + k*a mod q
	s := new(edwards25519.Scalar).MultiplyAdd(k, a, r)
	copy(sig[:32], encR)
	copy(sig[32:], s.Bytes())
	return sig, nil
}

// XEdDSASignRand signs with internally generated fresh randomness.
func XEdDSASignRand(priv []byte, msg []byte) ([64]byte, error) {
	var z [64]byte
	if _, err := rand.Read(z[:]); err != nil {
		return [64]byte{}, err
	}
	return XEdDSASign(priv, msg, z[:])
}

// XEdDSAVerify verifies an XEd25519 signature against the Montgomery public
// key u (the peer's X25519 public key bytes). It enforces canonical u, the
// full point/curve checks, and the group equation s·B = R + h·A'.
func XEdDSAVerify(u []byte, msg, sig []byte) bool {
	if len(sig) != 64 || len(u) != 32 {
		return false
	}
	A, err := montgomeryToEdwards(u)
	if err != nil {
		return false
	}
	R, err := edwards25519.NewIdentityPoint().SetBytes(sig[:32])
	if err != nil {
		return false
	}
	s, err := edwards25519.NewScalar().SetCanonicalBytes(sig[32:])
	if err != nil {
		return false
	}
	encA := A.Bytes() // sign bit already 0 by construction
	h := sha512.New()
	h.Write(sig[:32])
	h.Write(encA)
	h.Write(msg)
	k, err := new(edwards25519.Scalar).SetUniformBytes(h.Sum(nil))
	if err != nil {
		return false
	}
	// Check s·B == R + h·A'.
	lhs := new(edwards25519.Point).ScalarBaseMult(s)
	rhs := new(edwards25519.Point).Add(R, new(edwards25519.Point).ScalarMult(k, A))
	return lhs.Equal(rhs) == 1
}

// XEdDSAPublicIsValid validates a public identity key: canonical Montgomery
// encoding and a usable (non-small-order) Edwards image.
func XEdDSAPublicIsValid(u []byte) bool {
	if len(u) != 32 {
		return false
	}
	_, err := montgomeryToEdwards(u)
	return err == nil
}
