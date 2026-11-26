package builtin_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func keyEncoder(t *testing.T) *builtin.KeyEncoder {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(100)
	if err != nil {
		t.Fatal(err)
	}
	return encoder
}

func encodeKey(t *testing.T, encoder *builtin.KeyEncoder, layout []storage.KeyColumn, values ...model.Value) []byte {
	t.Helper()
	key, err := encoder.EncodePrefix(t.Context(), layout, values, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestKeyPostgresPrimitiveOrdering(t *testing.T) {
	encoder := keyEncoder(t)
	cases := []struct {
		name   string
		typ    model.Type
		values []model.Value
	}{
		{"bool", model.Type{ID: 16, Storage: model.TypeBoolean}, []model.Value{model.BoolVal(false), model.BoolVal(true)}},
		{"int2", model.Type{ID: 21, Storage: model.TypeInt64}, []model.Value{model.Int64Val(math.MinInt16), model.Int64Val(-1), model.Int64Val(0), model.Int64Val(1), model.Int64Val(math.MaxInt16)}},
		{"int4", model.Type{ID: 23, Storage: model.TypeInt64}, []model.Value{model.Int64Val(math.MinInt32), model.Int64Val(-1), model.Int64Val(0), model.Int64Val(1), model.Int64Val(math.MaxInt32)}},
		{"int8", model.Type{ID: 20, Storage: model.TypeInt64}, []model.Value{model.Int64Val(math.MinInt64), model.Int64Val(-1), model.Int64Val(0), model.Int64Val(1), model.Int64Val(math.MaxInt64)}},
		{"float4", model.Type{ID: 700, Storage: model.TypeFloat64}, []model.Value{model.Float64Val(math.Inf(-1)), model.Float64Val(-math.MaxFloat32), model.Float64Val(-math.SmallestNonzeroFloat32), model.Float64Val(0), model.Float64Val(math.SmallestNonzeroFloat32), model.Float64Val(math.MaxFloat32), model.Float64Val(math.Inf(1)), model.Float64Val(math.NaN())}},
		{"float8", model.Type{ID: 701, Storage: model.TypeFloat64}, []model.Value{model.Float64Val(math.Inf(-1)), model.Float64Val(-math.MaxFloat64), model.Float64Val(-math.SmallestNonzeroFloat64), model.Float64Val(0), model.Float64Val(math.SmallestNonzeroFloat64), model.Float64Val(math.MaxFloat64), model.Float64Val(math.Inf(1)), model.Float64Val(math.NaN())}},
		{"text", model.Type{ID: 25, Storage: model.TypeString, Collation: 100}, []model.Value{model.StringVal(""), model.StringVal("\x01"), model.StringVal("\x01\x01"), model.StringVal("\x01a"), model.StringVal("a"), model.StringVal("a\x01"), model.StringVal("a "), model.StringVal("é"), model.StringVal("😀")}},
		{"varchar", model.Type{ID: 1043, Storage: model.TypeString, Collation: 951}, []model.Value{model.StringVal(""), model.StringVal(" "), model.StringVal("a"), model.StringVal("a "), model.StringVal("b")}},
		{"bpchar", model.Type{ID: 1042, Storage: model.TypeString, Collation: 950}, []model.Value{model.StringVal(" "), model.StringVal("a "), model.StringVal("a\x01 "), model.StringVal("a b"), model.StringVal("b ")}},
		{"bytea", model.Type{ID: 17, Storage: model.TypeBytes}, []model.Value{model.BytesVal(nil), model.BytesVal([]byte{0}), model.BytesVal([]byte{0, 0}), model.BytesVal([]byte{0, 255}), model.BytesVal([]byte{1}), model.BytesVal([]byte{255}), model.BytesVal([]byte{255, 0}), model.BytesVal([]byte{255, 255})}},
	}
	for _, id := range []model.TypeID{26, 24, 2202, 2203, 2204, 2205, 2206, 3734, 3769, 4089, 4096, 4191} {
		cases = append(cases, struct {
			name   string
			typ    model.Type
			values []model.Value
		}{"oid", model.Type{ID: id, Storage: model.TypeInt64}, []model.Value{model.Int64Val(0), model.Int64Val(math.MaxInt32), model.Int64Val(math.MaxInt32 + 1), model.Int64Val(math.MaxUint32)}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, descending := range []bool{false, true} {
				layout := []storage.KeyColumn{{ColumnID: 1, Type: tc.typ, Descending: descending}}
				var previous []byte
				for i, value := range tc.values {
					key := encodeKey(t, encoder, layout, value)
					if i > 0 {
						order := bytes.Compare(previous, key)
						if descending {
							order = -order
						}
						if order >= 0 {
							t.Fatalf("type %d descending=%v pair %d: %x vs %x", tc.typ.ID, descending, i, previous, key)
						}
					}
					previous = key
				}
			}
		})
	}
}

func TestKeyLogicalEqualityKeepsPayloadBits(t *testing.T) {
	encoder := keyEncoder(t)
	for _, id := range []model.TypeID{700, 701} {
		layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: id, Storage: model.TypeFloat64}}}
		for _, pair := range [][2]float64{{0, math.Copysign(0, -1)}, {math.Float64frombits(0x7ff8000000000001), math.Float64frombits(0xfff0000000000001)}} {
			values := []model.Value{model.Float64Val(pair[0]), model.Float64Val(pair[1])}
			left := encodeKey(t, encoder, layout, values[0])
			right := encodeKey(t, encoder, layout, values[1])
			if !bytes.Equal(left, right) {
				t.Fatalf("equal float keys differ: %x vs %x", left, right)
			}
			for i := range values {
				if math.Float64bits(values[i].FloatVal) != math.Float64bits(pair[i]) {
					t.Fatal("key canonicalization changed the mutation payload")
				}
			}
		}
	}
	layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 1042, Storage: model.TypeString, Collation: 100}}}
	for _, pair := range [][2]string{{"", "   "}, {"a", "a   "}, {"a\u00a0", "a\u00a0 "}} {
		if !bytes.Equal(encodeKey(t, encoder, layout, model.StringVal(pair[0])), encodeKey(t, encoder, layout, model.StringVal(pair[1]))) {
			t.Fatalf("bpchar equality differs for %q", pair)
		}
	}
	if bytes.Equal(encodeKey(t, encoder, layout, model.StringVal("a")), encodeKey(t, encoder, layout, model.StringVal("a\u00a0"))) {
		t.Fatal("bpchar stripped a non-U+0020 space")
	}
}

