package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestIntegerGCDSignsAndZero(t *testing.T) {
	for _, width := range []struct {
		name                    string
		id                      model.FunctionID
		minimum, maximum        int64
		largeDivisor, largeWant int64
	}{
		{"int4", 5044, math.MinInt32, math.MaxInt32, -1073741824, 1073741824},
		{"int8", 5045, math.MinInt64, math.MaxInt64, -4611686018427387904, 4611686018427387904},
	} {
		t.Run(width.name, func(t *testing.T) {
			function, err := builtin.Resolve(width.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name              string
				left, right, want int64
			}{
				{"both zero", 0, 0, 0},
				{"left zero", 0, -42, 42},
				{"right zero", -42, 0, 42},
				{"positive operands", 54, 24, 6},
				{"larger right operand", 24, 54, 6},
				{"negative left", -54, 24, 6},
				{"negative right", 54, -24, 6},
				{"negative operands", -54, -24, 6},
				{"minimum with negative one", width.minimum, -1, 1},
				{"negative one with minimum", -1, width.minimum, 1},
				{"minimum with even divisor", width.minimum, 6, 2},
				{"negative divisor with minimum", -6, width.minimum, 2},
				{"minimum with large divisor", width.minimum, width.largeDivisor, width.largeWant},
				{"minimum with coprime maximum", width.minimum, width.maximum, 1},
			} {
				t.Run(test.name, func(t *testing.T) {
					result, err := function.Call(t.Context(), []model.Value{model.Int64Val(test.left), model.Int64Val(test.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					if err != nil || result.IsNull || result.IntVal != test.want {
						t.Fatalf("gcd(%d, %d) = %v, %v; want %d", test.left, test.right, result, err, test.want)
					}
				})
			}
		})
	}
}

func TestIntegerGCDRejectsUnrepresentableMagnitude(t *testing.T) {
	for _, width := range []struct {
		name    string
		id      model.FunctionID
		minimum int64
	}{
		{"int4", 5044, math.MinInt32},
		{"int8", 5045, math.MinInt64},
	} {
		t.Run(width.name, func(t *testing.T) {
			function, err := builtin.Resolve(width.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name        string
				left, right int64
			}{
				{"left zero", 0, width.minimum},
				{"right zero", width.minimum, 0},
				{"both minimum", width.minimum, width.minimum},
			} {
				t.Run(test.name, func(t *testing.T) {
					_, err := function.Call(t.Context(), []model.Value{model.Int64Val(test.left), model.Int64Val(test.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireSQLState(t, err, "22003")
				})
			}
		})
	}
}

func TestIntegerLCMSignsZeroAndReduction(t *testing.T) {
	for _, width := range []struct {
		name                        string
		id                          model.FunctionID
		minimum, maximum            int64
		largeLeft, largeRight, want int64
	}{
		{"int4", 5046, math.MinInt32, math.MaxInt32, 60000, 45000, 180000},
		{"int8", 5047, math.MinInt64, math.MaxInt64, 6000000000, 4500000000, 18000000000},
	} {
		t.Run(width.name, func(t *testing.T) {
			function, err := builtin.Resolve(width.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name              string
				left, right, want int64
			}{
				{"both zero", 0, 0, 0},
				{"left zero", 0, -42, 0},
				{"right zero", -42, 0, 0},
				{"zero bypasses left minimum", width.minimum, 0, 0},
				{"zero bypasses right minimum", 0, width.minimum, 0},
				{"positive operands", 21, 6, 42},
				{"negative left", -21, 6, 42},
				{"negative right", 21, -6, 42},
				{"negative operands", -21, -6, 42},
				{"divide before multiply", width.largeLeft, width.largeRight, width.want},
				{"negative reduced product", width.largeLeft, -width.largeRight, width.want},
				{"maximum result", width.maximum, width.maximum, width.maximum},
			} {
				t.Run(test.name, func(t *testing.T) {
					result, err := function.Call(t.Context(), []model.Value{model.Int64Val(test.left), model.Int64Val(test.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					if err != nil || result.IsNull || result.IntVal != test.want {
						t.Fatalf("lcm(%d, %d) = %v, %v; want %d", test.left, test.right, result, err, test.want)
					}
				})
			}
		})
	}
}

func TestIntegerLCMRejectsFinalOverflow(t *testing.T) {
	for _, width := range []struct {
		name             string
		id               model.FunctionID
		minimum, maximum int64
	}{
		{"int4", 5046, math.MinInt32, math.MaxInt32},
		{"int8", 5047, math.MinInt64, math.MaxInt64},
	} {
		t.Run(width.name, func(t *testing.T) {
			function, err := builtin.Resolve(width.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name        string
				left, right int64
			}{
				{"minimum with negative one", width.minimum, -1},
				{"negative one with minimum", -1, width.minimum},
				{"minimum with positive one", width.minimum, 1},
				{"minimum after gcd reduction", width.minimum, 2},
				{"both minimum", width.minimum, width.minimum},
				{"positive signed overflow", width.maximum, 2},
				{"negative signed overflow", -width.maximum, 2},
				{"unsigned product overflow", width.maximum, width.maximum - 1},
			} {
				t.Run(test.name, func(t *testing.T) {
					_, err := function.Call(t.Context(), []model.Value{model.Int64Val(test.left), model.Int64Val(test.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireSQLState(t, err, "22003")
				})
			}
		})
	}
}
