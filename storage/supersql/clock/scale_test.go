package clock

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"
)

type scaleEntry struct {
	second int64
	delta  int8
}

func scaleFixture(entries ...scaleEntry) ScaleDescriptor {
	data := make([]byte, 13+len(entries)*16)
	copy(data, "GSTL1")
	binary.BigEndian.PutUint32(data[5:9], 10)
	binary.BigEndian.PutUint32(data[9:13], uint32(len(entries)))
	for i, entry := range entries {
		binary.BigEndian.PutUint64(data[13+i*16:], uint64(entry.second))
		data[21+i*16] = byte(entry.delta)
	}
	return ScaleDescriptor{Name: ScaleName, LeapHistoryID: sha256.Sum256(data), CanonicalLeapHistory: data}
}

func TestScaleExtensionCannotChangeProtectedSIInstant(t *testing.T) {
	old := scaleFixture(scaleEntry{second: 1, delta: 1})
	for _, test := range []struct {
		name      string
		next      scaleEntry
		protected Instant
	}{
		{name: "positive_leap_start", next: scaleEntry{second: 3, delta: 1}, protected: 4_000_000_000},
		{name: "negative_leap_start", next: scaleEntry{second: 3, delta: -1}, protected: 3_000_000_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			next := scaleFixture(scaleEntry{second: 1, delta: 1}, test.next)
			if err := ValidateScaleExtension(t.Context(), old, next, test.protected-1, 4096); err != nil {
				t.Fatalf("future extension: %v", err)
			}
			if err := ValidateScaleExtension(t.Context(), old, next, test.protected, 4096); !errors.Is(err, ErrClockScale) {
				t.Fatalf("protected boundary changed: %v", err)
			}
		})
	}
}

func TestScaleExtensionRejectsRewrittenPastMapping(t *testing.T) {
	old := scaleFixture(scaleEntry{second: 1, delta: 1})
	rewritten := scaleFixture(scaleEntry{second: 1, delta: -1})
	if err := ValidateScaleExtension(t.Context(), old, rewritten, 10_000_000_000, 4096); !errors.Is(err, ErrClockScale) {
		t.Fatalf("past rewrite: %v", err)
	}
}
