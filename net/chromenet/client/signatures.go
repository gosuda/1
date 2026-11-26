package client

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
)

// Default browser headers reflecting Chrome 131.
const (
	DefaultUserAgent               = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	DefaultSecChUa                 = `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`
	DefaultSecChUaMobile           = "?0"
	DefaultSecChUaPlatform         = `"Linux"`
	DefaultUpgradeInsecureRequests = "1"
	DefaultAccept                  = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
	DefaultSecFetchSite            = "none"
	DefaultSecFetchMode            = "navigate"
	DefaultSecFetchUser            = "?1"
	DefaultSecFetchDest            = "document"
	DefaultAcceptEncoding          = "gzip, deflate, br, zstd"
	DefaultAcceptLanguage          = "en-US,en;q=0.9"
)

// TLS Cipher Suites (15 BoringSSL suites in authentic Chrome order).
const (
	TLS_AES_128_GCM_SHA256                 uint16 = 0x1301
	TLS_AES_256_GCM_SHA384                 uint16 = 0x1302
	TLS_CHACHA20_POLY1305_SHA256          uint16 = 0x1303
	TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 uint16 = 0xc02b
	TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256   uint16 = 0xc02f
	TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384 uint16 = 0xc02c
	TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384   uint16 = 0xc030
	TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305 uint16 = 0xcca9
	TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305   uint16 = 0xcca8
	TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA      uint16 = 0xc013
	TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA      uint16 = 0xc014
	TLS_RSA_WITH_AES_128_GCM_SHA256        uint16 = 0x009c
	TLS_RSA_WITH_AES_256_GCM_SHA384        uint16 = 0x009d
	TLS_RSA_WITH_AES_128_CBC_SHA           uint16 = 0x002f
	TLS_RSA_WITH_AES_256_CBC_SHA           uint16 = 0x0035

	// Key Exchange / Supported Groups
	GroupX25519MLKEM768   uint16 = 0x6399 // Post-Quantum ML-KEM-768 hybrid
	GroupX25519KyberDraft uint16 = 0x11ec // Kyber768 draft
	GroupX25519           uint16 = 0x001d // X25519
	GroupSecp256r1        uint16 = 0x0017 // NIST P-256
	GroupSecp384r1        uint16 = 0x0018 // NIST P-384
)

// GoldenChromeCipherSuites contains the 15 BoringSSL cipher suites in Chrome order.
var GoldenChromeCipherSuites = []uint16{
	TLS_AES_128_GCM_SHA256,
	TLS_AES_256_GCM_SHA384,
	TLS_CHACHA20_POLY1305_SHA256,
	TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
	TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
	TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	TLS_RSA_WITH_AES_128_GCM_SHA256,
	TLS_RSA_WITH_AES_256_GCM_SHA384,
	TLS_RSA_WITH_AES_128_CBC_SHA,
	TLS_RSA_WITH_AES_256_CBC_SHA,
}

// GoldenSupportedGroups contains Chrome's supported elliptic curves and post-quantum groups.
var GoldenSupportedGroups = []uint16{
	GroupX25519MLKEM768,
	GroupX25519,
	GroupSecp256r1,
	GroupSecp384r1,
}

// JA3 / JA4 Fingerprint specifications
const (
	GoldenChromeJA3String = "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-27-17513,25497-29-23-24,0"
	GoldenChromeJA4Prefix = "t13d"
)

// GoldenChromeJA3Hash returns the MD5 hash hex digest of GoldenChromeJA3String.
func GoldenChromeJA3Hash() string {
	h := md5.Sum([]byte(GoldenChromeJA3String))
	return hex.EncodeToString(h[:])
}

// HTTP/2 SETTINGS & Window Constants
const (
	ChromeH2WindowUpdateIncrement uint32 = 15663105 // Stream 0 WINDOW_UPDATE increment
	ChromeH2PriorityWeight        uint32 = 256

	H2SettingsHeaderTableSize   uint16 = 0x1
	H2SettingsEnablePush        uint16 = 0x2
	H2SettingsInitialWindowSize uint16 = 0x4
	H2SettingsMaxHeaderListSize uint16 = 0x6

	ChromeH2SettingHeaderTableSize   uint32 = 65536
	ChromeH2SettingEnablePush        uint32 = 0
	ChromeH2SettingInitialWindowSize uint32 = 6291456
	ChromeH2SettingMaxHeaderListSize uint32 = 262144
)

// GoldenChromeH2Settings maps HTTP/2 setting IDs to Chrome default values.
var GoldenChromeH2Settings = map[uint16]uint32{
	H2SettingsHeaderTableSize:   ChromeH2SettingHeaderTableSize,
	H2SettingsEnablePush:        ChromeH2SettingEnablePush,
	H2SettingsInitialWindowSize: ChromeH2SettingInitialWindowSize,
	H2SettingsMaxHeaderListSize: ChromeH2SettingMaxHeaderListSize,
}

