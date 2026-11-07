package builtin_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestDateAndTimeKeysPreserveScalarOrder(t *testing.T) {
	encoder := keyEncoder(t)
	for _, tc := range []struct {
		id     model.TypeID
		values []int64
	}{
		{1082, []int64{math.MinInt32, math.MinInt32 + 1, -3000000, -2451547, -2451545, -1, 0, 2145031948, 2145031949, math.MaxInt32 - 1, math.MaxInt32}},
		{1083, []int64{0, 1, 43200000000, 86399999999, 86400000000}},
		{1114, []int64{math.MinInt64, -211813488000000000, -1, 0, 9223371331199999999, 9223371331200000000, math.MaxInt64 - 1, math.MaxInt64}},
		{1184, []int64{math.MinInt64, -211813488000000000, -1, 0, 9223371331199999999, 9223371331200000000, math.MaxInt64 - 1, math.MaxInt64}},
	} {
		for _, descending := range []bool{false, true} {
			layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: tc.id, Storage: model.TypeTimestamp, Modifier: -1}, Descending: descending}}
			var prior []byte
			for _, value := range tc.values {
				encoded, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{{Type: model.TypeTimestamp, IntVal: value}}, 9)
				if err != nil {
					t.Fatal(err)
				}
				if prior != nil {
					order := bytes.Compare(prior, encoded)
					if descending {
						order = -order
					}
					if order >= 0 {
						t.Fatalf("type%d key order at %d", tc.id, value)
					}
				}
				decoded, err := encoder.DecodeKey(t.Context(), layout, encoded, 4096)
				if err != nil || len(decoded) != 1 || decoded[0].Type != model.TypeTimestamp || decoded[0].IntVal != value {
					t.Fatalf("temporal key roundtrip=%v,%v; want%d", decoded, err, value)
				}
				prior = encoded
			}
		}
	}
}

func TestTemporalKeysRejectOutOfDomainValues(t *testing.T) {
	encoder := keyEncoder(t)
	for _, tc := range []struct {
		id    model.TypeID
		value int64
	}{{1082, math.MinInt64}, {1082, math.MaxInt64}, {1083, -1}, {1083, 86400000001}} {
		_, err := encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: tc.id, Storage: model.TypeTimestamp}}}, []model.Value{{Type: model.TypeTimestamp, IntVal: tc.value}}, 9)
		requireSQLState(t, err, "22008")
	}
}

func TestTimetzKeysRoundTripFullZoneDomain(t *testing.T) {
	encoder := keyEncoder(t)
	for _, descending := range []bool{false, true} {
		layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 1266, Storage: model.TypeBytes, Modifier: -1}, Descending: descending}}
		var prior []byte
		for _, west := range []int32{math.MinInt32, -57601, 0, 57601, math.MaxInt32} {
			raw := binary.BigEndian.AppendUint64(nil, 86400000000)
			raw = binary.BigEndian.AppendUint32(raw, uint32(west))
			encoded, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{{Type: model.TypeBytes, BytesVal: raw}}, 13)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := encoder.DecodeKey(t.Context(), layout, encoded, 4096)
			if err != nil || len(decoded) != 1 || !bytes.Equal(decoded[0].BytesVal, raw) {
				t.Fatalf("timetz zone %d roundtrip = %v, %v", west, decoded, err)
			}
			if prior != nil {
				order := bytes.Compare(prior, encoded)
				if descending {
					order = -order
				}
				if order >= 0 {
					t.Fatalf("timetz key order at zone %d", west)
				}
			}
			prior = encoded
		}
	}
}
