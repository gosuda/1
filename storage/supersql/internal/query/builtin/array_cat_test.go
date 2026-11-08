package builtin_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func arrayCatCallable(t *testing.T, elementID model.TypeID, storage model.DataType) builtin.Callable {
	t.Helper()
	layout := arrayComparisonLayout(t, elementID, storage)
	function, err := builtin.ResolveTyped(t.Context(), 383, []model.TypeLayout{layout, layout}, layout, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func TestArrayCatEqualRanksPreservesFirstOuterBound(t *testing.T) {
	function := arrayCatCallable(t, 23, model.TypeInt64)
	first := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: -2, Length: 2}, {LowerBound: 4, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4)}}
	second := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 9, Length: 1}, {LowerBound: 4, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(5), model.Int64Val(6)}}
	result, err := function.Call(t.Context(), []model.Value{first, second}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	wantDimensions := []model.ArrayDimension{{LowerBound: -2, Length: 3}, {LowerBound: 4, Length: 2}}
	if result.Type != model.TypeArray || result.IsNull || !slices.Equal(result.Dimensions, wantDimensions) || len(result.ArrayVal) != 6 {
		t.Fatalf("concatenation = %v; want dimensions %v and six elements", result, wantDimensions)
	}
	for i, value := range result.ArrayVal {
		if i == 1 {
			if !value.IsNull || value.Type != model.TypeInt64 {
				t.Errorf("element %d = %v; want int4 NULL", i, value)
			}
			continue
		}
		if value.IsNull || value.Type != model.TypeInt64 || value.IntVal != int64(i+1) {
			t.Errorf("element %d = %v; want %d", i, value, i+1)
		}
	}
}

func TestArrayCatAdjacentRanksPreservesLargerBounds(t *testing.T) {
	function := arrayCatCallable(t, 23, model.TypeInt64)
	smaller := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 4, Length: 1}, {LowerBound: 7, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2)}}
	larger := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: -3, Length: 2}, {LowerBound: 4, Length: 1}, {LowerBound: 7, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(3), model.Int64Val(4), model.Int64Val(5), model.Int64Val(6)}}
	for _, tc := range []struct {
		name string
		args [2]model.Value
		want []int64
	}{
		{"front", [2]model.Value{smaller, larger}, []int64{1, 2, 3, 4, 5, 6}},
		{"end", [2]model.Value{larger, smaller}, []int64{3, 4, 5, 6, 1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := function.Call(t.Context(), tc.args[:], builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			wantDimensions := []model.ArrayDimension{{LowerBound: -3, Length: 3}, {LowerBound: 4, Length: 1}, {LowerBound: 7, Length: 2}}
			if result.IsNull || !slices.Equal(result.Dimensions, wantDimensions) || len(result.ArrayVal) != len(tc.want) {
				t.Fatalf("concatenation = %v; want dimensions %v and elements %v", result, wantDimensions, tc.want)
			}
			for i, value := range result.ArrayVal {
				if value.IsNull || value.IntVal != tc.want[i] {
					t.Errorf("element %d = %v; want %d", i, value, tc.want[i])
				}
			}
		})
	}
}

func TestArrayCatChecksEveryInnerDimensionAtMaximumRank(t *testing.T) {
	function := arrayCatCallable(t, 23, model.TypeInt64)
	first := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: -6, Length: 1}, {LowerBound: -5, Length: 1}, {LowerBound: -4, Length: 1}, {LowerBound: -3, Length: 1}, {LowerBound: -2, Length: 1}, {LowerBound: -1, Length: 1}}, ArrayVal: []model.Value{model.Int64Val(1)}}
	for _, rank := range []int{5, 6} {
		second := model.Value{Type: model.TypeArray, Dimensions: slices.Clone(first.Dimensions[6-rank:]), ArrayVal: []model.Value{model.Int64Val(2)}}
		result, err := function.Call(t.Context(), []model.Value{first, second}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
		if err != nil {
			t.Fatal(err)
		}
		wantDimensions := []model.ArrayDimension{{LowerBound: -6, Length: 2}, {LowerBound: -5, Length: 1}, {LowerBound: -4, Length: 1}, {LowerBound: -3, Length: 1}, {LowerBound: -2, Length: 1}, {LowerBound: -1, Length: 1}}
		if !slices.Equal(result.Dimensions, wantDimensions) || len(result.ArrayVal) != 2 || result.ArrayVal[0].IntVal != 1 || result.ArrayVal[1].IntVal != 2 {
			t.Fatalf("rank 6 concatenated with rank %d = %v", rank, result)
		}
		second.Dimensions[rank-1].LowerBound = 1
		_, err = function.Call(t.Context(), []model.Value{first, second}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "2202E")
	}
}

