package wire

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// Establishment message types.
const (
	MsgClient uint8 = 1
	MsgServer uint8 = 2
)

// Authentication modes.
const (
	AuthPinned uint8 = 0
	AuthChain  uint8 = 1
)

// Field sizes.
const (
	AttemptIDBytes   = 16
	Nonce32Bytes     = 32
	MLKEMEncapKeyLen = 1184
	MLKEMCTLen       = 1088
	SignatureLen     = 64
	AppIDLen         = 16
	TicketBytes      = 224
	TicketMetaBytes  = 204
	BinderBytes      = 32
	TicketPlainLen   = 180

	// ClientPrefixBytes is the fixed client-message prefix through
	// original_dcid (the part a tokenless fragment zero must carry).
	ClientPrefixBytes = 76

	ServerPrefixBytes = 1316
)

var (
	ErrFlightBounds    = errors.New("grt: flight message out of bounds")
	ErrOfferMalformed  = errors.New("grt: malformed cipher offer")
	ErrAuthMode        = errors.New("grt: bad authentication mode/pin shape")
	ErrAuthoritySyntax = errors.New("grt: noncanonical authority string")
	ErrTicketShape     = errors.New("grt: bad ticket/early fields")
	ErrFlightMismatch  = errors.New("grt: flight field mismatch")
)

// ClientMessage is a parsed/encoded §8.2 client flight.
type ClientMessage struct {
	Offer             [MaxCipherOffers]uint16 // Offer[1]==0 means a single offer
	AttemptID         [AttemptIDBytes]byte
	ClientNonce       [32]byte
	ClientInitialCID  uint64
	OriginalDCID      uint64
	AppID             [AppIDLen]byte
	AuthMode          uint8
	RequestedEarly    bool
	Authority         string // canonical, 1..253 bytes
	ExpectedServerKey [32]byte
	IKC               [32]byte
	SPKC              [32]byte
	SPKSignature      [SignatureLen]byte
	PQPKC             [MLKEMEncapKeyLen]byte
	PQPKSignature     [SignatureLen]byte
	Params            [ParamsBytes]byte
	Ticket            []byte // empty or exactly TicketBytes
	TicketMetadata    [TicketMetaBytes]byte
	EarlyCiphertext   []byte
	Binder            [BinderBytes]byte
}

// EarlyStatus values for the server body.
const (
	EarlyAbsent   uint8 = 0
	EarlyAccepted uint8 = 1
	EarlyRejected uint8 = 2
)

// ServerMessage is a parsed/encoded §8.3 server flight: SP prefix plus the
// decrypted body (populated only after successful AEAD open).
type ServerMessage struct {
	SelectedCipher     uint16
	ServerCipherMask   uint16
	AttemptID          [AttemptIDBytes]byte
	ClientFlightHash   [32]byte
	ServerNonce        [32]byte
	ClientInitialCID   uint64
	ServerInitialCID   uint64
	OriginalDCID       uint64
	AcceptedRoutingCID uint64
	AcceptedTokenHash  [32]byte
	IKS                [32]byte
	EKS                [32]byte
	CTS                [MLKEMCTLen]byte
	PSKAccepted        bool

	// Body fields (post-decrypt):
	Params              Params
	Credentials         [][CredentialBytes]byte
	ServerResetToken    [16]byte
	EarlyPlaintextHash  [32]byte
	EarlyStatus         uint8
	ServerFinishedMAC   [32]byte
	EncryptedBodyLength uint32

	// Raw prefix and sealed body for transcript purposes.
	RawSP     []byte
	RawBodyCT []byte
}

