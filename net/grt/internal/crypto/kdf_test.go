package crypto

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"strings"
	"testing"
)

// TestLMatchesStdlibHKDF cross-checks L against crypto/hkdf with the spec
// section 4.3 info string assembled independently.
func TestLMatchesStdlibHKDF(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, 32)
	cases := []struct {
		name    string
		label   string
		context []byte
		n       int
	}{
		{"one byte output", "root", []byte{9}, 1},
		{"aes iv length", lblC2SIV, make([]byte, 38), 12},
		{"xchacha iv length", lblEarlyIV, make([]byte, 32), 24},
		{"full block", lblRefreshedRoot, nil, 32},
		{"64-byte label", strings.Repeat("x", 64), []byte("ctx"), 32},
		{"max context", "root", make([]byte, 0xffff), 32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := "GRT11 " + tc.label
			info := []byte{byte(tc.n >> 8), byte(tc.n), byte(len(full))}
			info = append(info, full...)
			info = append(info, byte(len(tc.context)>>8), byte(len(tc.context)))
			info = append(info, tc.context...)
			want, err := hkdf.Expand(sha256.New, secret, string(info), tc.n)
			if err != nil {
				t.Fatal(err)
			}
			if got := L(secret, tc.label, tc.context, tc.n); !bytes.Equal(got, want) {
				t.Fatalf("L(%q, n=%d) = %x, want %x", tc.label, tc.n, got, want)
			}
		})
	}
}

func TestLPanicsOnOutOfRangeArguments(t *testing.T) {
	secret := make([]byte, 32)
	cases := []struct {
		name string
		call func()
	}{
		{"output longer than one block", func() { L(secret, "root", nil, 33) }},
		{"empty output", func() { L(secret, "root", nil, 0) }},
		{"label longer than 64 bytes", func() { L(secret, strings.Repeat("x", 65), nil, 32) }},
		{"label that would wrap the length byte", func() { L(secret, strings.Repeat("x", 250), nil, 32) }},
		{"context longer than 65535 bytes", func() { L(secret, "root", make([]byte, 0x10000), 32) }},
		{"LInto with 64-byte buffer", func() { LInto(make([]byte, 64), secret, "root", nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic, got none")
				}
			}()
			tc.call()
		})
	}
}

func TestExpandRawMatchesStdlibHKDF(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	const info = "GRT11-PQXDH_CURVE25519_SHA-256_ML-KEM-768"
	for _, n := range []int{0, 1, 32, 33, 100, 255 * 32} {
		want, err := hkdf.Expand(sha256.New, secret, info, n)
		if err != nil {
			t.Fatal(err)
		}
		if got := ExpandRaw(secret, info, n); !bytes.Equal(got, want) {
			t.Fatalf("ExpandRaw(n=%d) differs from crypto/hkdf", n)
		}
	}
}

func TestExpandRawPanicsPastCounterLimit(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("ExpandRaw(n=255*32+1) did not panic")
		}
	}()
	ExpandRaw(make([]byte, 32), "info", 255*32+1)
}

func TestHashHelpersMatchStdlib(t *testing.T) {
	a, b := []byte("left"), []byte("right")
	if got, want := Hash256(a, b), sha256.Sum256([]byte("leftright")); got != want {
		t.Fatalf("Hash256 = %x, want %x", got, want)
	}
	key := []byte("k")
	if HMAC256a(key, a, b) != HMAC256(key, []byte("leftright")) {
		t.Fatal("HMAC256a(a, b) differs from HMAC256(a||b)")
	}
	prk, err := hkdf.Extract(sha256.New, []byte("ikm"), []byte("salt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Extract([]byte("salt"), []byte("ikm")); !bytes.Equal(got[:], prk) {
		t.Fatalf("Extract = %x, want %x", got, prk)
	}
}