func TestArrayCatNullAndEmptyResults(t *testing.T) {
	function := arrayCatCallable(t, 23, model.TypeInt64)
	null := model.NullVal(model.TypeArray)
	null.ArrayVal = []model.Value{model.Int64Val(99)}
	empty := model.Value{Type: model.TypeArray}
	for _, tc := range []struct {
		name string
		args [2]model.Value
		null bool
	}{
		{"both null", [2]model.Value{null, null}, true},
		{"both empty", [2]model.Value{empty, empty}, false},
		{"null then empty", [2]model.Value{null, empty}, false},
		{"empty then null", [2]model.Value{empty, null}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := function.Call(t.Context(), tc.args[:], builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || result.Type != model.TypeArray || result.IsNull != tc.null || len(result.ArrayVal) != 0 || len(result.Dimensions) != 0 {
				t.Fatalf("concatenation = %v, %v; want NULL=%v without payload", result, err, tc.null)
			}
		})
	}
}

func TestArrayCatNoOpOwnsBorrowedPayload(t *testing.T) {
	function := arrayCatCallable(t, 17, model.TypeBytes)
	for _, tc := range []struct {
		name  string
		other model.Value
		front bool
	}{
		{"null first", model.NullVal(model.TypeArray), true},
		{"null second", model.NullVal(model.TypeArray), false},
		{"empty first", model.Value{Type: model.TypeArray}, true},
		{"empty second", model.Value{Type: model.TypeArray}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := model.Value{Type: model.TypeArray, Dimensions: make([]model.ArrayDimension, 2, 6), ArrayVal: make([]model.Value, 1, 16)}
			input.Dimensions[0] = model.ArrayDimension{LowerBound: -2, Length: 1}
			input.Dimensions[1] = model.ArrayDimension{LowerBound: 4, Length: 1}
			input.ArrayVal[0] = model.BytesVal([]byte{1, 2, 3})
			args := []model.Value{input, tc.other}
			if tc.front {
				args[0], args[1] = args[1], args[0]
			}
			size, err := model.ValueBytes(t.Context(), input, 4096)
			if err != nil {
				t.Fatal(err)
			}
			result, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
			input.ArrayVal[0].BytesVal[0] = 9
			input.ArrayVal[0] = model.NullVal(model.TypeBytes)
			input.Dimensions[0].LowerBound = 8
			wantDimensions := []model.ArrayDimension{{LowerBound: -2, Length: 1}, {LowerBound: 4, Length: 1}}
			if result.IsNull || !slices.Equal(result.Dimensions, wantDimensions) || len(result.ArrayVal) != 1 {
				t.Fatalf("owned result changed with input: %v", result)
			}
			if !bytes.Equal(result.ArrayVal[0].BytesVal, []byte{1, 2, 3}) || cap(result.ArrayVal) != 1 || cap(result.Dimensions) != 2 {
				t.Fatalf("result aliases or retains excess input storage: %v", result)
			}
		})
	}
}

func TestArrayCatRejectsIncompatibleDimensions(t *testing.T) {
	function := arrayCatCallable(t, 23, model.TypeInt64)
	matrix := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}, {LowerBound: 4, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2)}}
	for _, tc := range []struct {
		name  string
		other model.Value
	}{
		{"equal rank inner bound", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}, {LowerBound: 5, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(3), model.Int64Val(4)}}},
		{"equal rank inner length", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}, {LowerBound: 4, Length: 1}}, ArrayVal: []model.Value{model.Int64Val(3)}}},
		{"adjacent rank inner bound", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 5, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(3), model.Int64Val(4)}}},
		{"adjacent rank inner length", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 4, Length: 1}}, ArrayVal: []model.Value{model.Int64Val(3)}}},
		{"rank difference two", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}, {LowerBound: 4, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(3), model.Int64Val(4)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, args := range [][2]model.Value{{matrix, tc.other}, {tc.other, matrix}} {
				_, err := function.Call(t.Context(), args[:], builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "2202E")
			}
		})
	}
}

