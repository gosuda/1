// Package wire implements the fixed wire encodings of GRT/1.1 (spec section 3,
// 5, 6, 10, 11). All wire integers are unsigned big-endian.
package wire

// Protocol constants, spec section 3.
const (
	Magic   uint16 = 0x4752
	Version uint8  = 0x11

	KexProfile uint16 = 1

	CipherXChaCha20Poly1305 uint16 = 0x0001
	CipherAES256GCM         uint16 = 0x0002

	MaxCipherOffers   = 2
	HeaderBytes       = 32
	TagBytes          = 16
	MinDataPlaintext  = 16 // minimum decrypted DATA plaintext (section 8)
	BaseUDPPayload    = 1200
	MaxUDPPayload     = 4096
	FrameHeaderBytes  = 8
	MaxFrames         = 32
	MaxAckRanges      = 8
	BootstrapChunk    = 1024
	MaxFlightBytes    = 16384
	MaxFlightChunks   = 16
	MaxEarlyBytes     = 4096
	MaxCredentials    = 4
	CredentialBytes   = 196
	CredentialPrefix  = 132
	MaxRefreshMessage = 4096
	MaxControlItems   = 64
	MaxActiveCIDs     = 4
	MaxPaths          = 3
	MinReplayWindow   = 4096
	MaxReplayWindow   = 1048576
	MaxStreamSlots    = 1024
	MinStreamWindow   = 4096
	MaxStreamWindow   = 16777216

	DefaultStreamWindow     = 4096
	DefaultInitialMaxData   = 0
	DefaultInitialBidiSlots = 1
	DefaultInitialUniSlots  = 0

	DefaultIdleMS                  = 30000
	DefaultAckDelayUS              = 1000
	MaxAckDelayUS                  = 10000
	TimerGranularityUS             = 50
	InitialRTTUS                   = 333000
	MinPTOUS                       = 1000
	HandshakeDeadlineUS            = 10000000
	RekeySoftPacketsXChaCha uint64 = 1 << 23
	RekeyHardPacketsXChaCha uint64 = 1 << 24
	RekeySoftPacketsAESGCM  uint64 = 1 << 22
	RekeyHardPacketsAESGCM  uint64 = 1 << 23
	RekeySoftBytes          uint64 = 1 << 33
	RekeyHardBytes          uint64 = 1 << 34
	RekeySoftTimeUS                = 120000000
	InvalidAuthLimit        uint64 = 1 << 32

	DefaultAckRatio    = 2
	DefaultMaxAckRatio = 32

	MaxLocalBatchDatagrams       = 64
	SocketGSOMaxBytes            = 65507
	MaxPendingAckTransactions    = 8
	RetryUDPBytes                = 160
	RetryTokenLifetimeS          = 30
	ReuseTokenLifetimeS          = 600
	AmpFactor                    = 3
	DefaultPathValidationMS      = 10000
	MaxPathValidationMS          = 60000
	MaxChallengeTransmissions    = 3
	MaxParkDurationMS            = 1800000
	DefaultParkDurationMS        = 600000
	CompactCoreBytes             = 2048
	CompactQuiescentChargedBytes = 3072
	CompactTXHistoryEntries      = 256
	CompactHandshakeSlotBytes    = 65536
	RefreshControlReserveSeals   = 128
	RefreshControlReserveBytes   = 524288
)

// Packet classes, spec section 5.
const (
	ClassClientFlight uint8 = 0x00
	ClassServerFlight uint8 = 0x01
	ClassRetry        uint8 = 0x02
	ClassData         uint8 = 0x10
)

// Bootstrap fragment layout, spec section 6.1.
const (
	FragHeaderBytes = 120 // attempt_id:16 || hash:32 || total:4 || idx:2 || cnt:2 || token:64
	FlightPayload   = 1200
	RetryPayload    = 128 // RETRY body
)

// Address-token layout, spec section 6.2.
const (
	TokenBytes             = 64
	TokenKindRetry   uint8 = 0
	TokenKindReuse   uint8 = 1
	CanonicalEPBytes       = 19 // family:1 || addr:16 || port:2
)