// ValidateAuthority checks the canonical authority syntax of spec section 7:
// 1..253 bytes, lowercase DNS A-label host (no trailing dot) or bracketed
// IPv6 literal, ':', decimal port without leading zeros.
func ValidateAuthority(a string) bool {
	if len(a) == 0 || len(a) > 253 {
		return false
	}
	colon := -1
	for i := len(a) - 1; i >= 0; i-- {
		if a[i] == ':' {
			colon = i
			break
		}
	}
	if colon <= 0 || colon == len(a)-1 {
		return false
	}
	host := a[:colon]
	port := a[colon+1:]
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	if len(port) > 1 && port[0] == '0' {
		return false
	}
	// Port must fit u16.
	pv := 0
	for i := 0; i < len(port); i++ {
		pv = pv*10 + int(port[i]-'0')
		if pv > 65535 {
			return false
		}
	}
	if pv == 0 {
		return false
	}
	if host[0] == '[' {
		// Bracketed IPv6 literal: validate hex/colon shape minimally;
		// full canonicalization is a deployment responsibility.
		if len(host) < 4 || host[len(host)-1] != ']' {
			return false
		}
		inner := host[1 : len(host)-1]
		if len(inner) < 2 {
			return false
		}
		for i := 0; i < len(inner); i++ {
			if !isIPv6Char(inner[i]) {
				return false
			}
		}
		return true
	}
	if len(host) > 253 || host[len(host)-1] == '.' {
		return false
	}
	// Labels: a-z 0-9 '-', not starting/ending with '-', non-empty.
	start := 0
	for i := 0; i <= len(host); i++ {
		if i == len(host) || host[i] == '.' {
			if i == start {
				return false
			}
			if host[start] == '-' || host[i-1] == '-' {
				return false
			}
			start = i + 1
			continue
		}
		if !isLabelChar(host[i]) {
			return false
		}
	}
	return true
}

// isIPv6Char reports whether c is valid inside a bracketed IPv6 literal.
func isIPv6Char(c byte) bool {
	isDigit := c >= '0' && c <= '9'
	isHex := c >= 'a' && c <= 'f'
	return c == ':' || isDigit || isHex
}

// isLabelChar reports whether c is a valid DNS label byte.
func isLabelChar(c byte) bool {
	isLower := c >= 'a' && c <= 'z'
	isDigit := c >= '0' && c <= '9'
	return isLower || isDigit || c == '-'
}

// validOffer checks the §4.1 offer rules.
func validOffer(o [MaxCipherOffers]uint16) bool {
	if o[0] == 0 {
		return false
	}
	if o[1] != 0 && o[1] == o[0] {
		return false
	}
	hasXChaCha := false
	for _, c := range o {
		switch c {
		case 0:
		case CipherXChaCha20Poly1305:
			hasXChaCha = true
		case CipherAES256GCM:
		default:
			return false
		}
	}
	return hasXChaCha
}

// EncodeOffer builds the fixed 8-byte cipher_offer field.
func EncodeOffer(out []byte, o [MaxCipherOffers]uint16) {
	count := uint8(1)
	if o[1] != 0 {
		count = 2
	}
	out[0] = count
	out[1] = 0
	binary.BigEndian.PutUint16(out[2:], o[0])
	binary.BigEndian.PutUint16(out[4:], o[1])
	binary.BigEndian.PutUint16(out[6:], 0)
}

func decodeOffer(b []byte) ([MaxCipherOffers]uint16, bool) {
	var o [MaxCipherOffers]uint16
	count := b[0]
	if count < 1 || count > 2 || b[1] != 0 {
		return o, false
	}
	o[0] = binary.BigEndian.Uint16(b[2:])
	o[1] = binary.BigEndian.Uint16(b[4:])
	if binary.BigEndian.Uint16(b[6:]) != 0 {
		return o, false
	}
	if count == 1 && o[1] != 0 {
		return o, false
	}
	if count == 2 && o[1] == 0 {
		return o, false
	}
	if !validOffer(o) {
		return o, false
	}
	return o, true
}