func TestArrayCatValidatesShapesBeforeNoOp(t *testing.T) {
	function := arrayCatCallable(t, 23, model.TypeInt64)
	for _, invalid := range []model.Value{
		{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}},
		{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}},
		{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(1)}},
		{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: math.MaxInt32, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2)}},
		{Type: model.TypeArray, Dimensions: make([]model.ArrayDimension, 7)},
	} {
		for _, other := range []model.Value{model.NullVal(model.TypeArray), {Type: model.TypeArray}} {
			for _, args := range [][2]model.Value{{invalid, other}, {other, invalid}} {
				_, err := function.Call(t.Context(), args[:], builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "2202E")
			}
		}
	}
}

func TestArrayCatChecksOnePastLastBound(t *testing.T) {
	function := arrayCatCallable(t, 23, model.TypeInt64)
	row := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 4, Length: 1}}, ArrayVal: []model.Value{model.Int64Val(2)}}
	for _, rank := range []int{1, 2} {
		for _, lower := range []int32{math.MaxInt32 - 2, math.MaxInt32 - 1} {
			first := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: lower, Length: 1}}, ArrayVal: []model.Value{model.Int64Val(1)}}
			if rank == 2 {
				first.Dimensions = append(first.Dimensions, model.ArrayDimension{LowerBound: 4, Length: 1})
			}
			args := [][2]model.Value{{first, row}}
			if rank == 2 {
				args = append(args, [2]model.Value{row, first})
			}
			for _, arguments := range args {
				result, err := function.Call(t.Context(), arguments[:], builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
				if lower == math.MaxInt32-1 {
					requireSQLState(t, err, "54000")
					continue
				}
				if err != nil || len(result.Dimensions) != rank || result.Dimensions[0] != (model.ArrayDimension{LowerBound: lower, Length: 2}) {
					t.Fatalf("rank %d at final legal bound = %v, %v", rank, result, err)
				}
			}
		}
	}
}

func TestArrayCatOwnsBothInputsAtExactBudget(t *testing.T) {
	function := arrayCatCallable(t, 17, model.TypeBytes)
	first := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: -1, Length: 1}}, ArrayVal: []model.Value{model.BytesVal([]byte{1, 2})}}
	second := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 9, Length: 1}}, ArrayVal: []model.Value{model.BytesVal([]byte{3, 4})}}
	want := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: -1, Length: 2}}, ArrayVal: []model.Value{model.BytesVal([]byte{1, 2}), model.BytesVal([]byte{3, 4})}}
	size, err := model.ValueBytes(t.Context(), want, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := function.Call(t.Context(), []model.Value{first, second}, builtin.CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	_, err = function.Call(t.Context(), []model.Value{first, second}, builtin.CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	first.ArrayVal[0].BytesVal[0] = 8
	second.ArrayVal[0].BytesVal[0] = 9
	first.Dimensions[0].LowerBound = 7
	if result.IsNull || !slices.Equal(result.Dimensions, want.Dimensions) || len(result.ArrayVal) != 2 {
		t.Fatalf("owned concatenation changed with inputs: %v", result)
	}
	for i, value := range result.ArrayVal {
		if !bytes.Equal(value.BytesVal, want.ArrayVal[i].BytesVal) {
			t.Errorf("owned element %d = %v; want %v", i, value.BytesVal, want.ArrayVal[i].BytesVal)
		}
	}
}

func TestArrayCatCancellation(t *testing.T) {
	function := arrayCatCallable(t, 23, model.TypeInt64)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := function.Call(ctx, []model.Value{model.NullVal(model.TypeArray), model.NullVal(model.TypeArray)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled concatenation = %v", err)
	}
	input := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 4096}}, ArrayVal: make([]model.Value, 4096)}
	for i := range input.ArrayVal {
		input.ArrayVal[i] = model.NullVal(model.TypeInt64)
	}
	for _, other := range []model.Value{input, model.NullVal(model.TypeArray)} {
		ctx, cancel := context.WithCancel(t.Context())
		interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 20}
		_, err := function.Call(interrupted, []model.Value{input, other}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted concatenation = %v", err)
		}
		if !input.ArrayVal[0].IsNull || input.Dimensions[0].Length != 4096 {
			t.Fatal("canceled concatenation mutated its input")
		}
	}
}
