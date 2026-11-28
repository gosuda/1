package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"testing"
)

func TestNewEpochAEADRejectsBadParameters(t *testing.T) {
	key := make([]byte, 32)
	cases := []struct {
		name    string
		cs      uint16
		key, iv []byte
	}{
		{"unknown cipher", 0x0003, key, make([]byte, 24)},
		{"xchacha short key", CipherXChaCha20Poly1305, key[:31], make([]byte, 24)},
		{"xchacha aes-length iv", CipherXChaCha20Poly1305, key, make([]byte, 12)},
		{"aes short key", CipherAES256GCM, key[:16], make([]byte, 12)},
		{"aes xchacha-length iv", CipherAES256GCM, key, make([]byte, 24)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewEpochAEAD(tc.cs, tc.key, tc.iv); !errors.Is(err, ErrUnknownCipher) {
				t.Fatalf("err = %v, want ErrUnknownCipher", err)
			}
		})
	}
}

// TestAESGCMEpochMatchesStdlib checks the spec section 5.1 nonce
// iv XOR (Z(4) || u64(pn)) against a directly keyed stdlib AES-GCM.
func TestAESGCMEpochMatchesStdlib(t *testing.T) {
	key := bytes.Repeat([]byte{0x31}, 32)
	iv := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	e, err := NewEpochAEAD(CipherAES256GCM, key, iv)
	if err != nil {
		t.Fatal(err)
	}
	if e.CipherID() != CipherAES256GCM {
		t.Fatalf("CipherID = %#x, want %#x", e.CipherID(), CipherAES256GCM)
	}
	blk, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blk)
	const pn = 0x0102030405060708
	nonce := append([]byte(nil), iv...)
	for i := range 8 {
		nonce[4+i] ^= byte(uint64(pn) >> (56 - 8*i))
	}
	if n := e.Nonce(pn); !bytes.Equal(n[:], nonce) {
		t.Fatalf("Nonce(pn) = %x, want %x", n, nonce)
	}
	aad, pt := make([]byte, 32), []byte("packet")
	if got, want := e.Seal(pn, aad, pt, nil), gcm.Seal(nil, nonce, pt, aad); !bytes.Equal(got, want) {
		t.Fatalf("Seal = %x, want %x", got, want)
	}
}

func TestEpochAEADZeroize(t *testing.T) {
	e, err := NewEpochAEAD(CipherXChaCha20Poly1305, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 24))
	if err != nil {
		t.Fatal(err)
	}
	e.Zeroize()
	if e.keymat != ([32]byte{}) || e.subkey != ([32]byte{}) || e.nonceTl != ([8]byte{}) ||
		e.aead != nil || e.CipherID() != 0 {
		t.Fatal("Zeroize left key material or a live AEAD")
	}
}

func TestSealOnceRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, 32)
	pt, aad := []byte("flight body"), []byte("sp")
	for _, cs := range []uint16{CipherXChaCha20Poly1305, CipherAES256GCM} {
		nonce := make([]byte, NonceLen(cs))
		ct, err := SealOnce(cs, key, nonce, pt, aad, nil)
		if err != nil {
			t.Fatalf("cipher %#x SealOnce: %v", cs, err)
		}
		got, err := OpenOnce(cs, key, nonce, ct, aad, nil)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("cipher %#x OpenOnce = %q, %v", cs, got, err)
		}
		ct[0] ^= 1
		if _, err := OpenOnce(cs, key, nonce, ct, aad, nil); err == nil {
			t.Fatalf("cipher %#x opened tampered ciphertext", cs)
		}
	}
	if _, err := SealOnce(0x0009, key, nil, pt, aad, nil); !errors.Is(err, ErrUnknownCipher) {
		t.Fatalf("SealOnce unknown cipher err = %v", err)
	}
	if _, err := OpenOnce(0x0009, key, nil, pt, aad, nil); !errors.Is(err, ErrUnknownCipher) {
		t.Fatalf("OpenOnce unknown cipher err = %v", err)
	}
	if _, err := SealOnce(CipherAES256GCM, key[:7], make([]byte, 12), pt, aad, nil); err == nil {
		t.Fatal("SealOnce accepted a 7-byte AES key")
	}
	if _, err := SealXChaChaDirect(key[:7], make([]byte, 24), pt, aad, nil); err == nil {
		t.Fatal("SealXChaChaDirect accepted a 7-byte key")
	}
	if _, err := OpenXChaChaDirect(key[:7], make([]byte, 24), pt, aad, nil); err == nil {
		t.Fatal("OpenXChaChaDirect accepted a 7-byte key")
	}
}

func TestNonceLen(t *testing.T) {
	if NonceLen(CipherAES256GCM) != 12 || NonceLen(CipherXChaCha20Poly1305) != 24 {
		t.Fatal("NonceLen does not match spec section 4.1")
	}
}

func TestZeroizeClearsBuffer(t *testing.T) {
	b := []byte{1, 2, 3}
	Zeroize(b)
	if !bytes.Equal(b, []byte{0, 0, 0}) {
		t.Fatalf("Zeroize left %x", b)
	}
}
