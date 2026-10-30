package crypto

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

// Ticket and early-data derivations (spec section 9). The ticket envelope
// and early keys always use XChaCha20-Poly1305 regardless of the negotiated
// connection cipher.

const (
	// TicketPlainBytes is the 180-byte ticket plaintext.
	TicketPlainBytes = 180
	// TicketWireBytes is the 224-byte opaque ticket.
	TicketWireBytes = 224
	// TicketMetaBytes is the public ticket metadata.
	TicketMetaBytes = 204
	// TicketKeyBytes is the ticket-key size.
	TicketKeyBytes = 32
	// TicketMaxLifetimeS is the 24-hour maximum ticket lifetime.
	TicketMaxLifetimeS = 86400
)

var ErrTicketInvalid = errors.New("grt: ticket validation failed")

// TicketPlain is the decrypted ticket content (spec section 9.1).
type TicketPlain struct {
	PSK           [32]byte
	TicketID      [16]byte
	IssuedUnixS   uint64
	ExpiresUnixS  uint64
	ReplayDomain  [16]byte
	ServiceHash   [32]byte
	MetadataHash  [32]byte
	PolicyHash    [32]byte
	MaxEarlyPlain uint32
}

// TicketMetadata is the 204-byte public NEW_TICKET metadata:
//
//	ticket_id:16 || expires:u64 || replay_domain:16 ||
//	service_hash:32 || max_early:u32 || cached_server_params:128
type TicketMetadata struct {
	TicketID      [16]byte
	ExpiresUnixS  uint64
	ReplayDomain  [16]byte
	ServiceHash   [32]byte
	MaxEarlyPlain uint32
	CachedParams  [128]byte
}

// EncodeTicketMeta serializes the 204-byte metadata.
func EncodeTicketMeta(m *TicketMetadata) [TicketMetaBytes]byte {
	var out [TicketMetaBytes]byte
	copy(out[0:], m.TicketID[:])
	binary.BigEndian.PutUint64(out[16:], m.ExpiresUnixS)
	copy(out[24:], m.ReplayDomain[:])
	copy(out[40:], m.ServiceHash[:])
	binary.BigEndian.PutUint32(out[72:], m.MaxEarlyPlain)
	copy(out[76:], m.CachedParams[:])
	return out
}

// DecodeTicketMeta parses the 204-byte metadata.
func DecodeTicketMeta(b []byte) TicketMetadata {
	var m TicketMetadata
	copy(m.TicketID[:], b[0:16])
	m.ExpiresUnixS = binary.BigEndian.Uint64(b[16:])
	copy(m.ReplayDomain[:], b[24:40])
	copy(m.ServiceHash[:], b[40:72])
	m.MaxEarlyPlain = binary.BigEndian.Uint32(b[72:])
	copy(m.CachedParams[:], b[76:204])
	return m
}

// TicketPSK derives the ticket PSK: L(resumption_master, "ticket psk",
// ticket_id, 32).
func TicketPSK(resumptionMaster [32]byte, ticketID [16]byte) [32]byte {
	return L32(resumptionMaster[:], lblTicketPSK, ticketID[:])
}

// EncodeTicketPlain serializes the 180-byte ticket plaintext.
func EncodeTicketPlain(t *TicketPlain) [TicketPlainBytes]byte {
	var out [TicketPlainBytes]byte
	copy(out[0:], t.PSK[:])
	copy(out[32:], t.TicketID[:])
	binary.BigEndian.PutUint64(out[48:], t.IssuedUnixS)
	binary.BigEndian.PutUint64(out[56:], t.ExpiresUnixS)
	copy(out[64:], t.ReplayDomain[:])
	copy(out[80:], t.ServiceHash[:])
	copy(out[112:], t.MetadataHash[:])
	copy(out[144:], t.PolicyHash[:])
	binary.BigEndian.PutUint32(out[176:], t.MaxEarlyPlain)
	return out
}

// DecodeTicketPlain parses the 180-byte ticket plaintext.
func DecodeTicketPlain(b []byte) TicketPlain {
	var t TicketPlain
	copy(t.PSK[:], b[0:32])
	copy(t.TicketID[:], b[32:48])
	t.IssuedUnixS = binary.BigEndian.Uint64(b[48:])
	t.ExpiresUnixS = binary.BigEndian.Uint64(b[56:])
	copy(t.ReplayDomain[:], b[64:80])
	copy(t.ServiceHash[:], b[80:112])
	copy(t.MetadataHash[:], b[112:144])
	copy(t.PolicyHash[:], b[144:176])
	t.MaxEarlyPlain = binary.BigEndian.Uint32(b[176:])
	return t
}