// GoldenH2PseudoHeaderOrder defines the exact HTTP/2 pseudo-header order in Chrome.
var GoldenH2PseudoHeaderOrder = []string{
	":method",
	":authority",
	":scheme",
	":path",
}

// QUICTransportParameters specifies transport parameters for QUIC connections.
type QUICTransportParameters struct {
	InitialMaxData                 uint64
	InitialMaxStreamDataBidiLocal  uint64
	InitialMaxStreamDataBidiRemote uint64
	InitialMaxStreamDataUni        uint64
	InitialMaxStreamsBidi          uint64
	InitialMaxStreamsUni           uint64
	MaxIdleTimeoutMs               uint64
	ActiveConnectionIDLimit        uint64
	MaxUDPPayloadSize              uint64
	DisableActiveMigration         bool
}

// GoldenChromeQUICTransportParams reflects Chrome's authentic QUIC transport parameters.
var GoldenChromeQUICTransportParams = QUICTransportParameters{
	InitialMaxData:                 15728640,
	InitialMaxStreamDataBidiLocal:  6291456,
	InitialMaxStreamDataBidiRemote: 6291456,
	InitialMaxStreamDataUni:        6291456,
	InitialMaxStreamsBidi:          100,
	InitialMaxStreamsUni:           100,
	MaxIdleTimeoutMs:               30000,
	ActiveConnectionIDLimit:        4,
	MaxUDPPayloadSize:              1472,
	DisableActiveMigration:         false,
}

// QPACK constants for HTTP/3
const (
	QPACKMaxTableCapacity  uint64 = 4096
	QPACKMaxBlockedStreams uint64 = 100
)

// GoldenH3PseudoHeaderOrder defines the exact HTTP/3 pseudo-header order in Chrome.
var GoldenH3PseudoHeaderOrder = []string{
	":method",
	":scheme",
	":authority",
	":path",
}

// RFC 8701 GREASE values
var GreaseValues = []uint16{
	0x0a0a, 0x1a1a, 0x2a2a, 0x3a3a, 0x4a4a, 0x5a5a, 0x6a6a, 0x7a7a,
	0x8a8a, 0x9a9a, 0xaaaa, 0xbaba, 0xcaca, 0xdada, 0xeaea, 0xfafa,
}

// IsGREASE reports whether a 16-bit value matches RFC 8701 GREASE pattern 0x?a?a where both nibbles match.
func IsGREASE(val uint16) bool {
	return ((val & 0x0f0f) == 0x0a0a) && ((val >> 8) == (val & 0xff))
}

// IsQuicGREASE reports whether a QUIC transport parameter ID or frame type matches the GREASE formula (27 + 31*N).
func IsQuicGREASE(id uint64) bool {
	return (id >= 27) && ((id-27)%31 == 0)
}

// ApplyChromeHeaders injects Chrome browser defaults into h if not already set.
// It preserves existing caller overrides. If ua is non-empty, it is used instead of DefaultUserAgent.
func ApplyChromeHeaders(h http.Header, ua string) {
	if ua == "" {
		ua = DefaultUserAgent
	}
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", ua)
	}
	if h.Get("Sec-Ch-Ua") == "" {
		h.Set("Sec-Ch-Ua", DefaultSecChUa)
	}
	if h.Get("Sec-Ch-Ua-Mobile") == "" {
		h.Set("Sec-Ch-Ua-Mobile", DefaultSecChUaMobile)
	}
	if h.Get("Sec-Ch-Ua-Platform") == "" {
		h.Set("Sec-Ch-Ua-Platform", DefaultSecChUaPlatform)
	}
	if h.Get("Upgrade-Insecure-Requests") == "" {
		h.Set("Upgrade-Insecure-Requests", DefaultUpgradeInsecureRequests)
	}
	if h.Get("Accept") == "" {
		h.Set("Accept", DefaultAccept)
	}
	if h.Get("Sec-Fetch-Site") == "" {
		h.Set("Sec-Fetch-Site", DefaultSecFetchSite)
	}
	if h.Get("Sec-Fetch-Mode") == "" {
		h.Set("Sec-Fetch-Mode", DefaultSecFetchMode)
	}
	if h.Get("Sec-Fetch-User") == "" {
		h.Set("Sec-Fetch-User", DefaultSecFetchUser)
	}
	if h.Get("Sec-Fetch-Dest") == "" {
		h.Set("Sec-Fetch-Dest", DefaultSecFetchDest)
	}
	if h.Get("Accept-Encoding") == "" {
		h.Set("Accept-Encoding", DefaultAcceptEncoding)
	}
	if h.Get("Accept-Language") == "" {
		h.Set("Accept-Language", DefaultAcceptLanguage)
	}
}
