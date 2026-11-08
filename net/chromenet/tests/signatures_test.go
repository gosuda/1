package tests

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"reflect"
	"testing"

	"gosuda.org/chromenet/client"
)

func TestSignatures_CipherSuiteOrder(t *testing.T) {
	expectedSuites := []uint16{
		client.TLS_AES_128_GCM_SHA256,
		client.TLS_AES_256_GCM_SHA384,
		client.TLS_CHACHA20_POLY1305_SHA256,
		client.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		client.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		client.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		client.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		client.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		client.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		client.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		client.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		client.TLS_RSA_WITH_AES_128_GCM_SHA256,
		client.TLS_RSA_WITH_AES_256_GCM_SHA384,
		client.TLS_RSA_WITH_AES_128_CBC_SHA,
		client.TLS_RSA_WITH_AES_256_CBC_SHA,
	}

	if len(client.GoldenChromeCipherSuites) != 15 {
		t.Fatalf("expected 15 golden cipher suites, got %d", len(client.GoldenChromeCipherSuites))
	}

	for i, suite := range expectedSuites {
		if client.GoldenChromeCipherSuites[i] != suite {
			t.Errorf("cipher suite mismatch at index %d: got 0x%04x, want 0x%04x", i, client.GoldenChromeCipherSuites[i], suite)
		}
	}
}

func TestSignatures_GREASEValidation(t *testing.T) {
	for _, grease := range client.GreaseValues {
		if !client.IsGREASE(grease) {
			t.Errorf("expected 0x%04x to be recognized as GREASE", grease)
		}
	}

	nonGrease := []uint16{0x1301, 0x1302, 0xc02b, 0x009c, 0x0a00, 0x0b0b, 0x1234}
	for _, val := range nonGrease {
		if client.IsGREASE(val) {
			t.Errorf("expected 0x%04x NOT to be recognized as GREASE", val)
		}
	}
}

func TestSignatures_PostQuantumGroups(t *testing.T) {
	if client.GroupX25519MLKEM768 != 0x6399 {
		t.Errorf("GroupX25519MLKEM768 = 0x%04x; want 0x6399", client.GroupX25519MLKEM768)
	}
	if client.GroupX25519KyberDraft != 0x11ec {
		t.Errorf("GroupX25519KyberDraft = 0x%04x; want 0x11ec", client.GroupX25519KyberDraft)
	}
	if client.GroupX25519 != 0x001d {
		t.Errorf("GroupX25519 = 0x%04x; want 0x001d", client.GroupX25519)
	}
	if client.GroupSecp256r1 != 0x0017 {
		t.Errorf("GroupSecp256r1 = 0x%04x; want 0x0017", client.GroupSecp256r1)
	}
	if client.GroupSecp384r1 != 0x0018 {
		t.Errorf("GroupSecp384r1 = 0x%04x; want 0x0018", client.GroupSecp384r1)
	}

	if len(client.GoldenSupportedGroups) == 0 || client.GoldenSupportedGroups[0] != client.GroupX25519MLKEM768 {
		t.Errorf("first supported group must be GroupX25519MLKEM768 (0x6399), got: %v", client.GoldenSupportedGroups)
	}
}

func TestSignatures_JA3AndJA4(t *testing.T) {
	if client.GoldenChromeJA3String == "" {
		t.Fatal("GoldenChromeJA3String is empty")
	}

	expectedMD5 := md5.Sum([]byte(client.GoldenChromeJA3String))
	expectedHash := hex.EncodeToString(expectedMD5[:])

	if got := client.GoldenChromeJA3Hash(); got != expectedHash {
		t.Errorf("GoldenChromeJA3Hash() = %s; want %s", got, expectedHash)
	}

	if client.GoldenChromeJA4Prefix != "t13d" {
		t.Errorf("GoldenChromeJA4Prefix = %s; want t13d", client.GoldenChromeJA4Prefix)
	}
}

