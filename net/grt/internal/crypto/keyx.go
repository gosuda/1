package crypto

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"errors"
)

var (
	ErrNonCanonicalKey = errors.New("grt: noncanonical X25519 public key")
	ErrAllZeroDH       = errors.New("grt: all-zero X25519 shared secret")
	ErrKEMKey          = errors.New("grt: ML-KEM key check failed")
)

// x25519FieldP is p = 2^255-19, little-endian, for the canonical-encoding
// check of spec section 4.2 (top bit clear, coordinate < p).
var x25519FieldP = [32]byte{
	0xec, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f,
}

// CanonicalX25519Public reports whether pk is a canonical X25519 encoding:
// top bit clear and the encoded coordinate < 2^255-19.
func CanonicalX25519Public(pk []byte) bool {
	if len(pk) != 32 || pk[31]&0x80 != 0 {
		return false
	}
	// Little-endian compare pk < p.
	for i := 31; i >= 0; i-- {
		if pk[i] != x25519FieldP[i] {
			return pk[i] < x25519FieldP[i]
		}
	}
	return false // pk == p is not < p
}

// X25519KeyPair is a Curve25519 key pair held as owned byte arrays: the
// scalar is erasable in place, and the stdlib ecdh object is built per
// DH operation (handshake/refresh-cold) so no expanded form outlives
// zeroization by more than the call.
type X25519KeyPair struct {
	Priv [32]byte
	Pub  [32]byte
}

// Zeroize erases the private scalar in place.
func (kp *X25519KeyPair) Zeroize() { *kp = X25519KeyPair{} }

// GenerateX25519 creates a fresh pair.
func GenerateX25519() (X25519KeyPair, error) {
	var kp X25519KeyPair
	if _, err := rand.Read(kp.Priv[:]); err != nil {
		return X25519KeyPair{}, err
	}
	k, err := ecdh.X25519().NewPrivateKey(kp.Priv[:])
	if err != nil {
		return X25519KeyPair{}, err
	}
	copy(kp.Pub[:], k.PublicKey().Bytes())
	return kp, nil
}

// X25519KeyPairFromPriv rebuilds a pair from 32 private bytes.
func X25519KeyPairFromPriv(priv []byte) (X25519KeyPair, error) {
	var kp X25519KeyPair
	if len(priv) != 32 {
		return kp, ErrKEMKey
	}
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return X25519KeyPair{}, err
	}
	copy(kp.Priv[:], priv)
	copy(kp.Pub[:], k.PublicKey().Bytes())
	return kp, nil
}

// X25519 computes the DH shared output, rejecting noncanonical public
// encodings and all-zero results (spec section 4.2).
func X25519(priv [32]byte, pub []byte) ([32]byte, error) {
	var out [32]byte
	if !CanonicalX25519Public(pub) {
		return out, ErrNonCanonicalKey
	}
	sk, err := ecdh.X25519().NewPrivateKey(priv[:])
	if err != nil {
		return out, err
	}
	pk, err := ecdh.X25519().NewPublicKey(pub)
	if err != nil {
		return out, err
	}
	shared, err := sk.ECDH(pk)
	if err != nil {
		return out, err
	}
	copy(out[:], shared)
	// ecdh.X25519 already rejects all-zero shared output; double-check
	// defensively.
	if out == [32]byte{} {
		return out, ErrAllZeroDH
	}
	return out, nil
}

// MLKEM768KeyPair is a fresh ML-KEM-768 key pair; Seed retains the 64-byte
// decapsulation seed (permitted internal storage form, FIPS 203) and the
// expanded decapsulation key is rebuilt per operation — a handshake/
// refresh-cold path — so no expanded secret survives seed zeroization.
type MLKEM768KeyPair struct {
	EK   [1184]byte
	Seed [64]byte
}

// Zeroize erases the decapsulation seed in place.
func (kp *MLKEM768KeyPair) Zeroize() { *kp = MLKEM768KeyPair{} }

// GenerateMLKEM768 creates a fresh pair.
func GenerateMLKEM768() (MLKEM768KeyPair, error) {
	var kp MLKEM768KeyPair
	if _, err := rand.Read(kp.Seed[:]); err != nil {
		return kp, err
	}
	dk, err := mlkem.NewDecapsulationKey768(kp.Seed[:])
	if err != nil {
		return kp, err
	}
	copy(kp.EK[:], dk.EncapsulationKey().Bytes())
	return kp, nil
}

// MLKEM768Encaps parses ek (with FIPS 203 key checks) and encapsulates.
// Returns (ciphertext, shared secret).
func MLKEM768Encaps(ek []byte) ([1088]byte, [32]byte, error) {
	var ct [1088]byte
	var ss [32]byte
	k, err := mlkem.NewEncapsulationKey768(ek)
	if err != nil {
		return ct, ss, ErrKEMKey
	}
	ssB, ctB := k.Encapsulate()
	copy(ss[:], ssB)
	copy(ct[:], ctB)
	return ct, ss, nil
}

// MLKEM768Decaps decapsulates under the 64-byte seed. Implicit rejection
// stays internal per spec section 4.2 — callers must not expose a
// decapsulation-error oracle.
func MLKEM768Decaps(seed [64]byte, ct []byte) [32]byte {
	var ss [32]byte
	dk, err := mlkem.NewDecapsulationKey768(seed[:])
	if err != nil {
		return ss
	}
	sh, err := dk.Decapsulate(ct)
	if err != nil {
		// Keep implicit rejection semantics: return a stable dummy value
		// rather than an error the caller could leak as an oracle.
		return ss
	}
	copy(ss[:], sh)
	return ss
}

// EncodeEC builds EncodeEC(pk) = 0x01 || pk32 (spec section 4.3).
func EncodeEC(pk [32]byte) [33]byte {
	var out [33]byte
	out[0] = 0x01
	copy(out[1:], pk[:])
	return out
}

// EncodeKEM builds EncodeKEM(pk) = 0x02 || pk1184.
func EncodeKEM(pk [1184]byte) [1185]byte {
	var out [1185]byte
	out[0] = 0x02
	copy(out[1:], pk[:])
	return out
}

// PQXDHKDF computes the PQXDH-shaped KDF of spec section 4.3:
//
//	pq_prk = Extract(Z(32), FF(32) || DH1 || DH2 || DH3 || SS)
//	SK     = HKDF-Expand-SHA256(pq_prk,
//	            "GRT11-PQXDH_CURVE25519_SHA-256_ML-KEM-768", 32)
func PQXDHKDF(dh1, dh2, dh3, ss [32]byte) [32]byte {
	// IKM = FF(32) || DH1 || DH2 || DH3 || SS, 160 bytes.
	var ikm [160]byte
	for i := 0; i < 32; i++ {
		ikm[i] = 0xff
	}
	copy(ikm[32:], dh1[:])
	copy(ikm[64:], dh2[:])
	copy(ikm[96:], dh3[:])
	copy(ikm[128:], ss[:])
	prk := Extract(make([]byte, 32), ikm[:])
	var out [32]byte
	copy(out[:], ExpandRaw(prk[:], "GRT11-PQXDH_CURVE25519_SHA-256_ML-KEM-768", 32))
	return out
}
