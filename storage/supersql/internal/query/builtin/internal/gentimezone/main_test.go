package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func tzifBlock(version byte, width int, transitions []int64, indices []byte) []byte {
	raw := make([]byte, 44)
	copy(raw, "TZif")
	raw[4] = version
	binary.BigEndian.PutUint32(raw[32:], uint32(len(transitions)))
	binary.BigEndian.PutUint32(raw[36:], 2)
	binary.BigEndian.PutUint32(raw[40:], 8)
	for _, at := range transitions {
		if width == 4 {
			raw = binary.BigEndian.AppendUint32(raw, uint32(at))
		} else {
			raw = binary.BigEndian.AppendUint64(raw, uint64(at))
		}
	}
	raw = append(raw, indices...)
	raw = binary.BigEndian.AppendUint32(raw, 0)
	raw = append(raw, 0, 0)
	raw = binary.BigEndian.AppendUint32(raw, 3600)
	raw = append(raw, 1, 4)
	return append(raw, "STD\x00DST\x00"...)
}

func TestSelect64BitHistoryAndFuture(t *testing.T) {
	raw := tzifBlock('2', 4, []int64{0}, []byte{1})
	raw = append(raw, tzifBlock('2', 8, []int64{-5000000000, 5000000000}, []byte{1, 0})...)
	raw = append(raw, "\nSTD0DST,M3.2.0,M11.1.0\n"...)
	z, err := parseTZif(raw)
	if err != nil {
		t.Fatal(err)
	}
	if z.timeSize != 8 || z.count != 2 {
		t.Fatalf("wrong timestamp block: %+v", z)
	}
	if at := int64(binary.BigEndian.Uint64(raw[z.times:])); at != -5000000000 {
		t.Fatalf("decoded transition: %d", at)
	}
	if z.defaultType != 0 || !z.hasFuture || z.future.DaylightOffset != 3600 {
		t.Fatalf("decoded zone: %+v", z)
	}
}

func TestRejectCorruptTZif(t *testing.T) {
	valid := tzifBlock(0, 4, []int64{0, 100}, []byte{1, 0})
	cases := map[string]func([]byte) []byte{
		"truncated":             func(b []byte) []byte { return b[:len(b)-1] },
		"bad index":             func(b []byte) []byte { b[52] = 2; return b },
		"duplicate transitions": func(b []byte) []byte { copy(b[48:52], b[44:48]); return b },
		"missing terminator":    func(b []byte) []byte { b[len(b)-1] = 'X'; return b },
		"leap dependent":        func(b []byte) []byte { binary.BigEndian.PutUint32(b[28:], 1); return b },
		"oversized counts":      func(b []byte) []byte { binary.BigEndian.PutUint32(b[32:], 0xffffffff); return b },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTZif(corrupt(bytes.Clone(valid))); err == nil {
				t.Fatal("accepted corrupt TZif")
			}
		})
	}
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSetOverrideAndDynamicResolution(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "Default", "CST -21600\nIST Asia/Kolkata\nD 3600 D\n")
	writeFixture(t, root, "Australia", "@INCLUDE Default\n@OVERRIDE\nCST 34200\n")
	g := generator{digest: sha256.New(), zoneIndex: map[string]int{"asia/kolkata": 7}}
	entries := make(map[string]abbreviation)
	if err := g.loadSet(root, "Australia", 0, entries); err != nil {
		t.Fatal(err)
	}
	if entries["cst"].offset != 34200 || entries["ist"].zone != 7 || !entries["d"].dst {
		t.Fatalf("entries: %+v", entries)
	}
}

func TestSetRejectsConflictsAndIncludeCycles(t *testing.T) {
	for name, content := range map[string]string{"conflict": "CST 0\ncst 3600\n", "cycle": "@INCLUDE Default\n", "missing zone": "IST Missing/Zone\n", "offset range": "CST 50401\n"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "Default", content)
			g := generator{digest: sha256.New()}
			if err := g.loadSet(root, "Default", 0, make(map[string]abbreviation)); err == nil {
				t.Fatal("accepted invalid set")
			}
		})
	}
}

func TestGenerationDeterminismAndInputDigest(t *testing.T) {
	root := t.TempDir()
	zones, sets := filepath.Join(root, "zones"), filepath.Join(root, "sets")
	for _, dir := range []string{zones, sets} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, zones, "UTC", string(tzifBlock(0, 4, nil, nil)))
	writeFixture(t, sets, "Default", "UTC 0\n")
	writeFixture(t, root, "tzdata.zi", "# version 2026b\nZ UTC 0 - UTC\n")
	output := filepath.Join(root, "generated.go")
	run := func() []byte {
		t.Helper()
		if err := generate(zones, sets, filepath.Join(root, "tzdata.zi"), output); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := run()
	if !bytes.Equal(first, run()) {
		t.Fatal("identical inputs generated different output")
	}
	writeFixture(t, sets, "Default", "# provenance changed\nUTC 0\n")
	if bytes.Equal(first, run()) {
		t.Fatal("changed source bytes did not change the digest")
	}
	writeFixture(t, root, "tzdata.zi", "# version 2026b\nZ Missing 0 - UTC\n")
	if err := generate(zones, sets, filepath.Join(root, "tzdata.zi"), output); err == nil {
		t.Fatal("accepted incomplete compiled inventory")
	}
	writeFixture(t, root, "tzdata.zi", "# version 2026b\nZ UTC 0 - UTC\n")
	writeFixture(t, zones, "StaleAlias", string(tzifBlock(0, 4, nil, nil)))
	if err := generate(zones, sets, filepath.Join(root, "tzdata.zi"), output); err == nil {
		t.Fatal("accepted a compiled zone absent from the pinned source")
	}
}