func TestSignatures_H2SettingsAndWindowUpdate(t *testing.T) {
	if client.ChromeH2SettingHeaderTableSize != 65536 {
		t.Errorf("ChromeH2SettingHeaderTableSize = %d; want 65536", client.ChromeH2SettingHeaderTableSize)
	}
	if client.ChromeH2SettingEnablePush != 0 {
		t.Errorf("ChromeH2SettingEnablePush = %d; want 0", client.ChromeH2SettingEnablePush)
	}
	if client.ChromeH2SettingInitialWindowSize != 6291456 {
		t.Errorf("ChromeH2SettingInitialWindowSize = %d; want 6291456", client.ChromeH2SettingInitialWindowSize)
	}
	if client.ChromeH2SettingMaxHeaderListSize != 262144 {
		t.Errorf("ChromeH2SettingMaxHeaderListSize = %d; want 262144", client.ChromeH2SettingMaxHeaderListSize)
	}
	if client.ChromeH2WindowUpdateIncrement != 15663105 {
		t.Errorf("ChromeH2WindowUpdateIncrement = %d; want 15663105", client.ChromeH2WindowUpdateIncrement)
	}
	if client.ChromeH2PriorityWeight != 256 {
		t.Errorf("ChromeH2PriorityWeight = %d; want 256", client.ChromeH2PriorityWeight)
	}

	settings := client.GoldenChromeH2Settings
	if settings[client.H2SettingsHeaderTableSize] != 65536 {
		t.Errorf("settings table size mismatch: %d", settings[client.H2SettingsHeaderTableSize])
	}
	if settings[client.H2SettingsEnablePush] != 0 {
		t.Errorf("settings enable push mismatch: %d", settings[client.H2SettingsEnablePush])
	}
	if settings[client.H2SettingsInitialWindowSize] != 6291456 {
		t.Errorf("settings initial window size mismatch: %d", settings[client.H2SettingsInitialWindowSize])
	}
	if settings[client.H2SettingsMaxHeaderListSize] != 262144 {
		t.Errorf("settings max header list size mismatch: %d", settings[client.H2SettingsMaxHeaderListSize])
	}
}

func TestSignatures_PseudoHeaderOrdering(t *testing.T) {
	wantH2 := []string{":method", ":authority", ":scheme", ":path"}
	if !reflect.DeepEqual(client.GoldenH2PseudoHeaderOrder, wantH2) {
		t.Errorf("GoldenH2PseudoHeaderOrder = %v; want %v", client.GoldenH2PseudoHeaderOrder, wantH2)
	}

	wantH3 := []string{":method", ":scheme", ":authority", ":path"}
	if !reflect.DeepEqual(client.GoldenH3PseudoHeaderOrder, wantH3) {
		t.Errorf("GoldenH3PseudoHeaderOrder = %v; want %v", client.GoldenH3PseudoHeaderOrder, wantH3)
	}
}

func TestSignatures_QUICTransportParams(t *testing.T) {
	p := client.GoldenChromeQUICTransportParams
	if p.MaxUDPPayloadSize != 1472 {
		t.Errorf("MaxUDPPayloadSize = %d; want 1472", p.MaxUDPPayloadSize)
	}
	if p.InitialMaxData != 15728640 {
		t.Errorf("InitialMaxData = %d; want 15728640", p.InitialMaxData)
	}
	if p.InitialMaxStreamDataBidiLocal != 6291456 {
		t.Errorf("InitialMaxStreamDataBidiLocal = %d; want 6291456", p.InitialMaxStreamDataBidiLocal)
	}
	if p.InitialMaxStreamDataBidiRemote != 6291456 {
		t.Errorf("InitialMaxStreamDataBidiRemote = %d; want 6291456", p.InitialMaxStreamDataBidiRemote)
	}
	if p.InitialMaxStreamDataUni != 6291456 {
		t.Errorf("InitialMaxStreamDataUni = %d; want 6291456", p.InitialMaxStreamDataUni)
	}
	if p.InitialMaxStreamsBidi != 100 {
		t.Errorf("InitialMaxStreamsBidi = %d; want 100", p.InitialMaxStreamsBidi)
	}
	if p.InitialMaxStreamsUni != 100 {
		t.Errorf("InitialMaxStreamsUni = %d; want 100", p.InitialMaxStreamsUni)
	}
	if client.QPACKMaxTableCapacity != 4096 {
		t.Errorf("QPACKMaxTableCapacity = %d; want 4096", client.QPACKMaxTableCapacity)
	}

	greaseIDs := []uint64{27, 58, 89, 120, 151}
	for _, id := range greaseIDs {
		if !client.IsQuicGREASE(id) {
			t.Errorf("expected %d to be recognized as QuicGREASE", id)
		}
	}

	nonGreaseIDs := []uint64{0, 1, 26, 28, 57, 59, 88, 90}
	for _, id := range nonGreaseIDs {
		if client.IsQuicGREASE(id) {
			t.Errorf("expected %d NOT to be recognized as QuicGREASE", id)
		}
	}
}

