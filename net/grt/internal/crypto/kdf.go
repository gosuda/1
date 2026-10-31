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
// Expansion is exactly one HMAC block (HKDF-Expand T(1)), so n is limited
// to 1..32. The "GRT11 "+label prefix is streamed into the HMAC without
// materializing it.
//
// Labels and lengths are package constants, never peer input, so an
// out-of-range argument is a programmer error and panics rather than
// silently truncating the output or wrapping a length field.
func L(secret []byte, label string, context []byte, n int) []byte {
	out := make([]byte, n)
	LInto(out, secret, label, context)
	return out
}

const (
	maxLOutput  = 32 // one SHA-256 block
	maxLLabel   = 64 // spec section 4.3
	maxLContext = 0xffff
)

// LInto is L with a caller-provided output buffer of 1..32 bytes. It panics
// on an out-of-range output, label, or context length (see L).
func LInto(out []byte, secret []byte, label string, context []byte) {
	n := len(out)
	if n < 1 || n > maxLOutput {
		panic("grt/crypto: L output length out of range 1..32")
	}
	if len(label) > maxLLabel {
		panic("grt/crypto: L label longer than 64 bytes")
	}
	if len(context) > maxLContext {
		panic("grt/crypto: L context longer than 65535 bytes")
	}
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
// (used for the fixed PQXDH KDF info of spec section 4.3). It panics when n
// exceeds the RFC 5869 limit of 255 blocks, where the counter byte would
// wrap.
func ExpandRaw(secret []byte, info string, n int) []byte {
	if n < 0 || n > 255*sha256.Size {
		panic("grt/crypto: HKDF-Expand length out of range")
	}
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
