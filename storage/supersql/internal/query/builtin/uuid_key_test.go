package builtin_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"math"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func uuidKeyDatum(text string) model.Value {
	return model.Value{Type: model.TypeUUID, StrVal: text}
}

func TestUUIDKeyCanonicalEncoding(t *testing.T) {
	encoder := keyEncoder(t)
	const canonical = "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	for _, descending := range []bool{false, true} {
		layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 2950, Storage: model.TypeUUID}, Descending: descending}}
		want := []byte{1, 0xa0, 0xee, 0xbc, 0x99, 0x9c, 0x0b, 0x4e, 0xf8, 0xbb, 0x6d, 0x6b, 0xb9, 0xbd, 0x38, 0x0a, 0x11}
		if descending {
			for i := 1; i < len(want); i++ {
				want[i] ^= 0xff
			}
		}
		for _, text := range []string{
			canonical,
			"A0EEBC999C0B4EF8BB6D6BB9BD380A11",
			"{A0EE-BC99-9C0B-4EF8-BB6D-6BB9-BD38-0A11}",
		} {
			input := []model.Value{uuidKeyDatum(text)}
			key, err := encoder.EncodePrefix(t.Context(), layout, input, 17)
			if err != nil || !bytes.Equal(key, want) || cap(key) != len(key) {
				t.Fatalf("UUID %q descending=%v: key=%x capacity=%d, %v; want %x", text, descending, key, cap(key), err, want)
			}
			values, err := encoder.DecodeKey(t.Context(), layout, key, uint64(unsafe.Sizeof(model.Value{}))+17+36)
			if err != nil || len(values) != 1 {
				t.Fatalf("decode %x: %v, %v", key, values, err)
			}
			if values[0].Type != model.TypeUUID || values[0].IsNull || values[0].StrVal != canonical {
				t.Fatalf("decoded UUID = %v, want %s", values[0], canonical)
			}
			if roundtrip := encodeKey(t, encoder, layout, values...); !bytes.Equal(roundtrip, key) {
				t.Fatalf("UUID roundtrip changed key: %x vs %x", roundtrip, key)
			}
			key[1] ^= 0xff
			if values[0].StrVal != canonical || input[0].StrVal != text {
				t.Fatal("UUID key buffers alias datum text")
			}
		}
	}
}

