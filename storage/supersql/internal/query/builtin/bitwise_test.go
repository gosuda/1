package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestIntegerBitwiseSignedWidths(t *testing.T) {
	for _, width := range []struct {
		prefix           string
		id               model.TypeID
		minimum, maximum int64
	}{{"int2", 21, math.MinInt16, math.MaxInt16}, {"int4", 23, math.MinInt32, math.MaxInt32}, {"int8", 20, math.MinInt64, math.MaxInt64}} {
		t.Run(width.prefix, func(t *testing.T) {
			for _, test := range []struct {
				op                string
				left, right, want int64
			}{{"and", 7, 10, 2}, {"or", 7, 10, 15}, {"xor", 7, 10, 13}, {"and", width.minimum, -1, width.minimum}, {"xor", width.minimum, -1, width.maximum}} {
				result, err := callable(t, width.prefix+test.op, width.id, width.id).Call(t.Context(), []model.Value{model.Int64Val(test.left), model.Int64Val(test.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || result.IsNull || result.IntVal != test.want {
					t.Fatalf("%s %s result=%v,%v want %d", width.prefix, test.op, result, err, test.want)
				}
			}
			result, err := callable(t, width.prefix+"not", width.id).Call(t.Context(), []model.Value{model.Int64Val(7)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || result.IntVal != -8 {
				t.Fatalf("%snot=%v,%v", width.prefix, result, err)
			}
		})
	}
}

func TestIntegerShiftUsesPromotedNativeWidth(t *testing.T) {
	for _, test := range []struct {
		prefix                    string
		id                        model.TypeID
		value, count, left, right int64
	}{
		{"int2", 21, 1, 15, math.MinInt16, 0}, {"int2", 21, 1, 16, 0, 0}, {"int2", 21, 1, 32, 1, 1},
		{"int2", 21, math.MinInt16, 1, 0, -16384}, {"int2", 21, -1, -1, 0, -1},
		{"int4", 23, 1, 31, math.MinInt32, 0}, {"int4", 23, 1, 32, 1, 1}, {"int4", 23, 1, -1, math.MinInt32, 0},
		{"int4", 23, math.MinInt32, 1, 0, -1073741824}, {"int4", 23, 7, math.MinInt32, 7, 7},
		{"int8", 20, 1, 63, math.MinInt64, 0}, {"int8", 20, 1, 64, 1, 1}, {"int8", 20, 1, -1, math.MinInt64, 0},
		{"int8", 20, math.MinInt64, 1, 0, -4611686018427387904}, {"int8", 20, -1, math.MaxInt32, math.MinInt64, -1},
	} {
		for _, direction := range []struct {
			name string
			want int64
		}{{"shl", test.left}, {"shr", test.right}} {
			result, err := callable(t, test.prefix+direction.name, test.id, 23).Call(t.Context(), []model.Value{model.Int64Val(test.value), model.Int64Val(test.count)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || result.IsNull || result.IntVal != direction.want {
				t.Fatalf("%s%s(%d,%d)=%v,%v; want %d", test.prefix, direction.name, test.value, test.count, result, err, direction.want)
			}
		}
	}
}