func TestKeyNullableCompositePrefixes(t *testing.T) {
	encoder := keyEncoder(t)
	textType := model.Type{ID: 25, Storage: model.TypeString, Collation: 950}
	for _, descending := range []bool{false, true} {
		for _, nullsFirst := range []bool{false, true} {
			layout := []storage.KeyColumn{{ColumnID: 1, Type: textType, Descending: descending, NullsFirst: nullsFirst}, {ColumnID: 2, Type: model.Type{ID: 20, Storage: model.TypeInt64}, Descending: !descending, NullsFirst: !nullsFirst}}
			nullKey := encodeKey(t, encoder, layout, model.NullVal(model.TypeString))
			emptyKey := encodeKey(t, encoder, layout, model.StringVal(""))
			order := bytes.Compare(nullKey, emptyKey)
			if !nullsFirst {
				order = -order
			}
			if order >= 0 {
				t.Fatalf("NULL ordering: descending=%v nullsFirst=%v", descending, nullsFirst)
			}
			for _, first := range []model.Value{model.NullVal(model.TypeString), model.StringVal(""), model.StringVal("\x01"), model.StringVal("a")} {
				prefix := encodeKey(t, encoder, layout, first)
				for _, second := range []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(math.MinInt64), model.Int64Val(0), model.Int64Val(math.MaxInt64)} {
					full := encodeKey(t, encoder, layout, first, second)
					if !bytes.HasPrefix(full, prefix) {
						t.Fatalf("complete tuple field lost prefix: %x vs %x", prefix, full)
					}
				}
				low := encodeKey(t, encoder, layout, first, model.Int64Val(math.MinInt64))
				high := encodeKey(t, encoder, layout, first, model.Int64Val(math.MaxInt64))
				order := bytes.Compare(low, high)
				if !descending {
					order = -order
				}
				if order >= 0 {
					t.Fatal("second-field direction not preserved")
				}
			}
			for _, second := range []model.Value{model.Int64Val(math.MinInt64), model.Int64Val(math.MaxInt64)} {
				short := encodeKey(t, encoder, layout, model.StringVal("a"), second)
				long := encodeKey(t, encoder, layout, model.StringVal("a\x01"), second)
				if bytes.HasPrefix(long, short) || bytes.HasPrefix(short, long) {
					t.Fatal("variable field boundary collides with tuple extension")
				}
			}
		}
	}
}

