package wire

import (
	"encoding/binary"
	"net/netip"
)

// Token is the 64-byte address token body (spec section 6.2). The HMAC tag
// occupies bytes 32..63.
type Token struct {
	KeyID        uint32
	IssuedUnixS  uint64
	ExpiresUnixS uint64
	Binding      uint64
	Kind         uint8
	Tag          [32]byte
}

// EncodeToken serializes t (with its tag) into out[0:64].
func EncodeToken(out []byte, t *Token) {
	binary.BigEndian.PutUint32(out[0:], t.KeyID)
	binary.BigEndian.PutUint64(out[4:], t.IssuedUnixS)
	binary.BigEndian.PutUint64(out[12:], t.ExpiresUnixS)
	binary.BigEndian.PutUint64(out[20:], t.Binding)
	out[28] = t.Kind
	clear(out[29:32])
	copy(out[32:], t.Tag[:])
}

// DecodeToken parses the fixed token encoding. It checks kind, the reserved
// zero bytes, and issue/expiry ordering; MAC and lifetime are verified by the
// caller (crypto token service).
func DecodeToken(b []byte) (Token, bool) {
	var t Token
	if len(b) < TokenBytes {
		return t, false
	}
	t.KeyID = binary.BigEndian.Uint32(b[0:])
	t.IssuedUnixS = binary.BigEndian.Uint64(b[4:])
	t.ExpiresUnixS = binary.BigEndian.Uint64(b[12:])
	t.Binding = binary.BigEndian.Uint64(b[20:])
	t.Kind = b[28]
	if t.Kind > TokenKindReuse {
		return t, false
	}
	if b[29] != 0 || b[30] != 0 || b[31] != 0 {
		return t, false
	}
	if t.IssuedUnixS > t.ExpiresUnixS {
		return t, false
	}
	copy(t.Tag[:], b[32:])
	return t, true
}

// CanonicalEndpoint encodes an endpoint as family:u8 || address:16 || port:u16
// (spec section 6.2). IPv4 is normalized to family 4 with 12 leading zero
// bytes; IPv4-mapped IPv6 is normalized to family 4. ok=false for non-IP
// endpoints (zone handling is a local routing concern, not encoded).
func CanonicalEndpoint(ap netip.AddrPort, out []byte) bool {
	addr := ap.Addr().Unmap()
	if !addr.Is4() && !addr.Is6() {
		return false
	}
	if addr.Is4() {
		out[0] = 4
		clear(out[1:13])
		a4 := addr.As4()
		copy(out[13:17], a4[:])
	} else {
		out[0] = 6
		a16 := addr.As16()
		copy(out[1:17], a16[:])
	}
	binary.BigEndian.PutUint16(out[17:], ap.Port())
	return true
}

// TokenCommonInput builds the HMAC input prefix shared by both token kinds:
//
//	"GRT11 address" || token[0:32] || canonical_source ||
//	canonical_local_destination || listener_domain || u8(VERSION)
//
// The caller appends (client_flight_hash || u64(retry_dcid)) for kind 0 or
// (Z(32) || Z(8)) for kind 1 before HMAC.
func TokenCommonInput(dst []byte, tokenHead []byte, source, localDest netip.AddrPort, listenerDomain []byte) []byte {
	dst = append(dst, "GRT11 address"...)
	dst = append(dst, tokenHead[:32]...)
	var src, loc [CanonicalEPBytes]byte
	if !CanonicalEndpoint(source, src[:]) || !CanonicalEndpoint(localDest, loc[:]) {
		return nil
	}
	dst = append(dst, src[:]...)
	dst = append(dst, loc[:]...)
	dst = append(dst, listenerDomain...)
	dst = append(dst, Version)
	return dst
}