func TestUUIDKeyOrderMatchesComparator(t *testing.T) {
	encoder := keyEncoder(t)
	compare := callable(t, "uuid_cmp", 2950, 2950)
	texts := []string{
		"00000000-0000-0000-0000-000000000000",
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-0000000000ff",
		"00000000-0000-0000-0000-000000000100",
		"00000000-0000-0000-0001-000000000000",
		"00000000-0000-0001-0000-000000000000",
		"00000000-0001-0000-0000-000000000000",
		"00000001-0000-0000-0000-000000000000",
		"7fffffff-ffff-ffff-ffff-ffffffffffff",
		"80000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	}
	for _, descending := range []bool{false, true} {
		layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 2950, Storage: model.TypeUUID}, Descending: descending}}
		keys := make([][]byte, len(texts))
		for i, text := range texts {
			keys[i] = encodeKey(t, encoder, layout, uuidKeyDatum(text))
		}
		for i, left := range texts {
			for j, right := range texts {
				want := cmp.Compare(i, j)
				order, err := compare.Call(t.Context(), []model.Value{uuidKeyDatum(left), uuidKeyDatum(right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || order.IsNull || cmp.Compare(order.IntVal, int64(0)) != want {
					t.Fatalf("uuid_cmp(%s,%s) = %v, %v; want sign %d", left, right, order, err, want)
				}
				if descending {
					want = -want
				}
				if got := bytes.Compare(keys[i], keys[j]); got != want {
					t.Fatalf("UUID key order (%s,%s), descending=%v: %d, want %d", left, right, descending, got, want)
				}
				if i != j && (bytes.HasPrefix(keys[i], keys[j]) || bytes.HasPrefix(keys[j], keys[i])) {
					t.Fatalf("distinct UUID fields share a complete prefix: %s, %s", left, right)
				}
			}
		}
	}
}

func TestUUIDKeyNullableCompositePrefixes(t *testing.T) {
	encoder := keyEncoder(t)
	uuidType := model.Type{ID: 2950, Storage: model.TypeUUID}
	zero := uuidKeyDatum("00000000-0000-0000-0000-000000000000")
	high := uuidKeyDatum("ffffffff-ffff-ffff-ffff-ffffffffffff")
	for _, tc := range []struct {
		descending, nullsFirst bool
		tailOrder              int
	}{
		{false, false, 1}, {false, true, 1}, {true, false, -1}, {true, true, -1},
	} {
		layout := []storage.KeyColumn{
			{ColumnID: 1, Type: uuidType, Descending: tc.descending, NullsFirst: tc.nullsFirst},
			{ColumnID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}},
			{ColumnID: 3, Type: uuidType, Descending: !tc.descending, NullsFirst: !tc.nullsFirst},
		}
		for _, first := range []model.Value{model.NullVal(model.TypeUUID), zero, high} {
			prefix := encodeKey(t, encoder, layout, first)
			decoded, err := encoder.DecodePrefix(t.Context(), layout, prefix, 1<<20)
			if err != nil || len(decoded) != 1 {
				t.Fatalf("prefix decode=%v,%v", decoded, err)
			}
			requireUUIDKeyDatum(t, decoded[0], first)
			if values, err := encoder.DecodeKey(t.Context(), layout, prefix, 1<<20); !errors.Is(err, storage.ErrInvalidEncoding) || values != nil {
				t.Fatalf("incomplete tuple=%v,%v", values, err)
			}
			lowKey := encodeKey(t, encoder, layout, first, model.BytesVal([]byte{0, 255}), zero)
			highKey := encodeKey(t, encoder, layout, first, model.BytesVal([]byte{0, 255}), high)
			if !bytes.HasPrefix(lowKey, prefix) || !bytes.HasPrefix(highKey, prefix) {
				t.Fatal("tuple extension lost its prefix")
			}
			if bytes.Compare(lowKey, highKey) != tc.tailOrder {
				t.Fatal("trailing UUID lost its independent direction")
			}
			values, err := encoder.DecodeKey(t.Context(), layout, highKey, 1<<20)
			if err != nil || len(values) != 3 {
				t.Fatalf("tuple decode=%v,%v", values, err)
			}
			requireUUIDKeyDatum(t, values[0], first)
			requireUUIDKeyDatum(t, values[2], high)
			if !bytes.Equal(values[1].BytesVal, []byte{0, 255}) {
				t.Fatal("bytea field boundaries changed")
			}
		}
	}
}

func TestUUIDKeyNullPlacementDoesNotDependOnDirection(t *testing.T) {
	encoder := keyEncoder(t)
	value := uuidKeyDatum("00000000-0000-0000-0000-000000000000")
	for _, tc := range []struct {
		descending, nullsFirst bool
		order                  int
	}{{false, false, 1}, {true, false, 1}, {false, true, -1}, {true, true, -1}} {
		layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 2950, Storage: model.TypeUUID}, Descending: tc.descending, NullsFirst: tc.nullsFirst}}
		null := encodeKey(t, encoder, layout, model.NullVal(model.TypeUUID))
		present := encodeKey(t, encoder, layout, value)
		if order := bytes.Compare(null, present); order != tc.order {
			t.Fatalf("descending=%v nullsFirst=%v order=%d want=%d", tc.descending, tc.nullsFirst, order, tc.order)
		}
	}
}

func requireUUIDKeyDatum(t *testing.T, got, want model.Value) {
	t.Helper()
	if got.Type != model.TypeUUID || got.IsNull != want.IsNull || got.StrVal != want.StrVal {
		t.Fatalf("UUID datum=%v, want %v", got, want)
	}
}

func TestUUIDKeyRejectsMalformedValues(t *testing.T) {
	encoder := keyEncoder(t)
	layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 2950, Storage: model.TypeUUID}}}
	for _, text := range []string{
		"",
		"0000000000000000000000000000000",
		"00000000-0000-0000-0000-00000000000g",
		"00000000-0000-0000-0000-000000000000x",
		"00000000-0000-0000-0000-000000000000\x00",
		"00000000--0000-0000-0000-000000000000",
		"{00000000-0000-0000-0000-000000000000",
		" 00000000-0000-0000-0000-000000000000",
	} {
		key, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{uuidKeyDatum(text)}, 17)
		requireSQLState(t, err, "22P02")
		if key != nil {
			t.Fatalf("malformed UUID %q returned partial key %x", text, key)
		}
	}
	for _, value := range []model.Value{model.StringVal("00000000-0000-0000-0000-000000000000"), model.NullVal(model.TypeString)} {
		key, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{value}, 17)
		requireSQLState(t, err, "42804")
		if key != nil {
			t.Fatalf("non-UUID physical value returned key %x", key)
		}
	}
}

