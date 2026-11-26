package builtin_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestGetByteUsesUnsignedZeroBasedIndex(t *testing.T) {
	function := callable(t, "get_byte", 17, 23)
	for index, want := range []int64{0, 128, 255} {
		value, err := function.Call(t.Context(), []model.Value{
			model.BytesVal([]byte{0, 0x80, 0xff}), model.Int64Val(int64(index)),
		}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || value.IsNull || value.Type != model.TypeInt64 || value.IntVal != want {
			t.Errorf("get_byte at %d = %v, %v; want %d", index, value, err, want)
		}
	}
}

func TestGetBitUsesLeastSignificantBitFirst(t *testing.T) {
	function := callable(t, "get_bit", 17, 20)
	for _, tc := range []struct {
		index int64
		want  int64
	}{
		{0, 1}, {1, 0}, {7, 0}, {8, 0}, {15, 1},
	} {
		value, err := function.Call(t.Context(), []model.Value{
			model.BytesVal([]byte{0x01, 0x80}), model.Int64Val(tc.index),
		}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || value.IsNull || value.Type != model.TypeInt64 || value.IntVal != tc.want {
			t.Errorf("get_bit at %d = %v, %v; want %d", tc.index, value, err, tc.want)
		}
	}
}

func TestByteAccessRejectsOutOfRangeIndexes(t *testing.T) {
	for _, operation := range []struct {
		name   string
		id     model.FunctionID
		limit  int64
		min    int64
		max    int64
		setter bool
	}{
		{"get_byte", 721, 2, math.MinInt32, math.MaxInt32, false},
		{"set_byte", 722, 2, math.MinInt32, math.MaxInt32, true},
		{"get_bit", 723, 16, math.MinInt64, math.MaxInt64, false},
		{"set_bit", 724, 16, math.MinInt64, math.MaxInt64, true},
	} {
		t.Run(operation.name, func(t *testing.T) {
			function, err := builtin.Resolve(operation.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name  string
				input []byte
				index int64
			}{
				{"negative", []byte{0x81, 0x80}, -1},
				{"past last", []byte{0x81, 0x80}, operation.limit},
				{"minimum", []byte{0x81, 0x80}, operation.min},
				{"maximum", []byte{0x81, 0x80}, operation.max},
				{"empty", nil, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					args := []model.Value{model.BytesVal(tc.input), model.Int64Val(tc.index)}
					if operation.setter {
						args = append(args, model.Int64Val(1))
					}
					_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.input)), MemoryBytes: 64 << 20})
					requireSQLState(t, err, "2202E")
				})
			}
		})
	}
}

func TestByteBitIndexesDoNotWrap(t *testing.T) {
	for _, id := range []model.FunctionID{723, 724} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function, err := builtin.Resolve(id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, index := range []int64{1 << 32, 1 << 61} {
				args := []model.Value{model.BytesVal([]byte{0xff}), model.Int64Val(index)}
				if id == 724 {
					args = append(args, model.Int64Val(1))
				}
				_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "2202E")
			}
		})
	}
}

func TestByteSettersCopyAcrossChunkBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.FunctionID
		index int64
		last  byte
	}{
		{"set_byte", 722, 11999, 0},
		{"set_bit", 724, 95999, 0x7f},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			input := bytes.Repeat([]byte{0x81, 0x55, 0xff}, 4000)
			want := bytes.Clone(input)
			want[len(want)-1] = tc.last
			value, err := function.Call(t.Context(), []model.Value{
				model.BytesVal(input), model.Int64Val(tc.index), model.Int64Val(0),
			}, builtin.CallBudget{OutputBytes: int64(len(input)), MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || !bytes.Equal(value.BytesVal, want) {
				t.Fatalf("setter did not preserve the payload around the changed byte: %v", err)
			}
			if !bytes.Equal(input, bytes.Repeat([]byte{0x81, 0x55, 0xff}, 4000)) {
				t.Fatal("setter mutated borrowed input")
			}
		})
	}
}

func TestSetByteTruncatesToLowByte(t *testing.T) {
	function := callable(t, "set_byte", 17, 23, 23)
	for _, tc := range []struct {
		name  string
		value int64
		want  []byte
	}{
		{"negative", -1, []byte{0xaa, 0xff, 0x55}},
		{"wrap to zero", 256, []byte{0xaa, 0, 0x55}},
		{"discard high byte", 0x1234, []byte{0xaa, 0x34, 0x55}},
		{"minimum int4", math.MinInt32, []byte{0xaa, 0, 0x55}},
		{"maximum int4", math.MaxInt32, []byte{0xaa, 0xff, 0x55}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{
				model.BytesVal([]byte{0xaa, 0xbb, 0x55}), model.Int64Val(1), model.Int64Val(tc.value),
			}, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.Type != model.TypeBytes || !bytes.Equal(value.BytesVal, tc.want) {
				t.Fatalf("set_byte = %v, %v; want %x", value, err, tc.want)
			}
		})
	}
}

func TestSetBitChangesOnlySelectedLittleEndianBit(t *testing.T) {
	function := callable(t, "set_bit", 17, 20, 23)
	for _, tc := range []struct {
		name  string
		index int64
		value int64
		want  []byte
	}{
		{"clear first", 0, 0, []byte{0x80, 0x7e}},
		{"set next byte first", 8, 1, []byte{0x81, 0x7f}},
		{"set last", 15, 1, []byte{0x81, 0xfe}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{
				model.BytesVal([]byte{0x81, 0x7e}), model.Int64Val(tc.index), model.Int64Val(tc.value),
			}, builtin.CallBudget{OutputBytes: 2, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.Type != model.TypeBytes || !bytes.Equal(value.BytesVal, tc.want) {
				t.Fatalf("set_bit = %v, %v; want %x", value, err, tc.want)
			}
		})
	}
}

