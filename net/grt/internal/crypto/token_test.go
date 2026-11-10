package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/netip"
	"sync"
	"testing"

	"gosuda.org/grt/internal/wire"
)

var (
	tokenTestSrc = netip.MustParseAddrPort("192.0.2.1:1000")
	tokenTestDst = netip.MustParseAddrPort("192.0.2.2:4433")
)

const tokenTestNow = 1_800_000_000

func newTestTokenService(t *testing.T) *TokenService {
	t.Helper()
	return NewTokenService(func() uint64 { return tokenTestNow }, []byte("domain"))
}

func issueTestReuse(t *testing.T, s *TokenService) [wire.TokenBytes]byte {
	t.Helper()
	tok, ok := s.IssueReuse(tokenTestSrc, tokenTestDst)
	if !ok {
		t.Fatal("IssueReuse refused on a seeded service")
	}
	return tok
}

func validReuse(s *TokenService, tok [wire.TokenBytes]byte) bool {
	return s.ValidateToken(tok, tokenTestSrc, tokenTestDst, [32]byte{}, 0, 0)
}

// forgeReuse builds a REUSE token under keyID tagged with key, bypassing
// the service.
func forgeReuse(s *TokenService, keyID uint32, key [32]byte) [wire.TokenBytes]byte {
	tok := wire.Token{KeyID: keyID, Kind: wire.TokenKindReuse,
		IssuedUnixS: tokenTestNow, ExpiresUnixS: tokenTestNow + 60}
	var head [32]byte
	wire.EncodeToken(head[:], &tok)
	in := wire.TokenCommonInput(nil, head[:], tokenTestSrc, tokenTestDst, s.domain)
	in = append(in, make([]byte, 40)...)
	m := hmac.New(sha256.New, key[:])
	m.Write(in)
	copy(tok.Tag[:], m.Sum(nil))
	var tb [wire.TokenBytes]byte
	wire.EncodeToken(tb[:], &tok)
	return tb
}

func TestTokenValidAcrossOneRotation(t *testing.T) {
	s := newTestTokenService(t)
	tok := issueTestReuse(t, s)
	if !validReuse(s, tok) {
		t.Fatal("fresh token rejected")
	}
	s.Rotate()
	if !validReuse(s, tok) {
		t.Fatal("token rejected one rotation after issue")
	}
	if !validReuse(s, issueTestReuse(t, s)) {
		t.Fatal("token under the rotated-in key rejected")
	}
}

func TestTokenInvalidAfterTwoRotations(t *testing.T) {
	s := newTestTokenService(t)
	tok := issueTestReuse(t, s)
	s.Rotate()
	s.Rotate()
	if validReuse(s, tok) {
		t.Fatal("token validated two rotations after issue")
	}
}

func TestTokenZeroizedKeysNeverValidate(t *testing.T) {
	s := newTestTokenService(t)
	old := issueTestReuse(t, s)
	s.Rotate()
	cur := issueTestReuse(t, s)
	s.Zeroize()
	if validReuse(s, old) || validReuse(s, cur) {
		t.Fatal("token validated after Zeroize")
	}
	// Zeroized slots hold zero bytes; a zero-key forgery must fail for
	// both the retired and the current key_id.
	for _, id := range []uint32{0, 1} {
		if validReuse(s, forgeReuse(s, id, [32]byte{})) {
			t.Fatalf("zero-key forgery under key_id %d validated after Zeroize", id)
		}
	}
	s.Rotate()
	if _, ok := s.IssueReuse(tokenTestSrc, tokenTestDst); ok {
		t.Fatal("Rotate revived a zeroized service")
	}
}

func TestTokenUnknownKeyIDRejected(t *testing.T) {
	s := newTestTokenService(t)
	if validReuse(s, forgeReuse(s, 99, s.cur)) {
		t.Fatal("token naming an unknown key_id validated under the current key")
	}
}

func TestTokenRotateFromDeadKeyDoesNotRetainIt(t *testing.T) {
	s := newTestTokenService(t)
	s.dead = true
	s.Rotate()
	if validReuse(s, forgeReuse(s, 0, s.prev)) {
		t.Fatal("unseeded key entered the verify-only slot")
	}
	if !validReuse(s, issueTestReuse(t, s)) {
		t.Fatal("rotation after an unseeded key did not recover issuing")
	}
}

func TestRetryTokenBindsAttempt(t *testing.T) {
	s := newTestTokenService(t)
	flight := [32]byte{1}
	tok, ok := s.IssueRetry(10, 20, flight, tokenTestSrc, tokenTestDst)
	if !ok {
		t.Fatal("IssueRetry refused on a seeded service")
	}
	cases := []struct {
		name              string
		flight            [32]byte
		retryDCID, origID uint64
		src               netip.AddrPort
		want              bool
	}{
		{"matching attempt", flight, 20, 10, tokenTestSrc, true},
		{"different flight hash", [32]byte{2}, 20, 10, tokenTestSrc, false},
		{"different retry DCID", flight, 21, 10, tokenTestSrc, false},
		{"different original DCID", flight, 20, 11, tokenTestSrc, false},
		{"different source", flight, 20, 10, netip.MustParseAddrPort("192.0.2.9:1000"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.ValidateToken(tok, tc.src, tokenTestDst, tc.flight, tc.retryDCID, tc.origID)
			if got != tc.want {
				t.Fatalf("ValidateToken = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTokenExpiry(t *testing.T) {
	now := uint64(tokenTestNow)
	s := NewTokenService(func() uint64 { return now }, []byte("domain"))
	tok := issueTestReuse(t, s)
	now += wire.ReuseTokenLifetimeS
	if !validReuse(s, tok) {
		t.Fatal("token rejected at its expiry instant")
	}
	now++
	if validReuse(s, tok) {
		t.Fatal("token accepted one second after expiry")
	}
}

// TestTokenRotateConcurrentWithIssueAndValidate is a race-detector check
// for a timer-driven Rotate running beside the packet path.
func TestTokenRotateConcurrentWithIssueAndValidate(t *testing.T) {
	s := newTestTokenService(t)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			s.Rotate()
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 200 {
				if tok, ok := s.IssueReuse(tokenTestSrc, tokenTestDst); ok {
					validReuse(s, tok)
				}
			}
		})
	}
	wg.Wait()
	if !validReuse(s, issueTestReuse(t, s)) {
		t.Fatal("token issued after concurrent rotations rejected")
	}
}
