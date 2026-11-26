package builtin_test

import (
	"bytes"
	"math"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

var arrayPushFunctions = [...]struct {
	name string
	id   model.FunctionID
}{
	{"append", 378},
	{"prepend", 379},
}

func arrayPushCallable(t *testing.T, id model.FunctionID, array model.TypeLayout) builtin.Callable {
	t.Helper()
	arguments := []model.TypeLayout{array, array.Children[0]}
	if id == 379 {
		arguments[0], arguments[1] = arguments[1], arguments[0]
	}
	function, err := builtin.ResolveTyped(t.Context(), id, arguments, array, builtin.ResolveOptions{}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func arrayPushArguments(id model.FunctionID, array, element model.Value) [2]model.Value {
	if id == 379 {
		return [2]model.Value{element, array}
	}
	return [2]model.Value{array, element}
}

func TestArrayPushDistinguishesNullArrayFromNullElement(t *testing.T) {
	for _, operation := range arrayPushFunctions {
		t.Run(operation.name, func(t *testing.T) {
			function := arrayPushCallable(t, operation.id, arrayComparisonLayout(t, 23, model.TypeInt64))
			for _, tc := range []struct {
				name    string
				array   model.Value
				element model.Value
				want    []model.Value
			}{
				{"null array", model.NullVal(model.TypeArray), model.Int64Val(7), []model.Value{model.Int64Val(7)}},
				{"empty array", comparisonArray(), model.Int64Val(7), []model.Value{model.Int64Val(7)}},
				{"null array and element", model.NullVal(model.TypeArray), model.NullVal(model.TypeInt64), []model.Value{model.NullVal(model.TypeInt64)}},
				{"empty array and null element", comparisonArray(), model.NullVal(model.TypeInt64), []model.Value{model.NullVal(model.TypeInt64)}},
				{"existing and inserted null slots", comparisonArray(model.NullVal(model.TypeInt64)), model.NullVal(model.TypeInt64), []model.Value{model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					arguments := arrayPushArguments(operation.id, tc.array, tc.element)
					result, err := function.Call(t.Context(), arguments[:], builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
					if err != nil {
						t.Fatal(err)
					}
					wantDimensions := []model.ArrayDimension{{LowerBound: 1, Length: int32(len(tc.want))}}
					if result.Type != model.TypeArray || result.IsNull || !slices.Equal(result.Dimensions, wantDimensions) || len(result.ArrayVal) != len(tc.want) {
						t.Fatalf("result = %v, want nonnull array with dimensions %v", result, wantDimensions)
					}
					for i, want := range tc.want {
						got := result.ArrayVal[i]
						if got.Type != want.Type || got.IsNull != want.IsNull || got.IntVal != want.IntVal {
							t.Errorf("element %d = %v, want %v", i, got, want)
						}
					}
				})
			}
		})
	}
}

func TestArrayPushPreservesLowerBoundAndElementOrder(t *testing.T) {
	for _, operation := range arrayPushFunctions {
		t.Run(operation.name, func(t *testing.T) {
			function := arrayPushCallable(t, operation.id, arrayComparisonLayout(t, 23, model.TypeInt64))
			for _, lower := range []int32{-4, 7} {
				input := comparisonArray(model.Int64Val(10), model.Int64Val(20))
				input.Dimensions[0].LowerBound = lower
				arguments := arrayPushArguments(operation.id, input, model.Int64Val(30))
				result, err := function.Call(t.Context(), arguments[:], builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
				if err != nil {
					t.Fatal(err)
				}
				want := []int64{10, 20, 30}
				if operation.id == 379 {
					want = []int64{30, 10, 20}
				}
				if result.IsNull || len(result.Dimensions) != 1 || result.Dimensions[0] != (model.ArrayDimension{LowerBound: lower, Length: 3}) || len(result.ArrayVal) != len(want) {
					t.Fatalf("lower bound %d: result = %v", lower, result)
				}
				for i, value := range result.ArrayVal {
					if value.IsNull || value.IntVal != want[i] {
						t.Errorf("lower bound %d: element %d = %v, want %d", lower, i, value, want[i])
					}
				}
				if input.Dimensions[0] != (model.ArrayDimension{LowerBound: lower, Length: 2}) || input.ArrayVal[0].IntVal != 10 || input.ArrayVal[1].IntVal != 20 {
					t.Fatalf("push changed borrowed input: %v", input)
				}
			}
		})
	}
}

func TestArrayPushRejectsMultidimensionalAndMalformedArrays(t *testing.T) {
	for _, operation := range arrayPushFunctions {
		t.Run(operation.name, func(t *testing.T) {
			function := arrayPushCallable(t, operation.id, arrayComparisonLayout(t, 23, model.TypeInt64))
			for _, tc := range []struct {
				name       string
				dimensions []model.ArrayDimension
				code       string
			}{
				{"two dimensions", []model.ArrayDimension{{LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}}, "22000"},
				{"mismatched element count", []model.ArrayDimension{{LowerBound: 1, Length: 2}}, "2202E"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					input := model.Value{Type: model.TypeArray, Dimensions: tc.dimensions, ArrayVal: []model.Value{model.Int64Val(1)}}
					arguments := arrayPushArguments(operation.id, input, model.Int64Val(2))
					_, err := function.Call(t.Context(), arguments[:], builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
					requireSQLState(t, err, tc.code)
				})
			}
		})
	}
}

func TestArrayPushSubscriptBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.FunctionID
		lower int32
		code  string
	}{
		{"append at minimum lower bound", 378, math.MinInt32, ""},
		{"prepend temporary lower bound fits", 379, math.MinInt32 + 1, ""},
		{"prepend temporary lower bound underflows", 379, math.MinInt32, "22003"},
		{"append one-past-last fits", 378, math.MaxInt32 - 2, ""},
		{"prepend one-past-last fits", 379, math.MaxInt32 - 2, ""},
		{"append one-past-last overflows", 378, math.MaxInt32 - 1, "54000"},
		{"prepend last index reaches maximum", 379, math.MaxInt32 - 1, ""},
		{"append index overflows", 378, math.MaxInt32, "22003"},
		{"prepend temporary one-past-last overflows", 379, math.MaxInt32, "54000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := arrayPushCallable(t, tc.id, arrayComparisonLayout(t, 23, model.TypeInt64))
			input := comparisonArray(model.Int64Val(1))
			input.Dimensions[0].LowerBound = tc.lower
			arguments := arrayPushArguments(tc.id, input, model.Int64Val(2))
			result, err := function.Call(t.Context(), arguments[:], builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if tc.code != "" {
				requireSQLState(t, err, tc.code)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.IsNull || len(result.ArrayVal) != 2 || len(result.Dimensions) != 1 || result.Dimensions[0] != (model.ArrayDimension{LowerBound: tc.lower, Length: 2}) {
				t.Fatalf("boundary result = %v", result)
			}
		})
	}
}

func TestArrayPushOwnsBytePayloadAtExactBudget(t *testing.T) {
	for _, operation := range arrayPushFunctions {
		t.Run(operation.name, func(t *testing.T) {
			function := arrayPushCallable(t, operation.id, arrayComparisonLayout(t, 17, model.TypeBytes))
			input := comparisonArray(model.BytesVal([]byte("old")))
			inserted := model.BytesVal([]byte("new"))
			arguments := arrayPushArguments(operation.id, input, inserted)
			want := comparisonArray(model.BytesVal([]byte("old")), model.BytesVal([]byte("new")))
			if operation.id == 379 {
				want.ArrayVal[0], want.ArrayVal[1] = want.ArrayVal[1], want.ArrayVal[0]
			}
			size, err := model.ValueBytes(t.Context(), want, 4096)
			if err != nil {
				t.Fatal(err)
			}
			_, err = function.Call(t.Context(), arguments[:], builtin.CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
			result, err := function.Call(t.Context(), arguments[:], builtin.CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			input.ArrayVal[0].BytesVal[0] = '!'
			inserted.BytesVal[0] = '?'
			input.ArrayVal[0] = model.NullVal(model.TypeBytes)
			input.Dimensions[0].LowerBound = 10
			if result.IsNull || !slices.Equal(result.Dimensions, want.Dimensions) || len(result.ArrayVal) != len(want.ArrayVal) {
				t.Fatalf("result changed through borrowed array storage: %v", result)
			}
			for i, value := range result.ArrayVal {
				if value.Type != model.TypeBytes || value.IsNull || !bytes.Equal(value.BytesVal, want.ArrayVal[i].BytesVal) {
					t.Errorf("owned element %d = %v, want %v", i, value, want.ArrayVal[i])
				}
			}
		})
	}
}

func TestArrayPushOwnsNestedArraysAtExactBudget(t *testing.T) {
	for _, operation := range arrayPushFunctions {
		t.Run(operation.name, func(t *testing.T) {
			layout := arrayComparisonLayout(t, 22, model.TypeArray)
			layout.Children[0].Children = []model.TypeLayout{{Type: model.Type{ID: 21, Storage: model.TypeInt64, Modifier: -1}}}
			function := arrayPushCallable(t, operation.id, layout)
			original := comparisonArray(model.Int64Val(1), model.Int64Val(2))
			original.Dimensions[0].LowerBound = 0
			inserted := comparisonArray(model.Int64Val(3), model.Int64Val(4))
			inserted.Dimensions[0].LowerBound = 0
			input := comparisonArray(original)
			input.Dimensions[0].LowerBound = -3
			arguments := arrayPushArguments(operation.id, input, inserted)
			want := comparisonArray(original, inserted)
			want.Dimensions[0].LowerBound = -3
			size, err := model.ValueBytes(t.Context(), want, 4096)
			if err != nil {
				t.Fatal(err)
			}
			_, err = function.Call(t.Context(), arguments[:], builtin.CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
			result, err := function.Call(t.Context(), arguments[:], builtin.CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			original.Dimensions[0].LowerBound = 19
			original.ArrayVal[0] = model.Int64Val(9)
			inserted.Dimensions[0].LowerBound = 29
			inserted.ArrayVal[0] = model.Int64Val(8)
			input.ArrayVal[0] = model.NullVal(model.TypeArray)
			input.Dimensions[0].LowerBound = 30
			if result.IsNull || len(result.Dimensions) != 1 || result.Dimensions[0] != (model.ArrayDimension{LowerBound: -3, Length: 2}) || len(result.ArrayVal) != 2 {
				t.Fatalf("nested result changed through input: %v", result)
			}
			expected := [2][2]int64{{1, 2}, {3, 4}}
			if operation.id == 379 {
				expected[0], expected[1] = expected[1], expected[0]
			}
			for i, nested := range result.ArrayVal {
				if nested.Type != model.TypeArray || nested.IsNull || len(nested.ArrayVal) != 2 || len(nested.Dimensions) != 1 || nested.Dimensions[0] != (model.ArrayDimension{LowerBound: 0, Length: 2}) {
					t.Fatalf("owned nested array %d = %v", i, nested)
				}
				for j, value := range nested.ArrayVal {
					if value.IsNull || value.IntVal != expected[i][j] {
						t.Errorf("owned nested element [%d][%d] = %v, want %d", i, j, value, expected[i][j])
					}
				}
			}
		})
	}
}
