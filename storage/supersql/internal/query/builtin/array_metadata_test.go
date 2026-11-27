package builtin_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestArrayMetadataPreservesMultidimensionalBounds(t *testing.T) {
	value := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4), model.Int64Val(5), model.Int64Val(6)}, Dimensions: []model.ArrayDimension{{LowerBound: -2, Length: 2}, {LowerBound: 4, Length: 3}}}
	for _, tc := range []struct {
		id              model.FunctionID
		dimension, want int64
		text            string
	}{
		{747, 0, 0, "[-2:-1][4:6]"}, {748, 0, 2, ""}, {3179, 0, 6, ""},
		{2091, 1, -2, ""}, {2092, 1, -1, ""}, {2176, 2, 3, ""},
	} {
		t.Run(fmt.Sprint(tc.id), func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			args := []model.Value{value}
			if function.Signature().ArgumentCount == 2 {
				args = append(args, model.Int64Val(tc.dimension))
			}
			got, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.text)), MemoryBytes: 64 << 20})
			if err != nil || got.IsNull || got.IntVal != tc.want || got.StrVal != tc.text {
				t.Fatalf("metadata = %v, %v; want %d %q", got, err, tc.want, tc.text)
			}
		})
	}
	for _, id := range []model.FunctionID{2091, 2092, 2176} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, dimension := range []int64{math.MinInt32, 0, 3, math.MaxInt32} {
			got, err := function.Call(t.Context(), []model.Value{value, model.Int64Val(dimension)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !got.IsNull || got.Type != model.TypeInt64 {
				t.Fatalf("dimension %d: %v, %v; want NULL", dimension, got, err)
			}
		}
	}
}

func TestArrayMetadataDistinguishesEmptyFromNull(t *testing.T) {
	for _, id := range []model.FunctionID{747, 748, 2091, 2092, 2176, 3179} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function, err := builtin.Resolve(id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			args := []model.Value{{Type: model.TypeArray}}
			if function.Signature().ArgumentCount == 2 {
				args = append(args, model.Int64Val(1))
			}
			got, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IsNull != (id != 3179) || got.IntVal != 0 {
				t.Fatalf("empty array: %v, %v", got, err)
			}
			args[0].IsNull = true
			got, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !got.IsNull {
				t.Fatalf("NULL array: %v, %v", got, err)
			}
		})
	}
}

func TestArrayDimensionTextAdmitsMaximumBounds(t *testing.T) {
	function := callable(t, "array_dims", 2277)
	value := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}, Dimensions: make([]model.ArrayDimension, 6)}
	for i := range value.Dimensions {
		value.Dimensions[i] = model.ArrayDimension{LowerBound: math.MinInt32, Length: 1}
	}
	want := strings.Repeat("[-2147483648:-2147483648]", 6)
	got, err := function.Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || got.StrVal != want {
		t.Fatalf("dimensions: %v, %v; want %q", got, err, want)
	}
	_, err = function.Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	value.Dimensions[0].Length = 2
	_, err = function.Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "2202E")
}

func TestArrayMetadataRequiresConcreteArrayStorage(t *testing.T) {
	function := callable(t, "array_ndims", 2277)
	result := model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}
	for _, id := range []model.TypeID{1007, 1009} {
		if err := function.ValidateTypes([]model.Type{{ID: id, Storage: model.TypeArray, Modifier: -1}}, result); err != nil {
			t.Fatal(err)
		}
	}
	for _, argument := range []model.Type{{ID: 23, Storage: model.TypeInt64}, {ID: 23, Storage: model.TypeArray}, {ID: 1007, Storage: model.TypeInt64}, {ID: 0, Storage: model.TypeArray}, {ID: 2277, Storage: model.TypeArray}} {
		requireSQLState(t, function.ValidateTypes([]model.Type{argument}, result), "42804")
	}
}
