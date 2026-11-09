package neosepia

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestIntegerColumnsPreserveFullSignedRange(t *testing.T) {
	cases := []struct {
		name   string
		values []int64
	}{
		{"extremes", []int64{math.MinInt64, math.MaxInt64, 0, -1}},
		{"near minimum", []int64{math.MinInt64, math.MinInt64 + 1, math.MinInt64 + 2, math.MinInt64 + 3, math.MinInt64 + 4, math.MinInt64 + 5}},
		{"runs", []int64{7, 7, 7, 7, 7, 7, 7, 7, 7, 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, kind := encodeIntegers(nil, tc.values)
			decoded := make([]int64, len(tc.values))
			if err := decodeIntegers(encoded, kind, decoded); err != nil {
				t.Fatal(err)
			}
			for i, want := range tc.values {
				if decoded[i] != want {
					t.Fatalf("lane %d = %d, want %d", i, decoded[i], want)
				}
			}
		})
	}
}

func TestRLEColumnRejectsIncompleteAndExcessRuns(t *testing.T) {
	encoded := binary.LittleEndian.AppendUint32(nil, 1)
	encoded = binary.LittleEndian.AppendUint32(encoded, 2)
	encoded = binary.LittleEndian.AppendUint64(encoded, 17)
	for _, count := range []int{1, 3} {
		if err := decodeIntegers(encoded, encodingRLE, make([]int64, count)); !errors.Is(err, errColumnEncoding) {
			t.Fatalf("two-value run accepted for %d lanes: %v", count, err)
		}
	}
}

func TestDeltaColumnRejectsSignedOverflow(t *testing.T) {
	encoded := binary.LittleEndian.AppendUint64(nil, math.MaxInt64)
	encoded = append(encoded, 1)
	encoded = binary.LittleEndian.AppendUint32(encoded, 1)
	encoded = append(encoded, 1)
	if err := decodeIntegers(encoded, encodingDelta, make([]int64, 1)); !errors.Is(err, errColumnEncoding) {
		t.Fatalf("MaxInt64 plus one: %v", err)
	}
}
