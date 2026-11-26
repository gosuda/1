package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

// Cipher identifiers (spec section 4.1).
const (
	CipherXChaCha20Poly1305 uint16 = 0x0001
	CipherAES256GCM         uint16 = 0x0002
)

var (
	ErrUnknownCipher = errors.New("grt: unknown cipher suite")
	ErrBadTag        = errors.New("grt: AEAD authentication failed")
)

// NonceLen returns the AEAD nonce length for a suite.
func NonceLen(cs uint16) int {
	if cs == CipherAES256GCM {
		return 12
	}
	return 24
}

// EpochAEAD is an installed directional packet-protection context for one
// key epoch. It implements the exact wire nonce derivation of spec section
// 5.1:
//
//	nonce = iv[d,e] XOR (Z(NLen-8) || u64(pn))      (u64 big-endian)
//
// For XChaCha the HChaCha subkey is cached at install time (spec 4.4): the
// first 16 nonce bytes are the constant iv[0:16], so the inner IETF
// ChaCha20-Poly1305 nonce is Z(4) || (iv[16:24] XOR u64(pn)). This is a
// byte-identical implementation of the negotiated XChaCha AEAD.
type EpochAEAD struct {
	cipher   uint16
	aead     cipher.AEAD // inner IETF ChachaPoly or AES-GCM, keyed with (sub)key
	noncePre [4]byte     // nonce[0:4]
	nonceTl  [8]byte     // nonce[NLen-8:NLen]; XORed with big-endian pn
	keymat   [32]byte    // raw epoch key, retained for explicit zeroization
	subkey   [32]byte    // XChaCha: cached HChaCha subkey keyed into aead
	nonceBuf [12]byte
}

// NewEpochAEAD installs key/iv for a cipher into the cached form.
// iv must be NonceLen(cs) bytes.
func NewEpochAEAD(cs uint16, key, iv []byte) (EpochAEAD, error) {
	var e EpochAEAD
	e.cipher = cs
	switch cs {
	case CipherXChaCha20Poly1305:
		if len(key) != 32 || len(iv) != 24 {
			return e, ErrUnknownCipher
		}
		copy(e.keymat[:], key)
		e.subkey = hChaCha20(key, iv[0:16])
		a, err := chacha20poly1305.New(e.subkey[:])
		if err != nil {
			return e, err
		}
		e.aead = a
		copy(e.nonceTl[:], iv[16:24])
		// noncePre stays zero: inner nonce = Z(4) || derived 8 bytes.
	case CipherAES256GCM:
		if len(key) != 32 || len(iv) != 12 {
			return e, ErrUnknownCipher
		}
		copy(e.keymat[:], key)
		blk, err := aes.NewCipher(key)
		if err != nil {
			return e, err
		}
		a, err := cipher.NewGCMWithTagSize(blk, 16)
		if err != nil {
			return e, err
		}
		e.aead = a
		copy(e.noncePre[:], iv[0:4])
		copy(e.nonceTl[:], iv[4:12])
	default:
		return e, ErrUnknownCipher
	}
	return e, nil
}

// Zeroize scrubs every secret byte the package owns: the raw epoch key,
// the cached HChaCha subkey, and the nonce masks. The wrapped library
// AEAD holds its own expanded copy (inner ChaCha key, or AES round keys
// plus the GHASH product table) that portable Go cannot reach into;
// detaching the interface leaves it unreachable, bounding that residual
// to collector reclamation rather than connection lifetime.
func (e *EpochAEAD) Zeroize() {
	Zeroize(e.keymat[:])
	Zeroize(e.subkey[:])
	Zeroize(e.noncePre[:])
	Zeroize(e.nonceTl[:])
	e.aead = nil
	e.cipher = 0
}

// Nonce builds the per-packet nonce: prefix || (tail XOR be64(pn)).
func (e *EpochAEAD) Nonce(pn uint64) [12]byte {
	var n [12]byte
	n[0] = e.noncePre[0]
	n[1] = e.noncePre[1]
	n[2] = e.noncePre[2]
	n[3] = e.noncePre[3]
	v := binary.BigEndian.Uint64(e.nonceTl[:]) ^ pn
	binary.BigEndian.PutUint64(n[4:], v)
	return n
}

func (e *EpochAEAD) fillNonce(pn uint64) []byte {
	e.nonceBuf[0] = e.noncePre[0]
	e.nonceBuf[1] = e.noncePre[1]
	e.nonceBuf[2] = e.noncePre[2]
	e.nonceBuf[3] = e.noncePre[3]
	v := binary.BigEndian.Uint64(e.nonceTl[:]) ^ pn
	binary.BigEndian.PutUint64(e.nonceBuf[4:], v)
	return e.nonceBuf[:]
}

// Seal encrypts one DATA packet payload: aad is the 32-byte header.
// out must have room for len(pt)+16; returns out[:len(pt)+16].
func (e *EpochAEAD) Seal(pn uint64, aad, pt, out []byte) []byte {
	n := e.fillNonce(pn)
	return e.aead.Seal(out, n, pt, aad)
}

// Open authenticates+decrypts one packet's ciphertext (including tag) into
// out. Returns nil slice on failure; callers MUST NOT expose speculative
// plaintext on failure.
func (e *EpochAEAD) Open(pn uint64, aad, ct, out []byte) ([]byte, error) {
	n := e.fillNonce(pn)
	return e.aead.Open(out[:0], n, ct, aad)
}

// CipherID returns the negotiated cipher.
func (e *EpochAEAD) CipherID() uint16 { return e.cipher }

// SealXChaChaDirect is the reference one-shot XChaCha20-Poly1305 seal with
// the full 24-byte nonce (no caching); used for ticket/early domains and
// cross-checking the cached form.
func SealXChaChaDirect(key, nonce24, pt, aad, out []byte) ([]byte, error) {
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	return a.Seal(out[:0], nonce24, pt, aad), nil
}

// OpenXChaChaDirect is the direct-form open.
func OpenXChaChaDirect(key, nonce24, ct, aad, out []byte) ([]byte, error) {
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	return a.Open(out[:0], nonce24, ct, aad)
}

// SealOnce seals with an arbitrary cipher and nonce (used for handshake
// response bodies and tickets where nonces are derived, not per-packet).
func SealOnce(cs uint16, key, nonce, pt, aad, out []byte) ([]byte, error) {
	var a cipher.AEAD
	var err error
	switch cs {
	case CipherXChaCha20Poly1305:
		a, err = chacha20poly1305.NewX(key)
	case CipherAES256GCM:
		var blk cipher.Block
		blk, err = aes.NewCipher(key)
		if err == nil {
			a, err = cipher.NewGCMWithTagSize(blk, 16)
		}
	default:
		return nil, ErrUnknownCipher
	}
	if err != nil {
		return nil, err
	}
	return a.Seal(out[:0], nonce, pt, aad), nil
}

// OpenOnce is the open counterpart of SealOnce.
func OpenOnce(cs uint16, key, nonce, ct, aad, out []byte) ([]byte, error) {
	var a cipher.AEAD
	var err error
	switch cs {
	case CipherXChaCha20Poly1305:
		a, err = chacha20poly1305.NewX(key)
	case CipherAES256GCM:
		var blk cipher.Block
		blk, err = aes.NewCipher(key)
		if err == nil {
			a, err = cipher.NewGCMWithTagSize(blk, 16)
		}
	default:
		return nil, ErrUnknownCipher
	}
	if err != nil {
		return nil, err
	}
	return a.Open(out[:0], nonce, ct, aad)
}
