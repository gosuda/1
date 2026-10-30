package wire

import "encoding/binary"

// Params is the 128-byte transport-parameter structure (spec section 10).
// All fields are validated at decode; unused tail bytes must be zero.
type Params struct {
	FeatureFlags         uint16
	MaxUDPPayload        uint16
	MaxFrames            uint16
	IdleTimeoutMS        uint32
	MaxAckDelayUS        uint32
	InitialMaxData       uint64
	InitialStreamWindow  uint32
	ReceivePacketHistory uint32
	InitialBidiSlots     uint16
	InitialUniSlots      uint16
	MaximumBidiSlots     uint16
	MaximumUniSlots      uint16
	ActiveCIDLimit       uint16
	MaxAckRanges         uint16
	MaxDatagramData      uint32
	MaxCryptoMessage     uint32
	MaxPendingControl    uint32
	MaxAckRatio          uint16
	InitialAckRatio      uint16
	MaxParkDurationMS    uint32
	MaxPathValidationMS  uint32
	CandidatePathLimit   uint16
	RetainedOldPathLimit uint16
}

// Preset identifies a standardized transport profile.
type Preset uint8

const (
	// PresetLowResource enforces the Section 36 Compact session profile (zero initial credit, ~3KB footprint).
	PresetLowResource Preset = iota
	// PresetLowLatency pre-funds initial credit (64KB) for zero-credit-RTT immediate request/response.
	PresetLowLatency
	// PresetStandard1RTT disables early-data replay exposure and funds 1MB credit for strict PFS.
	PresetStandard1RTT
	// PresetHighThroughput configures 4KB UDP payloads and 16MB credit for gigabit streaming.
	PresetHighThroughput
)

// String returns the human-readable profile name.
func (p Preset) String() string {
	switch p {
	case PresetLowResource:
		return "LowResource"
	case PresetLowLatency:
		return "LowLatency"
	case PresetStandard1RTT:
		return "Standard1RTT"
	case PresetHighThroughput:
		return "HighThroughput"
	default:
		return "Unknown"
	}
}

// LowResourceParams returns the Compact-profile defaults of spec sections 3 and 36.
func LowResourceParams() Params {
	return DefaultParams()
}

// LowLatencyParams returns parameters pre-funded with 64KB initial credit and
// stream windows, eliminating the credit round trip before first request bytes.
func LowLatencyParams() Params {
	p := DefaultParams()
	p.InitialMaxData = 65536
	p.InitialStreamWindow = 65536
	p.InitialBidiSlots = 16
	p.InitialUniSlots = 4
	p.ReceivePacketHistory = 16384
	p.MaxAckDelayUS = 500
	return p
}

// Standard1RTTParams returns parameters for safe 1-RTT operation: early-data
// feature flag is disabled to eliminate 0-RTT replay attack surface, while
// initial credit is pre-funded with 1MB for fast, responsive streaming.
func Standard1RTTParams() Params {
	p := DefaultParams()
	p.FeatureFlags &^= FeatureEarly
	p.InitialMaxData = 1048576
	p.InitialStreamWindow = 262144
	p.InitialBidiSlots = 64
	p.InitialUniSlots = 16
	p.ReceivePacketHistory = 16384
	return p
}

// HighThroughputParams returns parameters configured for line-rate data transfers:
// 4096-byte UDP payloads (GSO-optimized), 16MB initial credit, 1MB stream windows,
// 128 initial stream slots, and an initial ACK ratio of 8 (section 13.2's
// adaptive controller widens it toward the ceiling under sustained load).
func HighThroughputParams() Params {
	p := DefaultParams()
	p.MaxUDPPayload = 4096
	p.InitialMaxData = 16777216
	p.InitialStreamWindow = 1048576
	p.InitialBidiSlots = 128
	p.InitialUniSlots = 64
	p.ReceivePacketHistory = 65536
	p.ActiveCIDLimit = 4
	p.CandidatePathLimit = 1
	p.RetainedOldPathLimit = 1
	p.InitialAckRatio = 8
	return p
}

// Params returns the negotiated transport parameters for the preset.
func (p Preset) Params() Params {
	switch p {
	case PresetLowLatency:
		return LowLatencyParams()
	case PresetStandard1RTT:
		return Standard1RTTParams()
	case PresetHighThroughput:
		return HighThroughputParams()
	case PresetLowResource:
		fallthrough
	default:
		return LowResourceParams()
	}
}