func TestSetBitValidatesIndexBeforeNewBitAndQuota(t *testing.T) {
	function := callable(t, "set_bit", 17, 20, 23)
	for _, newBit := range []int64{-1, 2, math.MinInt32, math.MaxInt32} {
		t.Run(fmt.Sprint(newBit), func(t *testing.T) {
			args := []model.Value{model.BytesVal([]byte{0x80}), model.Int64Val(8), model.Int64Val(newBit)}
			_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "2202E")
			args[1] = model.Int64Val(7)
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22023")
		})
	}
}

func TestByteSettersOwnUnchangedResultsAtExactQuota(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.FunctionID
		index int64
		value int64
	}{
		{"set_byte", 722, 1, 0x55},
		{"set_bit", 724, 8, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			input := []byte{0x81, 0x55}
			args := []model.Value{model.BytesVal(input), model.Int64Val(tc.index), model.Int64Val(tc.value)}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
			if !bytes.Equal(input, []byte{0x81, 0x55}) {
				t.Fatalf("rejected setter mutated input: %x", input)
			}
			first, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 2, MemoryBytes: 64 << 20})
			if err != nil || first.IsNull || !bytes.Equal(first.BytesVal, []byte{0x81, 0x55}) {
				t.Fatalf("unchanged setter = %v, %v", first, err)
			}
			input[0] = 0x42
			second, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 2, MemoryBytes: 64 << 20})
			if err != nil || second.IsNull || !bytes.Equal(second.BytesVal, []byte{0x42, 0x55}) {
				t.Fatalf("later setter = %v, %v", second, err)
			}
			if !bytes.Equal(first.BytesVal, []byte{0x81, 0x55}) {
				t.Fatalf("result aliases borrowed input or later output: %x", first.BytesVal)
			}
			first.BytesVal[1] = 0
			if !bytes.Equal(input, []byte{0x42, 0x55}) || !bytes.Equal(second.BytesVal, []byte{0x42, 0x55}) {
				t.Fatalf("result mutation changed input %x or later output %x", input, second.BytesVal)
			}
		})
	}
}

func TestByteBitCountIncludesWordsAndTails(t *testing.T) {
	function := callable(t, "bit_count", 17)
	for _, tc := range []struct {
		name  string
		input []byte
		want  int64
	}{
		{"empty", nil, 0},
		{"tail only", []byte{0xff, 0x80, 0x03}, 11},
		{"word with high bit", []byte{0xff, 0, 1, 2, 4, 8, 0x55, 0x80}, 17},
		{"word and seven byte tail", []byte{0xff, 0, 1, 2, 4, 8, 0x55, 0x80, 0xff, 0, 1, 2, 4, 8, 0x55}, 33},
		{"multiple chunks and tail", append(bytes.Repeat([]byte{0xff, 0, 0x55, 0x80}, 3000), 0x0f), 39004},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{model.BytesVal(tc.input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.Type != model.TypeInt64 || value.IntVal != tc.want {
				t.Fatalf("bit_count = %v, %v; want %d", value, err, tc.want)
			}
		})
	}
}

func TestByteAccessStrictNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
		kind model.DataType
	}{
		{"get_byte", 721, []model.Value{model.BytesVal(nil), model.Int64Val(-1)}, model.TypeInt64},
		{"set_byte", 722, []model.Value{model.BytesVal(nil), model.Int64Val(-1), model.Int64Val(256)}, model.TypeBytes},
		{"get_bit", 723, []model.Value{model.BytesVal(nil), model.Int64Val(math.MaxInt64)}, model.TypeInt64},
		{"set_bit", 724, []model.Value{model.BytesVal(nil), model.Int64Val(math.MaxInt64), model.Int64Val(2)}, model.TypeBytes},
		{"bit_count", 6163, []model.Value{model.BytesVal([]byte{0xff})}, model.TypeInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for i := range tc.args {
				args := append([]model.Value(nil), tc.args...)
				args[i].IsNull = true
				value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull || value.Type != tc.kind {
					t.Errorf("NULL argument %d = %v, %v; want NULL type %v", i, value, err, tc.kind)
				}
			}
		})
	}
}

func TestByteAccessCancellation(t *testing.T) {
	input := bytes.Repeat([]byte{0x81, 0x55}, 1<<17)
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
	}{
		{"get_byte", 721, []model.Value{model.BytesVal(input), model.Int64Val(0)}},
		{"set_byte", 722, []model.Value{model.BytesVal(input), model.Int64Val(0), model.Int64Val(0)}},
		{"get_bit", 723, []model.Value{model.BytesVal(input), model.Int64Val(0)}},
		{"set_bit", 724, []model.Value{model.BytesVal(input), model.Int64Val(0), model.Int64Val(0)}},
		{"bit_count", 6163, []model.Value{model.BytesVal(input)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = function.Call(ctx, tc.args, builtin.CallBudget{OutputBytes: int64(len(input)), MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled byte access = %v", err)
			}
			if tc.id == 721 || tc.id == 723 {
				return
			}
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 4}
			_, err = function.Call(interrupted, tc.args, builtin.CallBudget{OutputBytes: int64(len(input)), MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted byte access = %v", err)
			}
			if !bytes.Equal(input, bytes.Repeat([]byte{0x81, 0x55}, 1<<17)) {
				t.Fatal("interrupted byte access mutated borrowed input")
			}
		})
	}
}