func TestKeyRejectsInvalidResolvedValues(t *testing.T) {
	encoder := keyEncoder(t)
	cases := []struct {
		name  string
		typ   model.Type
		value model.Value
	}{
		{"unresolved", model.Type{Storage: model.TypeInt64}, model.Int64Val(0)},
		{"physical type", model.Type{ID: 20, Storage: model.TypeFloat64}, model.Float64Val(0)},
		{"UUID physical type", model.Type{ID: 2950, Storage: model.TypeString}, model.StringVal("00000000-0000-0000-0000-000000000000")},
		{"UUID collation", model.Type{ID: 2950, Storage: model.TypeUUID, Collation: 950}, model.Value{Type: model.TypeUUID, StrVal: "00000000-0000-0000-0000-000000000000"}},
		{"value type", model.Type{ID: 20, Storage: model.TypeInt64}, model.Float64Val(0)},
		{"null type", model.Type{ID: 20, Storage: model.TypeInt64}, model.NullVal(model.TypeFloat64)},
		{"int2 low", model.Type{ID: 21, Storage: model.TypeInt64}, model.Int64Val(math.MinInt16 - 1)},
		{"int2 high", model.Type{ID: 21, Storage: model.TypeInt64}, model.Int64Val(math.MaxInt16 + 1)},
		{"int4 high", model.Type{ID: 23, Storage: model.TypeInt64}, model.Int64Val(math.MaxInt32 + 1)},
		{"oid negative", model.Type{ID: 26, Storage: model.TypeInt64}, model.Int64Val(-1)},
		{"oid high", model.Type{ID: 2205, Storage: model.TypeInt64}, model.Int64Val(math.MaxUint32 + 1)},
		{"boolean", model.Type{ID: 16, Storage: model.TypeBoolean}, model.Value{Type: model.TypeBoolean, IntVal: 2}},
		{"binary32", model.Type{ID: 700, Storage: model.TypeFloat64}, model.Float64Val(0.1)},
		{"unresolved collation", model.Type{ID: 25, Storage: model.TypeString}, model.StringVal("a")},
		{"locale", model.Type{ID: 25, Storage: model.TypeString, Collation: 999}, model.StringVal("a")},
		{"nontext collation", model.Type{ID: 20, Storage: model.TypeInt64, Collation: 950}, model.Int64Val(1)},
		{"UTF8", model.Type{ID: 25, Storage: model.TypeString, Collation: 950}, model.StringVal("\xff")},
		{"text NUL", model.Type{ID: 25, Storage: model.TypeString, Collation: 950}, model.StringVal("a\x00")},
		{"varchar NUL", model.Type{ID: 1043, Storage: model.TypeString, Collation: 950}, model.StringVal("\x00")},
		{"bpchar NUL", model.Type{ID: 1042, Storage: model.TypeString, Collation: 950}, model.StringVal("\x00 ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: tc.typ}}, []model.Value{tc.value}, 100)
			if err == nil || key != nil {
				t.Fatalf("invalid datum accepted: key=%x error=%v", key, err)
			}
			if strings.ContainsRune(tc.value.StrVal, 0) {
				var sqlErr *model.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code != "22021" {
					t.Fatalf("NUL text must report 22021: %v", err)
				}
			}
		})
	}
	if _, err := builtin.NewKeyEncoder(999); err == nil {
		t.Fatal("locale default silently fell back to C")
	}
	valid := storage.KeyColumn{ColumnID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	for _, layout := range [][]storage.KeyColumn{{{Type: valid.Type}}, {valid, valid}, {valid, {ColumnID: 2, Type: model.Type{ID: 0, Storage: model.TypeInt64}}}} {
		if _, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{model.Int64Val(1)}, 100); err == nil {
			t.Fatal("invalid complete layout accepted for a prefix")
		}
	}
	if _, err := encoder.EncodePrefix(t.Context(), []storage.KeyColumn{valid}, []model.Value{model.Int64Val(1), model.Int64Val(2)}, 100); err == nil {
		t.Fatal("tuple exceeds layout")
	}
}

func TestKeyBudgetCancellationOwnership(t *testing.T) {
	encoder := keyEncoder(t)
	layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
	input := []byte{0, 255, 0}
	values := []model.Value{model.BytesVal(input)}
	key := encodeKey(t, encoder, layout, values...)
	if cap(key) != len(key) {
		t.Fatalf("retained excess capacity %d > %d", cap(key), len(key))
	}
	for budget := range uint64(len(key)) {
		if got, err := encoder.EncodePrefix(t.Context(), layout, values, budget); !errors.Is(err, columnar.ErrBatchBudget) || got != nil {
			t.Fatalf("budget %d returned %x, %v", budget, got, err)
		}
	}
	exact, err := encoder.EncodePrefix(t.Context(), layout, values, uint64(len(key)))
	if err != nil || !bytes.Equal(key, exact) {
		t.Fatalf("exact budget: %x, %v", exact, err)
	}
	input[0] = 99
	if !bytes.Equal(key, exact) {
		t.Fatal("key retained caller bytes")
	}
	key[0] ^= 255
	if bytes.Equal(key, exact) {
		t.Fatal("encoder calls share output storage")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if key, err := encoder.EncodePrefix(ctx, layout, values, math.MaxUint64); !errors.Is(err, context.Canceled) || key != nil {
		t.Fatalf("canceled encode: %x, %v", key, err)
	}
	textLayout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 100}}}
	if key, err := encoder.EncodePrefix(t.Context(), textLayout, []model.Value{model.StringVal(strings.Repeat("a", 8192))}, 10); !errors.Is(err, columnar.ErrBatchBudget) || key != nil {
		t.Fatalf("large value exceeded budget: %x, %v", key, err)
	}
	if key, err := encoder.EncodePrefix(t.Context(), layout, nil, 0); err != nil || len(key) != 0 {
		t.Fatalf("empty tuple: %x, %v", key, err)
	}
}
