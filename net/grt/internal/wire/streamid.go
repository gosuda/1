package wire

// StreamID is the slot+generation stream handle of spec section 12.1:
//
//	stream_id = (generation << 18) | (slot << 2) | (uni << 1) | initiator
//
// initiator=0 is the network client, 1 the network server; slot is 16-bit
// (<1024); generation is 46-bit.
type StreamID uint64

const (
	StreamInitiatorClient = 0
	StreamInitiatorServer = 1
)

// MakeStreamID composes a handle.
func MakeStreamID(generation uint64, slot uint16, uni bool, initiator uint8) StreamID {
	u := uint64(0)
	if uni {
		u = 1
	}
	return StreamID((generation << 18) | (uint64(slot) << 2) | (u << 1) | uint64(initiator&1))
}

// Generation extracts the 46-bit generation.
func (s StreamID) Generation() uint64 { return uint64(s) >> 18 }

// Slot extracts the 16-bit slot.
func (s StreamID) Slot() uint16 { return uint16(uint64(s)>>2) & 0xffff }

// Uni reports the unidirectional bit.
func (s StreamID) Uni() bool { return uint64(s)&2 != 0 }

// Initiator returns 0 for client-initiated, 1 for server-initiated.
func (s StreamID) Initiator() uint8 { return uint8(uint64(s) & 1) }

// Class is stream_id & 3 (uni<<1|initiator).
func (s StreamID) Class() uint8 { return uint8(uint64(s) & 3) }

// Valid reports whether the handle fits this version's bounds: slot < 1024
// and generation <= 2^46-1 (guaranteed by the 64-bit layout).
func (s StreamID) Valid() bool { return s.Slot() < MaxStreamSlots }

// StreamClassLimit identifies one of the four initiator/direction classes
// for MAX_SLOTS accounting. From the receiver's perspective only the two
// peer-initiated classes may be granted.
func PeerClass(localInitiator uint8, uni bool) uint8 {
	u := uint8(0)
	if uni {
		u = 1
	}
	// Peer-initiated class: initiator bit = remote's initiator.
	peer := localInitiator ^ 1
	return u<<1 | peer
}