// DefaultParams returns the Compact-profile defaults of spec sections 3 and 36.
func DefaultParams() Params {
	return Params{
		FeatureFlags:         FeatureDatagram | FeatureEarly | FeatureMigration | FeaturePark,
		MaxUDPPayload:        BaseUDPPayload,
		MaxFrames:            MaxFrames,
		IdleTimeoutMS:        DefaultIdleMS,
		MaxAckDelayUS:        DefaultAckDelayUS,
		InitialMaxData:       DefaultInitialMaxData,
		InitialStreamWindow:  DefaultStreamWindow,
		ReceivePacketHistory: MinReplayWindow,
		InitialBidiSlots:     DefaultInitialBidiSlots,
		InitialUniSlots:      DefaultInitialUniSlots,
		MaximumBidiSlots:     MaxStreamSlots,
		MaximumUniSlots:      MaxStreamSlots,
		ActiveCIDLimit:       2,
		MaxAckRanges:         MaxAckRanges,
		MaxDatagramData:      0,
		MaxCryptoMessage:     MaxRefreshMessage,
		MaxPendingControl:    MaxControlItems,
		MaxAckRatio:          DefaultMaxAckRatio,
		InitialAckRatio:      DefaultAckRatio,
		MaxParkDurationMS:    DefaultParkDurationMS,
		MaxPathValidationMS:  DefaultPathValidationMS,
		CandidatePathLimit:   1,
		RetainedOldPathLimit: 1,
	}
}

func isPowerOfTwo(v uint32) bool { return v != 0 && v&(v-1) == 0 }

func validAckRatio(v uint16) bool {
	switch v {
	case 2, 4, 8, 16, 32:
		return true
	}
	return false
}

// Validate checks every parameter bound. Violations are
// TRANSPORT_PARAMETER_ERROR at the caller.
func (p *Params) Validate() bool {
	if p.FeatureFlags&^(FeatureDatagram|FeatureEarly|FeatureECN|FeatureMigration|FeaturePark) != 0 {
		return false
	}
	if p.MaxUDPPayload < BaseUDPPayload || p.MaxUDPPayload > MaxUDPPayload || p.MaxUDPPayload%8 != 0 {
		return false
	}
	if p.MaxFrames != MaxFrames {
		return false
	}
	if p.IdleTimeoutMS < 1000 || p.IdleTimeoutMS > 600000 {
		return false
	}
	if p.MaxAckDelayUS > MaxAckDelayUS {
		return false
	}
	if p.InitialMaxData > MaxDataValue {
		return false
	}
	if !isPowerOfTwo(p.InitialStreamWindow) || p.InitialStreamWindow < MinStreamWindow || p.InitialStreamWindow > MaxStreamWindow {
		return false
	}
	if !isPowerOfTwo(p.ReceivePacketHistory) || p.ReceivePacketHistory < MinReplayWindow || p.ReceivePacketHistory > MaxReplayWindow {
		return false
	}
	if p.InitialBidiSlots < 1 || p.InitialBidiSlots > MaxStreamSlots {
		return false
	}
	if p.InitialUniSlots > MaxStreamSlots {
		return false
	}
	if p.MaximumBidiSlots < p.InitialBidiSlots || p.MaximumBidiSlots > MaxStreamSlots {
		return false
	}
	if p.MaximumUniSlots < p.InitialUniSlots || p.MaximumUniSlots > MaxStreamSlots {
		return false
	}
	if p.ActiveCIDLimit < 2 || p.ActiveCIDLimit > MaxActiveCIDs {
		return false
	}
	if p.MaxAckRanges != MaxAckRanges {
		return false
	}
	if p.MaxDatagramData > 4032 {
		return false
	}
	if p.MaxCryptoMessage != MaxRefreshMessage {
		return false
	}
	if p.MaxPendingControl != MaxControlItems {
		return false
	}
	if !validAckRatio(p.MaxAckRatio) || !validAckRatio(p.InitialAckRatio) || p.InitialAckRatio > p.MaxAckRatio {
		return false
	}
	// PARK flag and lease limit must be consistent.
	parkFlag := p.FeatureFlags&FeaturePark != 0
	if p.MaxParkDurationMS == 0 {
		if parkFlag {
			return false
		}
	} else {
		if !parkFlag || p.MaxParkDurationMS < 1000 || p.MaxParkDurationMS > MaxParkDurationMS {
			return false
		}
	}
	if p.MaxPathValidationMS < 1000 || p.MaxPathValidationMS > MaxPathValidationMS {
		return false
	}
	if p.CandidatePathLimit != 1 || p.RetainedOldPathLimit != 1 {
		return false
	}
	return true
}