// EncodeClientMessage serializes m. It returns the exact immutable message
// bytes (to be hashed and fragmented).
func EncodeClientMessage(m *ClientMessage) ([]byte, error) {
	if !validOffer(m.Offer) {
		return nil, ErrOfferMalformed
	}
	if m.AuthMode > AuthChain {
		return nil, ErrAuthMode
	}
	if m.AuthMode == AuthPinned && m.ExpectedServerKey == ([32]byte{}) {
		return nil, ErrAuthMode
	}
	if m.AuthMode == AuthChain && m.ExpectedServerKey != ([32]byte{}) {
		return nil, ErrAuthMode
	}
	if !ValidateAuthority(m.Authority) {
		return nil, ErrAuthoritySyntax
	}
	if len(m.Ticket) != 0 && len(m.Ticket) != TicketBytes {
		return nil, ErrTicketShape
	}
	if len(m.Ticket) == 0 && m.RequestedEarly {
		return nil, ErrTicketShape
	}
	if len(m.Ticket) == 0 && len(m.EarlyCiphertext) != 0 {
		return nil, ErrTicketShape
	}
	if len(m.EarlyCiphertext) > MaxEarlyBytes+TagBytes {
		return nil, ErrTicketShape
	}
	if m.ClientInitialCID == 0 || m.OriginalDCID == 0 {
		return nil, ErrFlightMismatch
	}
	total := 98 + len(m.Authority) + 1536 + 36 + len(m.Ticket) + len(m.EarlyCiphertext)
	if len(m.Ticket) != 0 {
		total += TicketMetaBytes
	}
	if total > MaxFlightBytes {
		return nil, ErrFlightBounds
	}
	out := make([]byte, total)
	out[0] = MsgClient
	out[1] = Version
	binary.BigEndian.PutUint16(out[2:], KexProfile)
	EncodeOffer(out[4:], m.Offer)
	copy(out[12:], m.AttemptID[:])
	copy(out[28:], m.ClientNonce[:])
	binary.BigEndian.PutUint64(out[60:], m.ClientInitialCID)
	binary.BigEndian.PutUint64(out[68:], m.OriginalDCID)
	copy(out[76:], m.AppID[:])
	out[92] = m.AuthMode
	if m.RequestedEarly {
		out[93] = 1
	}
	binary.BigEndian.PutUint16(out[94:], 0)
	binary.BigEndian.PutUint16(out[96:], uint16(len(m.Authority)))
	copy(out[98:], m.Authority)
	p := 98 + len(m.Authority)
	copy(out[p:], m.ExpectedServerKey[:])
	copy(out[p+32:], m.IKC[:])
	copy(out[p+64:], m.SPKC[:])
	copy(out[p+96:], m.SPKSignature[:])
	copy(out[p+160:], m.PQPKC[:])
	copy(out[p+1344:], m.PQPKSignature[:])
	copy(out[p+1408:], m.Params[:])
	p += 1536
	if len(m.Ticket) == 0 {
		binary.BigEndian.PutUint16(out[p:], 0)
		p += 2
	} else {
		binary.BigEndian.PutUint16(out[p:], uint16(len(m.Ticket)))
		copy(out[p+2:], m.Ticket)
		copy(out[p+2+TicketBytes:], m.TicketMetadata[:])
		p += 2 + TicketBytes + TicketMetaBytes
	}
	binary.BigEndian.PutUint16(out[p:], uint16(len(m.EarlyCiphertext)))
	copy(out[p+2:], m.EarlyCiphertext)
	p += 2 + len(m.EarlyCiphertext)
	copy(out[p:], m.Binder[:])
	p += BinderBytes
	if p != total {
		return nil, ErrFlightBounds
	}
	return out, nil
}

