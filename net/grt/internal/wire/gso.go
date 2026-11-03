package wire

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

var (
	ErrGSOChildCountExceeded     = errors.New("grt: GSO child count exceeds 64")
	ErrGSOByteLimitExceeded      = errors.New("grt: GSO aggregate payload exceeds 65507 bytes")
	ErrInvalidSegmentSize        = errors.New("grt: GSO segment size must be between 1200 and 4096")
	ErrSegmentSizeNotMultipleOf8 = errors.New("grt: GSO segment size must be a multiple of 8")
	ErrShortMiddleChild          = errors.New("grt: GSO non-final child length is not equal to segment size")
	ErrFinalChildInvalid         = errors.New("grt: GSO final child is too short or malformed")
	ErrPostTagPadding            = errors.New("grt: GSO packet contains post-tag raw padding")
	ErrIncompatibleBatchKey      = errors.New("grt: GSO child does not match batch compatibility key")
	ErrMixedCongestionExemption  = errors.New("grt: cannot mix congestion-exempt and congestion-charged packets in GSO batch")
	ErrTruncatedGROChild         = errors.New("grt: GRO child length truncated or invalid")
	ErrZeroLengthChild           = errors.New("grt: GSO child length is zero")
)

// BatchCompatibilityKey is the section-30.2 batch compatibility grouping key.
// All children combined into a single GSO submission must have matching keys.
type BatchCompatibilityKey struct {
	ConnGen          uint32
	PathSlot         uint8
	PathGen          uint32
	Local            netip.AddrPort
	Remote           netip.AddrPort
	CID              uint64
	SelectedCipher   uint16
	KeyEpoch         uint32
	KeyGen           uint32
	PMTUGen          uint32
	SegmentSize      uint16
	ECN              uint8
	PacingClass      uint8
	CongestionExempt bool
}

// GSOBuilder builds a compliant GSO superbuffer from independent child datagrams
// adhering to spec section 30.
type GSOBuilder struct {
	key         BatchCompatibilityKey
	segmentSize int
	children    [][]byte
	totalBytes  int
	finalAdded  bool
}

// NewGSOBuilder initializes a builder for a GSO batch with segment size S.
func NewGSOBuilder(key BatchCompatibilityKey) (*GSOBuilder, error) {
	s := int(key.SegmentSize)
	if s < BaseUDPPayload || s > MaxUDPPayload {
		return nil, ErrInvalidSegmentSize
	}
	if s%8 != 0 {
		return nil, ErrSegmentSizeNotMultipleOf8
	}
	return &GSOBuilder{
		key:         key,
		segmentSize: s,
		children:    make([][]byte, 0, MaxLocalBatchDatagrams),
	}, nil
}

// AddChild appends one independently sealed, complete child datagram to the superbuffer.
// Returns an error if any section 30 or 34.1 invariant is violated.
func (b *GSOBuilder) AddChild(pkt []byte, key BatchCompatibilityKey, isCongestionExempt bool) error {
	if b.finalAdded {
		// A shorter final child has already been added; no further child is allowed.
		return ErrShortMiddleChild
	}
	if len(pkt) == 0 {
		return ErrZeroLengthChild
	}
	if len(b.children) >= MaxLocalBatchDatagrams {
		return ErrGSOChildCountExceeded
	}
	if b.totalBytes+len(pkt) > SocketGSOMaxBytes {
		return ErrGSOByteLimitExceeded
	}

	// Section 30.2: batch compatibility check.
	if key.ConnGen != b.key.ConnGen ||
		key.PathSlot != b.key.PathSlot ||
		key.PathGen != b.key.PathGen ||
		key.Local != b.key.Local ||
		key.Remote != b.key.Remote ||
		key.CID != b.key.CID ||
		key.SelectedCipher != b.key.SelectedCipher ||
		key.KeyEpoch != b.key.KeyEpoch ||
		key.KeyGen != b.key.KeyGen ||
		key.PMTUGen != b.key.PMTUGen ||
		key.SegmentSize != b.key.SegmentSize ||
		key.ECN != b.key.ECN ||
		key.PacingClass != b.key.PacingClass {
		return ErrIncompatibleBatchKey
	}

	// Section 30.2: Do not mix congestion-exempt with congestion-charged packets.
	if isCongestionExempt != b.key.CongestionExempt {
		return ErrMixedCongestionExemption
	}

	// Validate packet wire geometry (Section 5, 30.1).
	if len(pkt) < HeaderBytes {
		return ErrFinalChildInvalid
	}
	magic := binary.BigEndian.Uint16(pkt[0:])
	if magic != Magic {
		return ErrFinalChildInvalid
	}
	pclass := pkt[3]
	payloadLen := binary.BigEndian.Uint16(pkt[24:])

	// Section 30.1: No raw post-tag padding. payload_length + 32 must equal total packet length for DATA.
	if pclass == ClassData {
		if int(payloadLen)+HeaderBytes != len(pkt) {
			return ErrPostTagPadding
		}
		if payloadLen < TagBytes+MinDataPlaintext {
			return ErrFinalChildInvalid
		}
	}

	if len(pkt) > b.segmentSize {
		return ErrInvalidSegmentSize
	}

	if len(pkt) < b.segmentSize {
		// A child shorter than S is permitted ONLY as the final child (Section 30.1).
		b.finalAdded = true
	}

	b.children = append(b.children, pkt)
	b.totalBytes += len(pkt)
	return nil
}

// ChildCount returns the number of children currently in the batch.
func (b *GSOBuilder) ChildCount() int {
	return len(b.children)
}

// TotalBytes returns the total payload bytes in the batch.
func (b *GSOBuilder) TotalBytes() int {
	return b.totalBytes
}

// Build concatenates and returns the superbuffer.
func (b *GSOBuilder) Build() ([]byte, error) {
	if len(b.children) == 0 {
		return nil, errors.New("grt: empty GSO batch")
	}
	out := make([]byte, b.totalBytes)
	off := 0
	for _, child := range b.children {
		copy(out[off:], child)
		off += len(child)
	}
	return out, nil
}

// GRONormalizer normalizes a GRO aggregate payload into child datagram views (spec section 30.6).
// segmentSize comes from the transport ancillary metadata (e.g. UDP_GRO).
// If segmentSize == 0 (no metadata), aggregate is treated as a single UDP datagram.
// Magic-scanning is strictly forbidden.
func GRONormalize(aggregate []byte, segmentSize uint16) ([][]byte, error) {
	if len(aggregate) == 0 {
		return nil, nil
	}
	if segmentSize == 0 {
		// Section 30.6: Without that metadata, the result is exactly one UDP datagram.
		// The receiver MUST NOT guess boundaries by scanning for the magic value.
		return [][]byte{aggregate}, nil
	}
	s := int(segmentSize)
	if s < BaseUDPPayload || s > MaxUDPPayload || s%8 != 0 {
		return nil, ErrInvalidSegmentSize
	}

	var children [][]byte
	off := 0
	for off < len(aggregate) {
		rem := len(aggregate) - off
		childLen := s
		if rem < s {
			// Final shorter child
			childLen = rem
		}
		if len(children) >= MaxLocalBatchDatagrams {
			return nil, ErrGSOChildCountExceeded
		}
		child := aggregate[off : off+childLen]

		// Independent child validation
		if len(child) < HeaderBytes {
			return nil, ErrTruncatedGROChild
		}
		magic := binary.BigEndian.Uint16(child[0:])
		if magic != Magic {
			return nil, ErrTruncatedGROChild
		}
		children = append(children, child)
		off += childLen
	}
	return children, nil
}