// EncodeParams serializes p into out[0:128].
func EncodeParams(out []byte, p *Params) {
	binary.BigEndian.PutUint16(out[0:], ParamVersion)
	binary.BigEndian.PutUint16(out[2:], p.FeatureFlags)
	binary.BigEndian.PutUint16(out[4:], p.MaxUDPPayload)
	binary.BigEndian.PutUint16(out[6:], p.MaxFrames)
	binary.BigEndian.PutUint32(out[8:], p.IdleTimeoutMS)
	binary.BigEndian.PutUint32(out[12:], p.MaxAckDelayUS)
	binary.BigEndian.PutUint64(out[16:], p.InitialMaxData)
	binary.BigEndian.PutUint32(out[24:], p.InitialStreamWindow)
	binary.BigEndian.PutUint32(out[28:], p.ReceivePacketHistory)
	binary.BigEndian.PutUint16(out[32:], p.InitialBidiSlots)
	binary.BigEndian.PutUint16(out[34:], p.InitialUniSlots)
	binary.BigEndian.PutUint16(out[36:], p.MaximumBidiSlots)
	binary.BigEndian.PutUint16(out[38:], p.MaximumUniSlots)
	binary.BigEndian.PutUint16(out[40:], p.ActiveCIDLimit)
	binary.BigEndian.PutUint16(out[42:], p.MaxAckRanges)
	binary.BigEndian.PutUint32(out[44:], p.MaxDatagramData)
	binary.BigEndian.PutUint32(out[48:], p.MaxCryptoMessage)
	binary.BigEndian.PutUint32(out[52:], p.MaxPendingControl)
	binary.BigEndian.PutUint16(out[56:], p.MaxAckRatio)
	binary.BigEndian.PutUint16(out[58:], p.InitialAckRatio)
	binary.BigEndian.PutUint32(out[60:], p.MaxParkDurationMS)
	binary.BigEndian.PutUint32(out[64:], p.MaxPathValidationMS)
	binary.BigEndian.PutUint16(out[68:], p.CandidatePathLimit)
	binary.BigEndian.PutUint16(out[70:], p.RetainedOldPathLimit)
	clear(out[72:128])
}

// DecodeParams parses and validates the 128-byte structure.
func DecodeParams(b []byte) (Params, bool) {
	var p Params
	if len(b) < ParamsBytes {
		return p, false
	}
	if binary.BigEndian.Uint16(b[0:]) != ParamVersion {
		return p, false
	}
	p.FeatureFlags = binary.BigEndian.Uint16(b[2:])
	p.MaxUDPPayload = binary.BigEndian.Uint16(b[4:])
	p.MaxFrames = binary.BigEndian.Uint16(b[6:])
	p.IdleTimeoutMS = binary.BigEndian.Uint32(b[8:])
	p.MaxAckDelayUS = binary.BigEndian.Uint32(b[12:])
	p.InitialMaxData = binary.BigEndian.Uint64(b[16:])
	p.InitialStreamWindow = binary.BigEndian.Uint32(b[24:])
	p.ReceivePacketHistory = binary.BigEndian.Uint32(b[28:])
	p.InitialBidiSlots = binary.BigEndian.Uint16(b[32:])
	p.InitialUniSlots = binary.BigEndian.Uint16(b[34:])
	p.MaximumBidiSlots = binary.BigEndian.Uint16(b[36:])
	p.MaximumUniSlots = binary.BigEndian.Uint16(b[38:])
	p.ActiveCIDLimit = binary.BigEndian.Uint16(b[40:])
	p.MaxAckRanges = binary.BigEndian.Uint16(b[42:])
	p.MaxDatagramData = binary.BigEndian.Uint32(b[44:])
	p.MaxCryptoMessage = binary.BigEndian.Uint32(b[48:])
	p.MaxPendingControl = binary.BigEndian.Uint32(b[52:])
	p.MaxAckRatio = binary.BigEndian.Uint16(b[56:])
	p.InitialAckRatio = binary.BigEndian.Uint16(b[58:])
	p.MaxParkDurationMS = binary.BigEndian.Uint32(b[60:])
	p.MaxPathValidationMS = binary.BigEndian.Uint32(b[64:])
	p.CandidatePathLimit = binary.BigEndian.Uint16(b[68:])
	p.RetainedOldPathLimit = binary.BigEndian.Uint16(b[70:])
	for i := 72; i < ParamsBytes; i++ {
		if b[i] != 0 {
			return p, false
		}
	}
	if !p.Validate() {
		return p, false
	}
	return p, true
}