// ParseClientMessage decodes a complete client flight. It enforces every
// fixed field, reserved zero, and shape rule. CH0End and NoBinderLen are
// returned for the §9.2 early-key derivations.
func ParseClientMessage(b []byte) (m ClientMessage, ch0End, noBinderLen int, err error) {
	if len(b) < 98+1536+36 || len(b) > MaxFlightBytes {
		return m, 0, 0, ErrFlightBounds
	}
	if b[0] != MsgClient || b[1] != Version || binary.BigEndian.Uint16(b[2:]) != KexProfile {
		return m, 0, 0, ErrFlightMismatch
	}
	offer, ok := decodeOffer(b[4:])
	if !ok {
		return m, 0, 0, ErrOfferMalformed
	}
	m.Offer = offer
	copy(m.AttemptID[:], b[12:28])
	copy(m.ClientNonce[:], b[28:60])
	m.ClientInitialCID = binary.BigEndian.Uint64(b[60:])
	m.OriginalDCID = binary.BigEndian.Uint64(b[68:])
	if m.ClientInitialCID == 0 || m.OriginalDCID == 0 {
		return m, 0, 0, ErrFlightMismatch
	}
	copy(m.AppID[:], b[76:92])
	m.AuthMode = b[92]
	if m.AuthMode > AuthChain {
		return m, 0, 0, ErrAuthMode
	}
	if b[93] > 1 {
		return m, 0, 0, ErrFlightMismatch
	}
	m.RequestedEarly = b[93] == 1
	if binary.BigEndian.Uint16(b[94:]) != 0 {
		return m, 0, 0, ErrFlightMismatch
	}
	alen := int(binary.BigEndian.Uint16(b[96:]))
	if alen < 1 || alen > 253 {
		return m, 0, 0, ErrAuthoritySyntax
	}
	if 98+alen+1536+36 > len(b) {
		return m, 0, 0, ErrFlightBounds
	}
	m.Authority = string(b[98 : 98+alen])
	if !ValidateAuthority(m.Authority) {
		return m, 0, 0, ErrAuthoritySyntax
	}
	p := 98 + alen
	copy(m.ExpectedServerKey[:], b[p:p+32])
	copy(m.IKC[:], b[p+32:p+64])
	copy(m.SPKC[:], b[p+64:p+96])
	copy(m.SPKSignature[:], b[p+96:p+160])
	copy(m.PQPKC[:], b[p+160:p+1344])
	copy(m.PQPKSignature[:], b[p+1344:p+1408])
	copy(m.Params[:], b[p+1408:p+1536])
	p += 1536
	if m.AuthMode == AuthPinned && m.ExpectedServerKey == ([32]byte{}) {
		return m, 0, 0, ErrAuthMode
	}
	if m.AuthMode == AuthChain && m.ExpectedServerKey != ([32]byte{}) {
		return m, 0, 0, ErrAuthMode
	}
	tlen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	if tlen != 0 {
		if tlen != TicketBytes {
			return m, 0, 0, ErrTicketShape
		}
		if p+TicketBytes+TicketMetaBytes+2+BinderBytes > len(b) {
			return m, 0, 0, ErrFlightBounds
		}
		m.Ticket = append([]byte(nil), b[p:p+TicketBytes]...)
		copy(m.TicketMetadata[:], b[p+TicketBytes:p+TicketBytes+TicketMetaBytes])
		p += TicketBytes + TicketMetaBytes
	}
	ch0End = p
	elen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	if p+elen+BinderBytes != len(b) {
		return m, 0, 0, ErrFlightBounds
	}
	if elen > 0 {
		if elen > MaxEarlyBytes+TagBytes {
			return m, 0, 0, ErrTicketShape
		}
		m.EarlyCiphertext = append([]byte(nil), b[p:p+elen]...)
	}
	p += elen
	copy(m.Binder[:], b[p:p+BinderBytes])
	if tlen == 0 {
		if m.RequestedEarly || elen != 0 || m.Binder != ([32]byte{}) {
			return m, 0, 0, ErrTicketShape
		}
	}
	return m, ch0End, len(b) - BinderBytes, nil
}