// Transport parameter defaults and bounds, spec section 10.
const (
	ParamVersion uint16 = 0x0101
	ParamsBytes         = 128

	FeatureDatagram  uint16 = 1
	FeatureEarly     uint16 = 2
	FeatureECN       uint16 = 4
	FeatureMigration uint16 = 8
	FeaturePark      uint16 = 16
)

// ECN codepoints, RFC 3168.
const (
	ECNNotECT uint8 = 0
	ECNECT1   uint8 = 1
	ECNECT0   uint8 = 2
	ECNCE     uint8 = 3
)

// Frame types, spec section 11.
const (
	FramePadding        uint16 = 0x0000
	FramePing           uint16 = 0x0001
	FrameAck            uint16 = 0x0002
	FrameFinish         uint16 = 0x0003
	FrameHandshakeDone  uint16 = 0x0004
	FrameStreamOpen     uint16 = 0x0010
	FrameStream         uint16 = 0x0011
	FrameResetStream    uint16 = 0x0012
	FrameStopSending    uint16 = 0x0013
	FrameMaxData        uint16 = 0x0014
	FrameMaxStreamData  uint16 = 0x0015
	FrameMaxSlots       uint16 = 0x0016
	FrameDataBlocked    uint16 = 0x0017
	FrameStreamBlocked  uint16 = 0x0018
	FrameSlotsBlocked   uint16 = 0x0019
	FrameStreamRetire   uint16 = 0x001A
	FrameRetireAck      uint16 = 0x001B
	FrameDatagram       uint16 = 0x0020
	FramePathChallenge  uint16 = 0x0021
	FramePathResponse   uint16 = 0x0022
	FrameNewCID         uint16 = 0x0023
	FrameRetireCID      uint16 = 0x0024
	FrameNewTicket      uint16 = 0x0025
	FrameClose          uint16 = 0x0026
	FrameNewToken       uint16 = 0x0027
	FramePathIntent     uint16 = 0x0028
	FramePathSelected   uint16 = 0x0029
	FrameCryptoFragment uint16 = 0x0030
	FrameKeyCommit      uint16 = 0x0031
	FrameKeyConfirm     uint16 = 0x0032
	FrameKeyRequest     uint16 = 0x0033
	FrameMTUProbe       uint16 = 0x0034
	FrameParkRequest    uint16 = 0x0040
	FrameParkGrant      uint16 = 0x0041
	FrameParkWake       uint16 = 0x0042
	FrameParkAwake      uint16 = 0x0043
)

// FrameFlagStreamFin is STREAM flag bit 0.
const FrameFlagStreamFin uint16 = 1

// Error codes, spec section 24.
const (
	ErrNoError            uint32 = 0
	ErrInternal           uint32 = 1
	ErrProtocolViolation  uint32 = 2
	ErrFrameEncoding      uint32 = 3
	ErrFlowControl        uint32 = 4
	ErrStreamState        uint32 = 5
	ErrFinalSize          uint32 = 6
	ErrStreamData         uint32 = 7
	ErrAckRange           uint32 = 8
	ErrConnectionID       uint32 = 9
	ErrKeyUpdate          uint32 = 10
	ErrAEADLimit          uint32 = 11
	ErrCryptoBuffer       uint32 = 12
	ErrTransportParameter uint32 = 13
	ErrApplicationAbort   uint32 = 14
	ErrLocalResource      uint32 = 15
	ErrPathState          uint32 = 16
	ErrMobileState        uint32 = 17
)

// MaxDataValue is the 2^62-1 bound on offsets and absolute flow limits.
const MaxDataValue uint64 = (1 << 62) - 1

// MaxPacketNumber is the 2^64-2 packet-number bound.
const MaxPacketNumber uint64 = (1 << 64) - 2

// MaxGeneration is the 2^46-1 generation bound.
const MaxGeneration uint64 = (1 << 46) - 1

// NonceLen returns the AEAD nonce length for a cipher suite, spec section 4.1.
func NonceLen(cs uint16) int {
	if cs == CipherAES256GCM {
		return 12
	}
	return 24
}
