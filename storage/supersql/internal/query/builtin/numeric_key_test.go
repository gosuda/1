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

func TestNumericKeysAgreeWithExactComparison(t *testing.T) {
	encoder := keyEncoder(t)
	compare := callable(t, "numeric_cmp", 1700, 1700)
	groups := [][]string{
		{"-Infinity", "-inf"},
		{"-1e300"},
		{"-9007199254740993"},
		{"-9007199254740992"},
		{"-10", "-1e1"},
		{"-1.001"},
		{"-1", "-1.000"},
		{"-0.10000000000000000001"},
		{"-0.1", "-10e-2"},
		{"-1e-300"},
		{"0", "-0.000", "+0e100", "0e-100"},
		{"1e-300"},
		{"0.1", "0.1000", "+10e-2"},
		{"0.10000000000000000001"},
		{"1", "01.000", "100e-2"},
		{"1.00000000000000000001"},
		{"1.001"},
		{"1.01"},
		{"1.1"},
		{"9.99"},
		{"10", "1e1", "1000e-2"},
		{"9007199254740992"},
		{"9007199254740993"},
		{"1e300"},
		{"Infinity", "+inf"},
		{"NaN", "nan", "NAN"},
	}
	for _, descending := range []bool{false, true} {
		layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 1700, Storage: model.TypeNumeric}, Descending: descending}}
		type entry struct {
			group int
			text  string
			key   []byte
		}
		var entries []entry
		for group, texts := range groups {
			for _, text := range texts {
				entries = append(entries, entry{group, text, encodeKey(t, encoder, layout, numericDatum(text))})
			}
		}
		for _, left := range entries {
			for _, right := range entries {
				want := int64(0)
				if left.group < right.group {
					want = -1
				}
				if left.group > right.group {
					want = 1
				}
				order, err := compare.Call(t.Context(), []model.Value{numericDatum(left.text), numericDatum(right.text)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || order.IntVal != want {
					t.Fatalf("numeric_cmp(%s,%s) = %v, %v; want %d", left.text, right.text, order, err, want)
				}
				if descending {
					want = -want
				}
				if got := bytes.Compare(left.key, right.key); int64(got) != want {
					t.Fatalf("key order (%s,%s), descending=%v: %d, want %d", left.text, right.text, descending, got, want)
				}
				if want != 0 && (bytes.HasPrefix(left.key, right.key) || bytes.HasPrefix(right.key, left.key)) {
					t.Fatalf("distinct numeric fields share a complete prefix: %s, %s", left.text, right.text)
				}
			}
		}
	}
}

func TestNumericKeyCompositePrefixBounds(t *testing.T) {
	encoder := keyEncoder(t)
	numericType := model.Type{ID: 1700, Storage: model.TypeNumeric}
	for _, descending := range []bool{false, true} {
		for _, nullsFirst := range []bool{false, true} {
			layout := []storage.KeyColumn{
				{ColumnID: 1, Type: numericType, Descending: descending, NullsFirst: nullsFirst},
				{ColumnID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}},
				{ColumnID: 3, Type: numericType, Descending: !descending, NullsFirst: !nullsFirst},
				{ColumnID: 4, Type: model.Type{ID: 20, Storage: model.TypeInt64}},
			}
			nullKey := encodeKey(t, encoder, layout, model.NullVal(model.TypeNumeric))
			for _, text := range []string{"-Infinity", "-1.001", "-1", "0", "1", "1.001", "Infinity", "NaN"} {
				prefix := encodeKey(t, encoder, layout, numericDatum(text))
				order := bytes.Compare(nullKey, prefix)
				if !nullsFirst {
					order = -order
				}
				if order >= 0 {
					t.Fatalf("NULL ordering for %s: descending=%v nullsFirst=%v", text, descending, nullsFirst)
				}
			}
			for _, first := range []model.Value{model.NullVal(model.TypeNumeric), numericDatum("-1"), numericDatum("0"), numericDatum("1"), numericDatum("NaN")} {
				prefix := encodeKey(t, encoder, layout, first)
				for _, third := range []model.Value{model.NullVal(model.TypeNumeric), numericDatum("-1.001"), numericDatum("1.001"), numericDatum("NaN")} {
					full := encodeKey(t, encoder, layout, first, model.BytesVal([]byte{0, 255}), third, model.Int64Val(-1))
					if !bytes.HasPrefix(full, prefix) {
						t.Fatalf("numeric bound lost tuple extension: %x, %x", prefix, full)
					}
				}
			}
			for _, first := range []string{"-1", "1"} {
				prefix := encodeKey(t, encoder, layout, numericDatum(first+".00"))
				matching := encodeKey(t, encoder, layout, numericDatum(first+"e0"), model.BytesVal([]byte{0}), numericDatum("2"), model.Int64Val(0))
				neighbor := encodeKey(t, encoder, layout, numericDatum(first+".0001"), model.BytesVal(nil), model.NullVal(model.TypeNumeric), model.Int64Val(math.MinInt64))
				if !bytes.HasPrefix(matching, prefix) || bytes.HasPrefix(neighbor, prefix) {
					t.Fatalf("canonical prefix bound includes wrong equality class: %s", first)
				}
				if bytes.Compare(prefix, neighbor) != bytes.Compare(matching, neighbor) {
					t.Fatalf("tuple extension crosses neighboring numeric bound: %s", first)
				}
			}
			low := encodeKey(t, encoder, layout, numericDatum("1"), model.BytesVal([]byte{0}), numericDatum("-1"), model.Int64Val(math.MaxInt64))
			high := encodeKey(t, encoder, layout, numericDatum("1.00"), model.BytesVal([]byte{0}), numericDatum("1"), model.Int64Val(math.MinInt64))
			order := bytes.Compare(low, high)
			if !descending {
				order = -order
			}
			if order >= 0 {
				t.Fatal("second numeric field lost its independent direction")
			}
		}
	}
}

