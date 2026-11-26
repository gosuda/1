package wire

import (
	"testing"
)

func TestPresetParamsValid(t *testing.T) {
	presets := []struct {
		preset Preset
		name   string
	}{
		{PresetLowResource, "LowResource"},
		{PresetLowLatency, "LowLatency"},
		{PresetStandard1RTT, "Standard1RTT"},
		{PresetHighThroughput, "HighThroughput"},
	}

	for _, tc := range presets {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.preset.Params()
			if !p.Validate() {
				t.Fatalf("preset %s params failed Validate()", tc.name)
			}
			if tc.preset.String() != tc.name {
				t.Fatalf("preset String() = %q, want %q", tc.preset.String(), tc.name)
			}

			// Test encode/decode round trip
			buf := make([]byte, 128)
			EncodeParams(buf, &p)
			dec, ok := DecodeParams(buf)
			if !ok {
				t.Fatalf("preset %s DecodeParams failed", tc.name)
			}
			if !dec.Validate() {
				t.Fatalf("preset %s decoded params failed Validate()", tc.name)
			}
			if dec != p {
				t.Fatalf("preset %s decoded != original", tc.name)
			}
		})
	}
}

func TestPresetProperties(t *testing.T) {
	lowRes := LowResourceParams()
	if lowRes.InitialMaxData != 0 {
		t.Fatalf("LowResource should have 0 initial credit, got %d", lowRes.InitialMaxData)
	}

	lowLat := LowLatencyParams()
	if lowLat.InitialMaxData == 0 {
		t.Fatalf("LowLatency should have positive initial credit, got %d", lowLat.InitialMaxData)
	}
	if lowLat.FeatureFlags&FeatureEarly == 0 {
		t.Fatal("LowLatency should have FeatureEarly enabled")
	}

	std1RTT := Standard1RTTParams()
	if std1RTT.FeatureFlags&FeatureEarly != 0 {
		t.Fatal("Standard1RTT must have FeatureEarly disabled for strict 1-RTT PFS")
	}
	if std1RTT.InitialMaxData == 0 {
		t.Fatal("Standard1RTT should have positive initial credit for fast streaming")
	}

	highTP := HighThroughputParams()
	if highTP.MaxUDPPayload != 4096 {
		t.Fatalf("HighThroughput should have 4096 MaxUDPPayload, got %d", highTP.MaxUDPPayload)
	}
	if highTP.InitialMaxData < 16*1024*1024 {
		t.Fatalf("HighThroughput should have >= 16MB initial credit, got %d", highTP.InitialMaxData)
	}
}