// ClientFlightHash computes H(complete client message).
func ClientFlightHash(msg []byte) [32]byte { return sha256.Sum256(msg) }

// EncodeServerPrefix serializes SP (spec section 8.3) into a new slice.
func EncodeServerPrefix(s *ServerMessage) []byte {
	out := make([]byte, ServerPrefixBytes)
	out[0] = MsgServer
	out[1] = Version
	binary.BigEndian.PutUint16(out[2:], KexProfile)
	binary.BigEndian.PutUint16(out[4:], s.SelectedCipher)
	binary.BigEndian.PutUint16(out[6:], s.ServerCipherMask)
	copy(out[8:], s.AttemptID[:])
	copy(out[24:], s.ClientFlightHash[:])
	copy(out[56:], s.ServerNonce[:])
	binary.BigEndian.PutUint64(out[88:], s.ClientInitialCID)
	binary.BigEndian.PutUint64(out[96:], s.ServerInitialCID)
	binary.BigEndian.PutUint64(out[104:], s.OriginalDCID)
	binary.BigEndian.PutUint64(out[112:], s.AcceptedRoutingCID)
	copy(out[120:], s.AcceptedTokenHash[:])
	copy(out[152:], s.IKS[:])
	copy(out[184:], s.EKS[:])
	copy(out[216:], s.CTS[:])
	if s.PSKAccepted {
		out[1304] = 1
	}
	clear(out[1305:1312])
	binary.BigEndian.PutUint32(out[1312:], s.EncryptedBodyLength)
	return out
}

// ParseServerPrefix decodes and validates SP. It does not authenticate
// anything; the caller verifies the transcript before trusting fields.
func ParseServerPrefix(b []byte) (ServerMessage, error) {
	var s ServerMessage
	if len(b) < ServerPrefixBytes+TagBytes {
		return s, ErrFlightBounds
	}
	if b[0] != MsgServer || b[1] != Version || binary.BigEndian.Uint16(b[2:]) != KexProfile {
		return s, ErrFlightMismatch
	}
	s.SelectedCipher = binary.BigEndian.Uint16(b[4:])
	s.ServerCipherMask = binary.BigEndian.Uint16(b[6:])
	if s.SelectedCipher != CipherXChaCha20Poly1305 && s.SelectedCipher != CipherAES256GCM {
		return s, ErrFlightMismatch
	}
	if s.ServerCipherMask&^3 != 0 || s.ServerCipherMask&1 == 0 {
		return s, ErrFlightMismatch
	}
	copy(s.AttemptID[:], b[8:24])
	copy(s.ClientFlightHash[:], b[24:56])
	copy(s.ServerNonce[:], b[56:88])
	s.ClientInitialCID = binary.BigEndian.Uint64(b[88:])
	s.ServerInitialCID = binary.BigEndian.Uint64(b[96:])
	s.OriginalDCID = binary.BigEndian.Uint64(b[104:])
	s.AcceptedRoutingCID = binary.BigEndian.Uint64(b[112:])
	copy(s.AcceptedTokenHash[:], b[120:152])
	copy(s.IKS[:], b[152:184])
	copy(s.EKS[:], b[184:216])
	copy(s.CTS[:], b[216:1304])
	if b[1304] > 1 {
		return s, ErrFlightMismatch
	}
	s.PSKAccepted = b[1304] == 1
	for i := 1305; i < 1312; i++ {
		if b[i] != 0 {
			return s, ErrFlightMismatch
		}
	}
	s.EncryptedBodyLength = binary.BigEndian.Uint32(b[1312:])
	// body = SB || server_finished(32) || tag(16)
	if s.EncryptedBodyLength < 48+48 || int(s.EncryptedBodyLength) > MaxFlightBytes-ServerPrefixBytes {
		return s, ErrFlightBounds
	}
	if len(b) != ServerPrefixBytes+int(s.EncryptedBodyLength) {
		return s, ErrFlightBounds
	}
	s.RawSP = b[:ServerPrefixBytes]
	s.RawBodyCT = b[ServerPrefixBytes:]
	return s, nil
}

