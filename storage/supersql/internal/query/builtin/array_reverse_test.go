package builtin_test

import (
	"math"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestArrayReverseOwnsRowsWithoutReversingInnerElements(t *testing.T) {
	layout := arrayComparisonLayout(t, 17, model.TypeBytes)
	function, err := builtin.ResolveTyped(t.Context(), 6381, []model.TypeLayout{layout}, layout, builtin.ResolveOptions{}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	input := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: -2, Length: 3}, {LowerBound: 4, Length: 2}}, ArrayVal: []model.Value{model.BytesVal([]byte("a")), model.BytesVal([]byte("b")), model.BytesVal([]byte("c")), model.NullVal(model.TypeBytes), model.BytesVal([]byte("e")), model.BytesVal([]byte("f"))}}
	size, err := model.ValueBytes(t.Context(), input, 4096)
	if err != nil {
		t.Fatal(err)
	}
	_, err = function.Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	result, err := function.Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Dimensions, input.Dimensions) {
		t.Fatalf("bounds=%v, want %v", result.Dimensions, input.Dimensions)
	}
	input.Dimensions[0].LowerBound = 1
	input.ArrayVal[0].BytesVal[0] = 'X'
	for i, want := range []string{"e", "f", "c", "", "a", "b"} {
		if i == 3 {
			if !result.ArrayVal[i].IsNull {
				t.Fatal("NULL slot was lost")
			}
			continue
		}
		if got := string(result.ArrayVal[i].BytesVal); got != want {
			t.Fatalf("element %d=%q, want %q", i, got, want)
		}
	}
	if result.Dimensions[0].LowerBound != -2 {
		t.Fatal("owned bounds changed with input")
	}
}

func TestArrayReverseChecksConstructionBoundsOnlyWhenReordering(t *testing.T) {
	function := callable(t, "array_reverse", 2277)
	singleton := comparisonArray(model.Int64Val(7))
	singleton.Dimensions[0].LowerBound = math.MaxInt32
	result, err := function.Call(t.Context(), []model.Value{singleton}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	singleton.ArrayVal[0] = model.Int64Val(99)
	if result.Dimensions[0].LowerBound != math.MaxInt32 || result.ArrayVal[0].IntVal != 7 {
		t.Fatalf("singleton=%v", result)
	}
	pair := comparisonArray(model.Int64Val(1), model.Int64Val(2))
	pair.Dimensions[0].LowerBound = math.MaxInt32 - 1
	_, err = function.Call(t.Context(), []model.Value{pair}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	null, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeArray)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !null.IsNull {
		t.Fatalf("NULL reverse=%v,%v", null, err)
	}
	empty, err := function.Call(t.Context(), []model.Value{{Type: model.TypeArray}}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || empty.IsNull || len(empty.ArrayVal) != 0 || len(empty.Dimensions) != 0 {
		t.Fatalf("empty reverse=%v,%v", empty, err)
	}
}