func TestNumericKeyBudgetsAndOwnership(t *testing.T) {
	encoder := keyEncoder(t)
	layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}}
	for _, text := range []string{"-Infinity", "0", "NaN", "1", "-1.00000000000000000001", "1e32767", "1e-16383"} {
		values := []model.Value{numericDatum(text)}
		key := encodeKey(t, encoder, layout, values...)
		for budget := range uint64(len(key)) {
			got, err := encoder.EncodePrefix(t.Context(), layout, values, budget)
			if !errors.Is(err, columnar.ErrBatchBudget) || got != nil {
				t.Fatalf("%s budget %d: %x, %v", text, budget, got, err)
			}
		}
		exact, err := encoder.EncodePrefix(t.Context(), layout, values, uint64(len(key)))
		if err != nil || !bytes.Equal(key, exact) || cap(exact) != len(exact) {
			t.Fatalf("%s exact budget: %x, %v, capacity %d", text, exact, err, cap(exact))
		}
		key[0] ^= 255
		if bytes.Equal(key, exact) {
			t.Fatal("numeric keys share output storage")
		}
		if values[0].StrVal != text {
			t.Fatal("numeric key canonicalization mutated its datum")
		}
	}
	values := []model.Value{numericDatum(strings.Repeat("9", 32768))}
	if key, err := encoder.EncodePrefix(t.Context(), layout, values, 32775); err != nil || len(key) != 32775 || cap(key) != len(key) {
		t.Fatalf("work-limit boundary: key length %d, %v", len(key), err)
	}
	if key, err := encoder.EncodePrefix(t.Context(), layout, values, 32774); !errors.Is(err, columnar.ErrBatchBudget) || key != nil {
		t.Fatalf("work-limit value byte quota: key length %d, %v", len(key), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if key, err := encoder.EncodePrefix(ctx, layout, values, math.MaxUint64); !errors.Is(err, context.Canceled) || key != nil {
		t.Fatalf("canceled numeric key: key length %d, %v", len(key), err)
	}
}

func TestNumericKeyCompositeByteQuota(t *testing.T) {
	encoder := keyEncoder(t)
	numericType := model.Type{ID: 1700, Storage: model.TypeNumeric}
	layout := []storage.KeyColumn{
		{ColumnID: 1, Type: numericType},
		{ColumnID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}},
		{ColumnID: 3, Type: numericType, Descending: true},
		{ColumnID: 4, Type: numericType},
	}
	values := []model.Value{numericDatum("1.00"), model.BytesVal([]byte{0}), model.NullVal(model.TypeNumeric), numericDatum("-1.001")}
	want := encodeKey(t, encoder, layout, values...)
	for budget := range uint64(len(want)) {
		key, err := encoder.EncodePrefix(t.Context(), layout, values, budget)
		if !errors.Is(err, columnar.ErrBatchBudget) || key != nil {
			t.Fatalf("composite quota %d returned %x, %v", budget, key, err)
		}
	}
	key, err := encoder.EncodePrefix(t.Context(), layout, values, uint64(len(want)))
	if err != nil || !bytes.Equal(key, want) || cap(key) != len(key) {
		t.Fatalf("exact composite quota returned %x, %v", key, err)
	}
}

func TestNumericKeyRejectsMalformedAndOverBudgetWork(t *testing.T) {
	encoder := keyEncoder(t)
	layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}}
	if key, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{numericDatum(strings.Repeat("0", 65537))}, 1); !errors.Is(err, columnar.ErrBatchBudget) || key != nil {
		t.Fatalf("minimum byte quota must precede numeric parsing: %x, %v", key, err)
	}
	for _, test := range []struct{ text, state string }{
		{"", "22P02"}, {"1.2.3", "22P02"}, {"1e+", "22P02"}, {"NaNgarbage", "22P02"},
		{"1e-16384", "22003"}, {"0e-16384", "22003"},
		{"1e32768", "54000"}, {strings.Repeat("1", 32769), "54000"},
		{strings.Repeat("0", 65537), "54000"},
	} {
		key, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{numericDatum(test.text)}, 1<<20)
		requireSQLState(t, err, test.state)
		if key != nil {
			t.Fatal("failed numeric encoding returned a partial key")
		}
	}
	for _, test := range []struct {
		typ   model.Type
		value model.Value
	}{
		{model.Type{ID: 1700, Storage: model.TypeString}, model.StringVal("1")},
		{model.Type{ID: 1700, Storage: model.TypeNumeric, Collation: 950}, numericDatum("1")},
		{layout[0].Type, model.StringVal("1")},
		{layout[0].Type, model.NullVal(model.TypeString)},
	} {
		key, err := encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: test.typ}}, []model.Value{test.value}, 100)
		requireSQLState(t, err, "42804")
		if key != nil {
			t.Fatal("invalid numeric storage returned a key")
		}
	}
}
