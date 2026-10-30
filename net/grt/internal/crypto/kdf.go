// Package crypto implements the GRT/1.1 cryptographic profile (spec section
// 4, 8.3-8.4, 9, 19): AEAD ciphers, the domain-separated HKDF profile,
// X25519/ML-KEM-768 primitives, XEdDSA signatures, and token/ticket envelopes.
package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// Extract is HKDF-Extract-SHA256: HMAC-SHA256(salt, ikm).
func Extract(salt, ikm []byte) [32]byte {
	m := hmac.New(sha256.New, salt)
	m.Write(ikm)
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// L is the domain-separated derivation of spec section 4.3:
//
//	L(secret, label, context, n) = HKDF-Expand-SHA256(secret,
//	    u16(n) || u8(len("GRT11 "+label)) || "GRT11 "+label ||
//	    u16(len(context)) || context, n)
//
// All callers use n <= 32, so expansion is exactly one HMAC block
// (HKDF-Expand T(1)). Labels are <=64 bytes, contexts <=65535 bytes. The
// concatenated "GRT11 "+label prefix is streamed into the HMAC without
// materializing it.
func L(secret []byte, label string, context []byte, n int) []byte {
	out := make([]byte, n)
	LInto(out, secret, label, context)
	return out
}

// LInto is L with a caller-provided output buffer of len <= 32.
func LInto(out []byte, secret []byte, label string, context []byte) {
	n := len(out)
	m := hmac.New(sha256.New, secret)
	var tmp [8]byte
	binary.BigEndian.PutUint16(tmp[0:], uint16(n))
	m.Write(tmp[0:2])
	tmp[0] = uint8(len("GRT11 ") + len(label))
	m.Write(tmp[0:1])
	m.Write([]byte("GRT11 "))
	m.Write([]byte(label))
	binary.BigEndian.PutUint16(tmp[0:], uint16(len(context)))
	m.Write(tmp[0:2])
	m.Write(context)
	tmp[0] = 0x01 // HKDF-Expand counter byte
	m.Write(tmp[0:1])
	copy(out, m.Sum(nil))
}

// L32 is L specialized to a 32-byte result.
func L32(secret []byte, label string, context []byte) [32]byte {
	var out [32]byte
	LInto(out[:], secret, label, context)
	return out
}

// ExpandRaw is HKDF-Expand-SHA256 with a caller-supplied exact info string
// (used for the fixed PQXDH KDF info of spec section 4.3).
func ExpandRaw(secret []byte, info string, n int) []byte {
	m := hmac.New(sha256.New, secret)
	out := make([]byte, 0, n)
	var prev []byte
	for i := 1; len(out) < n; i++ {
		m.Reset()
		m.Write(prev)
		m.Write([]byte(info))
		m.Write([]byte{byte(i)})
		prev = m.Sum(nil)
		out = append(out, prev...)
	}
	return out[:n]
}

// HMAC256 computes HMAC-SHA256(key, msg).
func HMAC256(key, msg []byte) [32]byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// HMAC256a computes HMAC-SHA256(key, a||b) without concatenating.
func HMAC256a(key, a, b []byte) [32]byte {
	m := hmac.New(sha256.New, key)
	m.Write(a)
	m.Write(b)
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// Hash256 computes SHA-256(a || b).
func Hash256(a, b []byte) [32]byte {
	h := sha256.New()
	h.Write(a)
	h.Write(b)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