func TestUUIDKeyRejectsMalformedEncoding(t *testing.T) {
	encoder := keyEncoder(t)
	for _, descending := range []bool{false, true} {
		layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 2950, Storage: model.TypeUUID}, Descending: descending}}
		key := encodeKey(t, encoder, layout, uuidKeyDatum("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"))
		for length := range len(key) {
			values, err := encoder.DecodeKey(t.Context(), layout, key[:length], 1<<20)
			if !errors.Is(err, storage.ErrInvalidEncoding) || values != nil {
				t.Fatalf("truncated UUID key length %d descending=%v: %v, %v", length, descending, values, err)
			}
		}
		for _, malformed := range [][]byte{{0}, {3}, {255}, append(bytes.Clone(key), 0), append([]byte{2}, key[1:]...)} {
			values, err := encoder.DecodePrefix(t.Context(), layout, malformed, 1<<20)
			if !errors.Is(err, storage.ErrInvalidEncoding) || values != nil {
				t.Fatalf("malformed UUID key %x descending=%v: %v, %v", malformed, descending, values, err)
			}
		}
	}
}

func TestUUIDKeyBudgetsAndOwnership(t *testing.T) {
	encoder := keyEncoder(t)
	layout := []storage.KeyColumn{
		{ColumnID: 1, Type: model.Type{ID: 2950, Storage: model.TypeUUID}},
		{ColumnID: 2, Type: model.Type{ID: 2950, Storage: model.TypeUUID}, Descending: true},
	}
	value := uuidKeyDatum("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")
	for _, input := range [][]model.Value{{value}, {model.NullVal(model.TypeUUID)}, {value, value}} {
		fields := layout[:len(input)]
		want := encodeKey(t, encoder, fields, input...)
		for budget := range uint64(len(want)) {
			key, err := encoder.EncodePrefix(t.Context(), fields, input, budget)
			if !errors.Is(err, columnar.ErrBatchBudget) || key != nil {
				t.Fatalf("UUID encode budget %d: %x, %v", budget, key, err)
			}
		}
		key, err := encoder.EncodePrefix(t.Context(), fields, input, uint64(len(want)))
		if err != nil || !bytes.Equal(key, want) || cap(key) != len(key) {
			t.Fatalf("UUID exact encode budget: %x capacity %d, %v", key, cap(key), err)
		}
		decodeBudget := uint64(len(input))*uint64(unsafe.Sizeof(model.Value{})) + uint64(len(key))
		for _, datum := range input {
			if !datum.IsNull {
				decodeBudget += 36
			}
		}
		for budget := range decodeBudget {
			values, err := encoder.DecodeKey(t.Context(), fields, key, budget)
			if !errors.Is(err, columnar.ErrBatchBudget) || values != nil {
				t.Fatalf("UUID decode budget %d/%d: %v, %v", budget, decodeBudget, values, err)
			}
		}
		values, err := encoder.DecodeKey(t.Context(), fields, key, decodeBudget)
		if err != nil || len(values) != len(input) {
			t.Fatalf("UUID exact decode budget: %v, %v", values, err)
		}
		if roundtrip := encodeKey(t, encoder, fields, values...); !bytes.Equal(roundtrip, key) {
			t.Fatalf("UUID budgeted roundtrip: %x, want %x", roundtrip, key)
		}
		key[0] ^= 0xff
		if bytes.Equal(key, want) {
			t.Fatal("UUID encoder calls share output buffers")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if key, err := encoder.EncodePrefix(ctx, layout[:1], []model.Value{value}, math.MaxUint64); !errors.Is(err, context.Canceled) || key != nil {
		t.Fatalf("canceled UUID encode: %x, %v", key, err)
	}
	key := encodeKey(t, encoder, layout[:1], value)
	if values, err := encoder.DecodeKey(ctx, layout[:1], key, math.MaxUint64); !errors.Is(err, context.Canceled) || values != nil {
		t.Fatalf("canceled UUID decode: %v, %v", values, err)
	}
}