// EncodeServerBody serializes SB (without finished/tag).
func EncodeServerBody(s *ServerMessage) []byte {
	sb := make([]byte, 128+4+CredentialBytes*len(s.Credentials)+16+32)
	pw := s.ParamsWire()
	copy(sb[0:], pw[:])
	sb[128] = uint8(len(s.Credentials))
	sb[129] = s.EarlyStatus
	binary.BigEndian.PutUint16(sb[130:], 0)
	p := 132
	for i := range s.Credentials {
		copy(sb[p:], s.Credentials[i][:])
		p += CredentialBytes
	}
	copy(sb[p:], s.ServerResetToken[:])
	p += 16
	copy(sb[p:], s.EarlyPlaintextHash[:])
	return sb
}

// ParamsWire returns the encoded 128-byte parameters.
func (s *ServerMessage) ParamsWire() [ParamsBytes]byte {
	var b [ParamsBytes]byte
	EncodeParams(b[:], &s.Params)
	return b
}

// ParseServerBody decodes a decrypted SB (without the finished MAC, which the
// caller splits off before calling).
func ParseServerBody(sb []byte, s *ServerMessage) error {
	if len(sb) < 132+48 {
		return ErrFlightBounds
	}
	p, ok := DecodeParams(sb[:128])
	if !ok {
		return fmt.Errorf("%w: server params", ErrParamsInvalid)
	}
	s.Params = p
	ncred := int(sb[128])
	if ncred > MaxCredentials {
		return ErrFlightMismatch
	}
	s.EarlyStatus = sb[129]
	if s.EarlyStatus > EarlyRejected {
		return ErrFlightMismatch
	}
	if binary.BigEndian.Uint16(sb[130:]) != 0 {
		return ErrFlightMismatch
	}
	need := 132 + ncred*CredentialBytes + 48
	if len(sb) != need {
		return ErrFlightBounds
	}
	off := 132
	s.Credentials = make([][CredentialBytes]byte, ncred)
	for i := 0; i < ncred; i++ {
		copy(s.Credentials[i][:], sb[off:])
		off += CredentialBytes
	}
	copy(s.ServerResetToken[:], sb[off:off+16])
	copy(s.EarlyPlaintextHash[:], sb[off+16:off+48])
	return nil
}

var ErrParamsInvalid = errors.New("grt: transport parameter error")

// Fragment describes one parsed bootstrap fragment header plus its data.
type Fragment struct {
	AttemptID   [AttemptIDBytes]byte
	MessageHash [32]byte
	TotalLength uint32
	Index       uint16
	Count       uint16
	Token       [TokenBytes]byte
	Data        []byte // aliases datagram payload
}

// ParseFragment decodes a CLIENT_FLIGHT/SERVER_FLIGHT payload (the 1168 bytes
// after the 32-byte header). It enforces the §6.1 layout: chunk count and
// sizes consistent with total length, nonfinal chunks exactly 1024 bytes,
// and zero tail padding.
func ParseFragment(payload []byte) (Fragment, bool) {
	var f Fragment
	if len(payload) != FlightPayload-HeaderBytes {
		return f, false
	}
	copy(f.AttemptID[:], payload[0:16])
	copy(f.MessageHash[:], payload[16:48])
	f.TotalLength = binary.BigEndian.Uint32(payload[48:])
	f.Index = binary.BigEndian.Uint16(payload[52:])
	f.Count = binary.BigEndian.Uint16(payload[54:])
	copy(f.Token[:], payload[56:120])
	if f.TotalLength < 1 || f.TotalLength > MaxFlightBytes {
		return f, false
	}
	wantCount := (f.TotalLength + BootstrapChunk - 1) / BootstrapChunk
	if f.Count < 1 || f.Count > MaxFlightChunks || uint32(f.Count) != wantCount {
		return f, false
	}
	if f.Index >= f.Count {
		return f, false
	}
	// Fragment data region is payload[120:1168) (1048 bytes); a chunk is at
	// most 1024 bytes, so at least 24 zero padding bytes always follow.
	dataArea := payload[FragHeaderBytes:]
	var dlen int
	if f.Index == f.Count-1 {
		dlen = int(f.TotalLength) - int(f.Index)*BootstrapChunk
	} else {
		dlen = BootstrapChunk
	}
	if dlen < 1 || dlen > BootstrapChunk || dlen > len(dataArea) {
		return f, false
	}
	for i := dlen; i < len(dataArea); i++ {
		if dataArea[i] != 0 {
			return f, false
		}
	}
	f.Data = dataArea[:dlen]
	return f, true
}

