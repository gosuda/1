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
// (spec section 6.2). It holds a small key set indexed by key_id; retired
// keys remain verify-only for the admitted maximum lifetime.
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
	old   map[uint32][32]byte // verify-only
	oldAt map[uint32]uint64   // when each retired key stopped issuing
}

// NewTokenService creates a service with a fresh random key. now may be nil
// (uses system time).
func NewTokenService(now func() uint64, domain []byte) *TokenService {
	s := &TokenService{now: now, domain: append([]byte(nil), domain...),
		old: make(map[uint32][32]byte), oldAt: make(map[uint32]uint64)}
	if s.now == nil {
		s.now = func() uint64 { return uint64(time.Now().Unix()) }
	}
	if _, err := rand.Read(s.cur[:]); err != nil {
		s.dead = true
	}
	return s
}

// Zeroize erases the issuing and retired verification keys. Issuing and
// verification both refuse afterwards.
func (s *TokenService) Zeroize() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cur {
		s.cur[i] = 0
	}
	for id := range s.old {
		s.old[id] = [32]byte{}
	}
	s.dead = true
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
	var key [32]byte
	switch {
	case t.KeyID == s.curID:
		if s.dead {
			return false // current key was never seeded: it would verify forgeries
		}
		key = s.cur
	default:
		k, ok := s.old[t.KeyID]
		if !ok {
			return false
		}
		key = k
	}
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

// Rotate installs a fresh issuing key; the old key stays verify-only
// until every token it could have issued has passed its admitted maximum
// lifetime, after which it is pruned.
func (s *TokenService) Rotate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A dead key was never seeded: it must not enter the verify-only
	// set, where it would keep validating forgeries under its key_id.
	if !s.dead {
		s.old[s.curID] = s.cur
		s.oldAt[s.curID] = s.now()
	}
	s.curID++
	if _, err := rand.Read(s.cur[:]); err != nil {
		s.dead = true
		return
	}
	s.dead = false
	// A token issued under a retired key lives at most
	// ReuseTokenLifetimeS past retirement; keys beyond that horizon can
	// no longer validate anything.
	maxLife := uint64(wire.ReuseTokenLifetimeS)
	for id, at := range s.oldAt {
		if s.now() > at+maxLife {
			delete(s.old, id)
			delete(s.oldAt, id)
		}
	}
}
