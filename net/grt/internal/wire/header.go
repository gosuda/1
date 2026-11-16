package wire

import "encoding/binary"

// Header is the decoded 32-byte common packet header (spec section 5).
type Header struct {
	Class         uint8
	DestCID       uint64
	Epoch         uint32
	PacketNumber  uint64
	PayloadLength uint16 // ciphertext length including 16-byte tag
	AckEliciting  bool
}

// ParseHeader decodes the fixed 32-byte header. It checks magic, version,
// class, reserved-zero, and payload bounds. For bootstrap classes it requires
// the zero epoch/PN/flags/reserved fields and the class-fixed datagram size.
// DATA length consistency with the enclosing datagram is checked here too.
func ParseHeader(b []byte) (Header, bool) {
	var h Header
	if len(b) < HeaderBytes {
		return h, false
	}
	if binary.BigEndian.Uint16(b[0:]) != Magic || b[2] != Version {
		return h, false
	}
	h.Class = b[3]
	h.DestCID = binary.BigEndian.Uint64(b[4:])
	h.Epoch = binary.BigEndian.Uint32(b[12:])
	h.PacketNumber = binary.BigEndian.Uint64(b[16:])
	h.PayloadLength = binary.BigEndian.Uint16(b[24:])
	flags := binary.BigEndian.Uint16(b[26:])
	reserved := binary.BigEndian.Uint32(b[28:])
	if flags&^1 != 0 || reserved != 0 {
		return h, false
	}
	h.AckEliciting = flags&1 != 0
	switch h.Class {
	case ClassClientFlight, ClassServerFlight:
		if h.Epoch != 0 || h.PacketNumber != 0 || flags != 0 {
			return h, false
		}
		if len(b) != FlightPayload {
			return h, false
		}
		// payload_length covers the fragment area including padding;
		// it is always 1200-32 for a well-formed flight datagram.
		if int(h.PayloadLength) != FlightPayload-HeaderBytes {
			return h, false
		}
	case ClassRetry:
		if h.Epoch != 0 || h.PacketNumber != 0 || flags != 0 {
			return h, false
		}
		if len(b) != RetryUDPBytes || h.PayloadLength != RetryPayload {
			return h, false
		}
	case ClassData:
		// UDP payload length MUST equal 32 + payload_length; tag included.
		if len(b) != HeaderBytes+int(h.PayloadLength) {
			return h, false
		}
		// Decrypted plaintext must be >= 16 bytes: ciphertext is
		// plaintext+tag, so payload_length >= 16+16.
		if h.PayloadLength < 2*TagBytes {
			return h, false
		}
		if len(b) > MaxUDPPayload {
			return h, false
		}
	default:
		return h, false
	}
	return h, true
}

// BuildHeader writes the 32-byte header into b[0:32].
func BuildHeader(b []byte, class uint8, destCID uint64, epoch uint32, pn uint64, payloadLen uint16, ackEliciting bool) {
	binary.BigEndian.PutUint16(b[0:], Magic)
	b[2] = Version
	b[3] = class
	binary.BigEndian.PutUint64(b[4:], destCID)
	binary.BigEndian.PutUint32(b[12:], epoch)
	binary.BigEndian.PutUint64(b[16:], pn)
	binary.BigEndian.PutUint16(b[24:], payloadLen)
	var flags uint16
	if ackEliciting {
		flags = 1
	}
	binary.BigEndian.PutUint16(b[26:], flags)
	binary.BigEndian.PutUint32(b[28:], 0)
}

// BuildBootstrapHeader writes a CLIENT_FLIGHT/SERVER_FLIGHT header.
func BuildBootstrapHeader(b []byte, class uint8, destCID uint64) {
	BuildHeader(b, class, destCID, 0, 0, FlightPayload-HeaderBytes, false)
}

// BuildRetryHeader writes a RETRY header.
func BuildRetryHeader(b []byte, destCID uint64) {
	BuildHeader(b, ClassRetry, destCID, 0, 0, RetryPayload, false)
}
