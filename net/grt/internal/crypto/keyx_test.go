package crypto

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"testing"
)

func TestCanonicalX25519Public(t *testing.T) {
	pMinus1 := x25519FieldP
	pMinus1[0]--
	pPlus1 := x25519FieldP
	pPlus1[0]++
	topBit := [32]byte{31: 0x80}
	cases := []struct {
		name string
		pk   []byte
		want bool
	}{
		{"zero", make([]byte, 32), true},
		{"p-1", pMinus1[:], true},
		{"p", x25519FieldP[:], false},
		{"p+1", pPlus1[:], false},
		{"top bit set", topBit[:], false},
		{"31 bytes", make([]byte, 31), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalX25519Public(tc.pk); got != tc.want {
				t.Fatalf("CanonicalX25519Public(%x) = %v, want %v", tc.pk, got, tc.want)
			}
		})
	}
}

func TestX25519AgreesAndRejects(t *testing.T) {
	a, err := GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	b, err := X25519KeyPairFromPriv(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ab, err := X25519(a.Priv, b.Pub[:])
	if err != nil {
		t.Fatal(err)
	}
	ba, err := X25519(b.Priv, a.Pub[:])
	if err != nil || ab != ba {
		t.Fatalf("shared secrets differ (err %v)", err)
	}
	ref, _ := ecdh.X25519().NewPrivateKey(b.Priv[:])
	if !bytes.Equal(ref.PublicKey().Bytes(), b.Pub[:]) {
		t.Fatal("X25519KeyPairFromPriv public key differs from crypto/ecdh")
	}
	if _, err := X25519(a.Priv, x25519FieldP[:]); !errors.Is(err, ErrNonCanonicalKey) {
		t.Fatalf("non-canonical peer key err = %v, want ErrNonCanonicalKey", err)
	}
	// u = 0 is canonical but low order: the shared output is all zero.
	if _, err := X25519(a.Priv, make([]byte, 32)); err == nil {
		t.Fatal("low-order peer key produced a shared secret")
	}
	if _, err := X25519KeyPairFromPriv(make([]byte, 31)); err == nil {
		t.Fatal("31-byte private key accepted")
	}
	a.Zeroize()
	if a != (X25519KeyPair{}) {
		t.Fatal("X25519KeyPair.Zeroize left key material")
	}
}

func TestMLKEMKeyPairZeroize(t *testing.T) {
	kp, err := GenerateMLKEM768()
	if err != nil {
		t.Fatal(err)
	}
	kp.Zeroize()
	if kp != (MLKEM768KeyPair{}) {
		t.Fatal("MLKEM768KeyPair.Zeroize left key material")
	}
}

func TestTaggedKeyEncodings(t *testing.T) {
	ec := EncodeEC([32]byte{0xab})
	if ec[0] != 0x01 || ec[1] != 0xab {
		t.Fatalf("EncodeEC prefix = %x", ec[:2])
	}
	kem := EncodeKEM([1184]byte{0xcd})
	if kem[0] != 0x02 || kem[1] != 0xcd {
		t.Fatalf("EncodeKEM prefix = %x", kem[:2])
	}
}