func TestSignatures_DefaultHeadersInjection(t *testing.T) {
	h := make(http.Header)
	client.ApplyChromeHeaders(h, "")

	if h.Get("User-Agent") != client.DefaultUserAgent {
		t.Errorf("User-Agent = %q; want %q", h.Get("User-Agent"), client.DefaultUserAgent)
	}
	if h.Get("Sec-Ch-Ua") != client.DefaultSecChUa {
		t.Errorf("Sec-Ch-Ua = %q; want %q", h.Get("Sec-Ch-Ua"), client.DefaultSecChUa)
	}
	if h.Get("Sec-Ch-Ua-Mobile") != client.DefaultSecChUaMobile {
		t.Errorf("Sec-Ch-Ua-Mobile = %q; want %q", h.Get("Sec-Ch-Ua-Mobile"), client.DefaultSecChUaMobile)
	}
	if h.Get("Sec-Ch-Ua-Platform") != client.DefaultSecChUaPlatform {
		t.Errorf("Sec-Ch-Ua-Platform = %q; want %q", h.Get("Sec-Ch-Ua-Platform"), client.DefaultSecChUaPlatform)
	}
	if h.Get("Upgrade-Insecure-Requests") != client.DefaultUpgradeInsecureRequests {
		t.Errorf("Upgrade-Insecure-Requests = %q; want %q", h.Get("Upgrade-Insecure-Requests"), client.DefaultUpgradeInsecureRequests)
	}
	if h.Get("Accept") != client.DefaultAccept {
		t.Errorf("Accept = %q; want %q", h.Get("Accept"), client.DefaultAccept)
	}
	if h.Get("Accept-Encoding") != client.DefaultAcceptEncoding {
		t.Errorf("Accept-Encoding = %q; want %q", h.Get("Accept-Encoding"), client.DefaultAcceptEncoding)
	}
	if h.Get("Accept-Language") != client.DefaultAcceptLanguage {
		t.Errorf("Accept-Language = %q; want %q", h.Get("Accept-Language"), client.DefaultAcceptLanguage)
	}

	// Verify preservation of custom headers
	customH := make(http.Header)
	customH.Set("User-Agent", "CustomAgent/2.0")
	customH.Set("Accept-Encoding", "gzip")
	customH.Set("X-Custom-Header", "preserved-value")

	client.ApplyChromeHeaders(customH, "IgnoredAgent")

	if customH.Get("User-Agent") != "CustomAgent/2.0" {
		t.Errorf("existing User-Agent was overwritten: got %q", customH.Get("User-Agent"))
	}
	if customH.Get("Accept-Encoding") != "gzip" {
		t.Errorf("existing Accept-Encoding was overwritten: got %q", customH.Get("Accept-Encoding"))
	}
	if customH.Get("X-Custom-Header") != "preserved-value" {
		t.Errorf("custom header was modified: got %q", customH.Get("X-Custom-Header"))
	}
}