// BuildFragments splits message into CLIENT/SERVER flight datagrams: exactly
// 1200-byte UDP payloads with the §6.1 fragment header, chunk data, and zero
// padding. token is 64 bytes (all zero when absent). destCID is the outer
// destination CID for every fragment.
func BuildFragments(message []byte, class uint8, destCID uint64, attemptID [AttemptIDBytes]byte, token [TokenBytes]byte) [][]byte {
	total := uint32(len(message))
	count := (total + BootstrapChunk - 1) / BootstrapChunk
	hash := sha256.Sum256(message)
	frags := make([][]byte, count)
	for i := uint32(0); i < count; i++ {
		pkt := make([]byte, FlightPayload)
		BuildBootstrapHeader(pkt, class, destCID)
		pl := pkt[HeaderBytes:]
		copy(pl[0:16], attemptID[:])
		copy(pl[16:48], hash[:])
		binary.BigEndian.PutUint32(pl[48:], total)
		binary.BigEndian.PutUint16(pl[52:], uint16(i))
		binary.BigEndian.PutUint16(pl[54:], uint16(count))
		copy(pl[56:120], token[:])
		start := i * BootstrapChunk
		end := min(start+BootstrapChunk, total)
		copy(pl[FragHeaderBytes:], message[start:end])
		frags[i] = pkt
	}
	return frags
}

// BuildRetryDatagram constructs one complete 160-byte RETRY datagram
// (spec section 6.3). token is the issued 64-byte kind-0 token.
func BuildRetryDatagram(attemptID [16]byte, clientFlightHash [32]byte, originalDCID, retryDCID uint64, token [TokenBytes]byte, clientInitialCID uint64) []byte {
	pkt := make([]byte, RetryUDPBytes)
	BuildRetryHeader(pkt, clientInitialCID)
	pl := pkt[HeaderBytes:]
	copy(pl[0:], attemptID[:])
	copy(pl[16:], clientFlightHash[:])
	binary.BigEndian.PutUint64(pl[48:], originalDCID)
	binary.BigEndian.PutUint64(pl[56:], retryDCID)
	copy(pl[64:], token[:])
	return pkt
}

// Retry is a parsed RETRY payload.
type Retry struct {
	AttemptID        [16]byte
	ClientFlightHash [32]byte
	OriginalDCID     uint64
	RetryDCID        uint64
	Token            [TokenBytes]byte
}

// ParseRetry decodes the 128-byte RETRY payload.
func ParseRetry(payload []byte) (Retry, bool) {
	var r Retry
	if len(payload) != RetryPayload {
		return r, false
	}
	copy(r.AttemptID[:], payload[0:16])
	copy(r.ClientFlightHash[:], payload[16:48])
	r.OriginalDCID = binary.BigEndian.Uint64(payload[48:])
	r.RetryDCID = binary.BigEndian.Uint64(payload[56:])
	copy(r.Token[:], payload[64:128])
	if r.RetryDCID == 0 {
		return r, false
	}
	return r, true
}
