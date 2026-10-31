package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"net/netip"
	"sync"
	"time"

	"gosuda.org/grt/internal/wire"
)

// TokenService issues and validates the 64-byte address tokens
// (spec section 6.2). It holds the issuing key and at most one retired
// verify-only key: a token validates only while its key_id is current or
// exactly one rotation old.
type TokenService struct {
	mu     sync.RWMutex
	now    func() uint64 // unix seconds
	domain []byte        // listener_domain
	// dead marks the current issuing key as unseeded after an entropy
	// failure: issuing refuses and tokens claiming that key never verify,
	// since an all-zero key would accept a forged HMAC.
	dead  bool
	curID uint32
	cur   [32]byte
	// hasPrev guards prev: an absent slot must never verify under its
	// zero bytes.
	hasPrev bool
	prevID  uint32
	prev    [32]byte
	// zeroized is terminal: Rotate cannot reseed after Zeroize.
	zeroized bool
}

// NewTokenService creates a service with a fresh random key. now may be nil
// (uses system time).
func NewTokenService(now func() uint64, domain []byte) *TokenService {
	s := &TokenService{now: now, domain: append([]byte(nil), domain...)}
	if s.now == nil {
		s.now = func() uint64 { return uint64(time.Now().Unix()) }
	}
	if _, err := rand.Read(s.cur[:]); err != nil {
		s.dead = true
	}
	return s
}

// Zeroize erases the issuing and retired verification keys. Issuing and
// verification refuse permanently afterwards; later Rotate calls are
// no-ops.
func (s *TokenService) Zeroize() {
	s.mu.Lock()
	defer s.mu.Unlock()
	Zeroize(s.cur[:])
	s.dropPrev()
	s.dead = true
	s.zeroized = true
}

func (s *TokenService) dropPrev() {
	Zeroize(s.prev[:])
	s.hasPrev = false
}

// verifyKey returns the HMAC key for keyID, or false when the id is
// unknown, retired more than one rotation ago, or unseeded.
func (s *TokenService) verifyKey(keyID uint32) ([32]byte, bool) {
	switch {
	case keyID == s.curID && !s.dead:
		return s.cur, true
	case s.hasPrev && keyID == s.prevID:
		return s.prev, true
	}
	return [32]byte{}, false
}

func (s *TokenService) common(t *wire.Token, source, localDest netip.AddrPort) []byte {
	var head [32]byte
	wire.EncodeToken(head[:], t)
	return wire.TokenCommonInput(nil, head[:], source, localDest, s.domain)
}

// IssueRetry creates a kind-0 token binding origDCID/flightHash/retryDCID.
func (s *TokenService) IssueRetry(origDCID, retryDCID uint64, flightHash [32]byte, source, localDest netip.AddrPort) ([wire.TokenBytes]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out [wire.TokenBytes]byte
	if s.dead {
		return out, false
	}
	var t wire.Token
	t.KeyID = s.curID
	t.IssuedUnixS = s.now()
	t.ExpiresUnixS = t.IssuedUnixS + wire.RetryTokenLifetimeS
	t.Binding = origDCID
	t.Kind = wire.TokenKindRetry
	in := s.common(&t, source, localDest)
	if in == nil {
		return out, false
	}
	in = append(in, flightHash[:]...)
	var rc [8]byte
	binary.BigEndian.PutUint64(rc[:], retryDCID)
	in = append(in, rc[:]...)
	m := hmac.New(sha256.New, s.cur[:])
	m.Write(in)
	copy(t.Tag[:], m.Sum(nil))
	wire.EncodeToken(out[:], &t)
	return out, true
}

// IssueReuse creates a kind-1 token proving return routability of source.
func (s *TokenService) IssueReuse(source, localDest netip.AddrPort) ([wire.TokenBytes]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out [wire.TokenBytes]byte
	if s.dead {
		return out, false
	}
	var t wire.Token
	t.KeyID = s.curID
	t.IssuedUnixS = s.now()
	t.ExpiresUnixS = t.IssuedUnixS + wire.ReuseTokenLifetimeS
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return out, false
	}
	t.Binding = binary.BigEndian.Uint64(b[:])
	t.Kind = wire.TokenKindReuse
	in := s.common(&t, source, localDest)
	if in == nil {
		return out, false
	}
	in = append(in, make([]byte, 40)...)
	m := hmac.New(sha256.New, s.cur[:])
	m.Write(in)
	copy(t.Tag[:], m.Sum(nil))
	wire.EncodeToken(out[:], &t)
	return out, true
}

// ValidateToken checks a presented token end to end. For kind-0 tokens,
// flightHash/retryDCID/origDCID bind the attempt: retryDCID is the outer
// datagram DCID and origDCID must equal the fragment-zero original DCID.
// For kind-1 tokens the hash/DCID arguments are ignored.
func (s *TokenService) ValidateToken(tb [wire.TokenBytes]byte, source, localDest netip.AddrPort, flightHash [32]byte, retryDCID, origDCID uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := wire.DecodeToken(tb[:])
	if !ok {
		return false
	}
	now := s.now()
	maxLife := uint64(wire.RetryTokenLifetimeS)
	if t.Kind == wire.TokenKindReuse {
		maxLife = wire.ReuseTokenLifetimeS
	}
	if t.ExpiresUnixS-t.IssuedUnixS > maxLife || now < t.IssuedUnixS || now > t.ExpiresUnixS {
		return false
	}
	key, ok := s.verifyKey(t.KeyID)
	if !ok {
		return false
	}
	defer Zeroize(key[:])
	in := s.common(&t, source, localDest)
	if in == nil {
		return false
	}
	var tail [40]byte
	if t.Kind == wire.TokenKindRetry {
		if t.Binding != origDCID {
			return false
		}
		copy(tail[:32], flightHash[:])
		binary.BigEndian.PutUint64(tail[32:], retryDCID)
	}
	in = append(in, tail[:]...)
	m := hmac.New(sha256.New, key[:])
	m.Write(in)
	want := m.Sum(nil)
	return subtle.ConstantTimeCompare(want, t.Tag[:]) == 1
}

// Rotate installs a fresh issuing key under key_id+1 and demotes the
// current key to the single verify-only slot, erasing the key it
// replaces. A token stays valid until the second Rotate after its
// issuance, so a periodic caller needs an interval of at least
// wire.ReuseTokenLifetimeS for REUSE tokens to live out their lifetime.
// Safe for concurrent use with issuing and validation; a no-op after
// Zeroize.
func (s *TokenService) Rotate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.zeroized {
		return
	}
	s.dropPrev()
	// A dead key was never seeded: it must not enter the verify-only
	// slot, where it would keep validating forgeries under its key_id.
	if !s.dead {
		s.prev, s.prevID, s.hasPrev = s.cur, s.curID, true
	}
	s.curID++
	if _, err := rand.Read(s.cur[:]); err != nil {
		Zeroize(s.cur[:])
		s.dead = true
		return
	}
	s.dead = false
}
