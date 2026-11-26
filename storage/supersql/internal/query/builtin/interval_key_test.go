package builtin_test

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestIntervalKeysCanonicalizeEquivalentDurations(t *testing.T) {
	encoder := keyEncoder(t)
	groups := [][]model.Interval{
		{{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}},
		{{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64 + 1}},
		{{Months: -1}, {Days: -30}, {Microseconds: -2592000000000}},
		{{}, {Months: 1, Days: -30}, {Days: 1, Microseconds: -86400000000}},
		{{Months: 1}, {Days: 30}, {Microseconds: 2592000000000}},
		{{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64 - 1}},
		{{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}},
	}
	for _, descending := range []bool{false, true} {
		layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 1186, Storage: model.TypeInterval, Modifier: -1}, Descending: descending}}
		var prior []byte
		for _, group := range groups {
			var equal []byte
			for _, interval := range group {
				key, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{{Type: model.TypeInterval, IntervalVal: interval}}, 17)
				if err != nil {
					t.Fatal(err)
				}
				if equal != nil && !bytes.Equal(equal, key) {
					t.Fatalf("unequal keys for equivalent durations: %v", group)
				}
				equal = key
				decoded, err := encoder.DecodeKey(t.Context(), layout, key, 4096)
				if err != nil {
					t.Fatal(err)
				}
				roundtrip, err := encoder.EncodePrefix(t.Context(), layout, decoded, 17)
				if err != nil || !bytes.Equal(key, roundtrip) {
					t.Fatalf("interval roundtrip %v: %x %x %v", interval, key, roundtrip, err)
				}
			}
			order := bytes.Compare(prior, equal)
			if descending {
				order = -order
			}
			if prior != nil && order >= 0 {
				t.Fatalf("interval ordering at %v", group)
			}
			prior = equal
		}
		_, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{{Type: model.TypeInterval}}, 16)
		if !errors.Is(err, columnar.ErrBatchBudget) {
			t.Fatalf("interval key admission: %v", err)
		}
		invalid := append([]byte{1}, bytes.Repeat([]byte{255}, 16)...)
		if _, err := encoder.DecodeKey(t.Context(), layout, invalid, 4096); !errors.Is(err, storage.ErrInvalidEncoding) {
			t.Fatalf("out of domain interval key: %v", err)
		}
	}
}