// SealTicket builds the opaque 224-byte ticket:
//
//	key_id:u32 || nonce:24 ||
//	Seal(XChaCha, ticket_key, nonce, plain180, AAD="GRT11 ticket"||u32(key_id))
//
// nonce must be a never-repeating per-key value: a 16-byte issuer prefix
// plus an 8-byte monotonic issuance counter (spec section 9.1).
func SealTicket(ticketKey [32]byte, keyID uint32, nonce [24]byte, plain [TicketPlainBytes]byte) [TicketWireBytes]byte {
	var out [TicketWireBytes]byte
	binary.BigEndian.PutUint32(out[0:], keyID)
	copy(out[4:], nonce[:])
	var aad [16]byte
	copy(aad[:], "GRT11 ticket")
	binary.BigEndian.PutUint32(aad[12:], keyID)
	// Seal appends into out[28:] (cap 196); the return aliases that region.
	if _, err := SealXChaChaDirect(ticketKey[:], nonce[:], plain[:], aad[:], out[28:28]); err != nil {
		panic(err)
	}
	return out
}

// OpenTicket decrypts a 224-byte ticket under keyID's key.
func OpenTicket(ticketKey [32]byte, ticket []byte) (TicketPlain, error) {
	var t TicketPlain
	if len(ticket) != TicketWireBytes {
		return t, ErrTicketInvalid
	}
	keyID := binary.BigEndian.Uint32(ticket[0:])
	var nonce [24]byte
	copy(nonce[:], ticket[4:28])
	var aad [16]byte
	copy(aad[:], "GRT11 ticket")
	binary.BigEndian.PutUint32(aad[12:], keyID)
	var buf [TicketPlainBytes]byte
	pt, err := OpenXChaChaDirect(ticketKey[:], nonce[:], ticket[28:], aad[:], buf[:])
	if err != nil {
		return t, ErrTicketInvalid
	}
	if len(pt) != TicketPlainBytes {
		return t, ErrTicketInvalid
	}
	return DecodeTicketPlain(buf[:]), nil
}

// TicketKeyID extracts the envelope key ID without decrypting.
func TicketKeyID(ticket []byte) (uint32, bool) {
	if len(ticket) != TicketWireBytes {
		return 0, false
	}
	return binary.BigEndian.Uint32(ticket[0:]), true
}

// TicketNonceIssue builds an issuance nonce: issuerPrefix16 || counter8.
// The issuer prefix must be a fresh per-key random value; the counter is a
// persistent monotonic issuance counter.
func TicketNonceIssue(issuerPrefix [16]byte, counter uint64) [24]byte {
	var n [24]byte
	copy(n[0:], issuerPrefix[:])
	binary.BigEndian.PutUint64(n[16:], counter)
	return n
}

// EarlyKeys holds the §9.2 early-data secrets derived from a ticket PSK.
type EarlyKeys struct {
	Key    [32]byte // XChaCha key
	IV     [24]byte
	Binder [32]byte // binder HMAC key
	ES     [32]byte
}

// Zeroize erases the derived early secrets in place.
func (e *EarlyKeys) Zeroize() { *e = EarlyKeys{} }

// DeriveEarly computes ES, early_key, early_iv, binder_key from the ticket
// PSK and CH0 (client message through ticket metadata).
func DeriveEarly(ticketPSK [32]byte, ch0 []byte) EarlyKeys {
	var e EarlyKeys
	e.ES = Extract(make([]byte, 32), ticketPSK[:])
	hch0 := sha256.Sum256(ch0)
	e.Key = L32(e.ES[:], lblEarlyKey, hch0[:])
	LInto(e.IV[:], e.ES[:], lblEarlyIV, hch0[:])
	e.Binder = L32(e.ES[:], lblBinder, hch0[:])
	return e
}

// EarlySeal encrypts the early plaintext batch (AAD = CH0).
func EarlySeal(e *EarlyKeys, plaintext, ch0 []byte) ([]byte, error) {
	return SealXChaChaDirect(e.Key[:], e.IV[:], plaintext, ch0, make([]byte, 0, len(plaintext)+16))
}

// EarlyOpen decrypts the early batch.
func EarlyOpen(e *EarlyKeys, ciphertext, ch0 []byte) ([]byte, error) {
	out := make([]byte, 0, len(ciphertext))
	pt, err := OpenXChaChaDirect(e.Key[:], e.IV[:], ciphertext, ch0, out[:0])
	if err != nil {
		return nil, err
	}
	return pt, nil
}

// EarlyBinder computes HMAC(binder_key, H(CH_no_binder)).
func EarlyBinder(e *EarlyKeys, chNoBinder []byte) [32]byte {
	h := sha256.Sum256(chNoBinder)
	return HMAC256(e.Binder[:], h[:])
}

// EarlyBinderOK is a constant-time binder check.
func EarlyBinderOK(e *EarlyKeys, chNoBinder []byte, binder [32]byte) bool {
	want := EarlyBinder(e, chNoBinder)
	return subtle.ConstantTimeCompare(want[:], binder[:]) == 1
}

// Zeroize clears a key-holding buffer.
func Zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
