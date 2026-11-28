package wire

import (
	"encoding/binary"
	"errors"
)

// Frame is one parsed frame. Body aliases the plaintext buffer.
type Frame struct {
	Type  uint16
	Flags uint16
	Body  []byte // length = body_length (padding excluded)
}

var (
	ErrFrameTruncated = errors.New("grt: frame crosses plaintext end")
	ErrFramePadding   = errors.New("grt: nonzero alignment padding")
	ErrFramePartial   = errors.New("grt: partial final frame header")
	ErrFrameCount     = errors.New("grt: more than 32 frames")
	ErrFrameCritical  = errors.New("grt: unknown critical frame type")
	ErrFrameFlags     = errors.New("grt: nonzero flags on frame that forbids them")
)

// KnownTypes reports whether the frame type is defined by GRT/1.1.
func KnownType(t uint16) bool {
	switch t {
	case FramePadding, FramePing, FrameAck, FrameFinish, FrameHandshakeDone,
		FrameStreamOpen, FrameStream, FrameResetStream, FrameStopSending,
		FrameMaxData, FrameMaxStreamData, FrameMaxSlots,
		FrameDataBlocked, FrameStreamBlocked, FrameSlotsBlocked,
		FrameStreamRetire, FrameRetireAck, FrameDatagram,
		FramePathChallenge, FramePathResponse, FrameNewCID, FrameRetireCID,
		FrameNewTicket, FrameClose, FrameNewToken, FramePathIntent,
		FramePathSelected, FrameCryptoFragment, FrameKeyCommit,
		FrameKeyConfirm, FrameKeyRequest, FrameMTUProbe,
		FrameParkRequest, FrameParkGrant, FrameParkWake, FrameParkAwake:
		return true
	}
	return false
}

// frameFlagsValid enforces "known frame flags must be zero except where
// specified". Only STREAM defines a flag (bit 0 = FIN).
func frameFlagsValid(t, flags uint16) bool {
	if t == FrameStream {
		return flags&^FrameFlagStreamFin == 0
	}
	return flags == 0
}

// ParseFrames scans an authenticated plaintext into out (capacity MaxFrames)
// and returns the populated prefix. It enforces the complete grammar: header
// bounds, zero alignment padding, no partial trailing header, frame count,
// critical/skippable unknown-type policy, and flag validity.
//
// Skippable unknown frames (type bit 15 set) are dropped from the result but
// still mark the packet ack-eliciting.
func ParseFrames(plaintext []byte, out []Frame) (frames []Frame, eliciting bool, err error) {
	n := 0
	total := 0
	off := 0
	for off < len(plaintext) {
		if len(plaintext)-off < FrameHeaderBytes {
			return nil, false, ErrFramePartial
		}
		typ := binary.BigEndian.Uint16(plaintext[off:])
		flags := binary.BigEndian.Uint16(plaintext[off+2:])
		blen := binary.BigEndian.Uint32(plaintext[off+4:])
		bodyStart := off + FrameHeaderBytes
		bodyEnd := bodyStart + int(blen)
		if uint64(blen) > uint64(len(plaintext)) || bodyEnd > len(plaintext) {
			return nil, false, ErrFrameTruncated
		}
		pad := (-int(blen)) & 7
		padEnd := bodyEnd + pad
		if padEnd > len(plaintext) {
			return nil, false, ErrFrameTruncated
		}
		for i := bodyEnd; i < padEnd; i++ {
			if plaintext[i] != 0 {
				return nil, false, ErrFramePadding
			}
		}
		off = padEnd

		// Every frame header counts toward the packet limit, including
		// skipped unknown types (section 11).
		total++
		if total > MaxFrames {
			return nil, false, ErrFrameCount
		}
		if !KnownType(typ) {
			if typ&0x8000 != 0 {
				// Skippable unknown: eliciting, not delivered.
				eliciting = true
				continue
			}
			return nil, false, ErrFrameCritical
		}
		if !frameFlagsValid(typ, flags) {
			return nil, false, ErrFrameFlags
		}
		if typ == FramePadding {
			for i := bodyStart; i < bodyEnd; i++ {
				if plaintext[i] != 0 {
					return nil, false, ErrFramePadding
				}
			}
		}
		out = out[:n+1]
		out[n] = Frame{Type: typ, Flags: flags, Body: plaintext[bodyStart:bodyEnd]}
		n++
		if AckElicitingType(typ) {
			eliciting = true
		}
	}
	return out[:n], eliciting, nil
}

// AckElicitingType reports whether a frame type makes a packet ack-eliciting.
// CLOSE is non-eliciting: a pure CLOSE packet is not in-flight and promises
// no ACK (spec section 14.1, appendix A).
func AckElicitingType(t uint16) bool {
	return t != FramePadding && t != FrameAck && t != FrameClose
}

// ParseErrorCode maps a frame-grammar failure to a transport error code.
// Non-critical unknown types never reach this; all parse failures are
// FRAME_ENCODING_ERROR per spec section 11.
func ParseErrorCode(error) uint32 { return ErrFrameEncoding }

// FrameBuilder appends aligned frames to a plaintext buffer.
type FrameBuilder struct {
	buf    []byte
	count  int
	elicit bool
	padded bool
}

func NewFrameBuilder(buf []byte) *FrameBuilder { return &FrameBuilder{buf: buf} }

// Bytes returns the accumulated plaintext.
func (b *FrameBuilder) Bytes() []byte { return b.buf }
func (b *FrameBuilder) Count() int    { return b.count }
func (b *FrameBuilder) Eliciting() bool {
	return b.elicit
}

// Padded reports whether a PADDING frame was added. Padded packets count
// as in-flight and require an ack-eliciting sibling frame (spec 15.1/22).
func (b *FrameBuilder) Padded() bool {
	return b.padded
}

// Remaining reports how many frames may still be added.
func (b *FrameBuilder) Remaining() int { return MaxFrames - b.count }

// Len returns current plaintext length.
func (b *FrameBuilder) Len() int { return len(b.buf) }

// Reserve appends a frame header and padding, returning the slice for the body.
// Returns nil, false when the frame cap or buffer space is exhausted.
func (b *FrameBuilder) Reserve(typ, flags uint16, bodyLen int) ([]byte, bool) {
	if b.count >= MaxFrames {
		return nil, false
	}
	pad := (-bodyLen) & 7
	need := FrameHeaderBytes + bodyLen + pad
	if cap(b.buf)-len(b.buf) < need {
		return nil, false
	}
	base := len(b.buf)
	b.buf = b.buf[:base+need]
	hd := b.buf[base:]
	binary.BigEndian.PutUint16(hd[0:], typ)
	binary.BigEndian.PutUint16(hd[2:], flags)
	binary.BigEndian.PutUint32(hd[4:], uint32(bodyLen))
	clear(hd[FrameHeaderBytes+bodyLen:])
	b.count++
	if AckElicitingType(typ) {
		b.elicit = true
	}
	if typ == FramePadding {
		b.padded = true
	}
	return hd[FrameHeaderBytes : FrameHeaderBytes+bodyLen], true
}

// Add appends one frame. bodyLen is the unpadded body size; body bytes are
// written by fill, which receives b.buf[len:len+bodyLen].
// Returns false when the frame cap or buffer space is exhausted.
func (b *FrameBuilder) Add(typ, flags uint16, bodyLen int, fill func(dst []byte)) bool {
	dst, ok := b.Reserve(typ, flags, bodyLen)
	if !ok {
		return false
	}
	if fill != nil && bodyLen > 0 {
		fill(dst)
	}
	return true
}

// AddBytes is Add with a body copied from src.
func (b *FrameBuilder) AddBytes(typ, flags uint16, src []byte) bool {
	return b.Add(typ, flags, len(src), func(dst []byte) { copy(dst, src) })
}

// Pad appends a PADDING frame that brings the plaintext to at least
// minLen total bytes. Returns false if impossible (cap/space).
func (b *FrameBuilder) Pad(minLen int) bool {
	cur := len(b.buf)
	if cur >= minLen {
		return true
	}
	// A PADDING frame of body p occupies 8+p+pad8(p) bytes.
	// Solve smallest p >= 0 with cur + 8 + p + ((-p)&7) >= minLen.
	// Since p+pad8(p) rounds up to multiple of 8, choose:
	want := minLen - cur - FrameHeaderBytes
	if want <= 0 {
		want = 0
	}
	p := want
	for {
		pad := (-p) & 7
		if cur+FrameHeaderBytes+p+pad >= minLen {
			break
		}
		p++
	}
	return b.Add(FramePadding, 0, p, func(dst []byte) { clear(dst) })
}
