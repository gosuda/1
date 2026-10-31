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

func TestArrayTrimResetsSliceBoundsAndRemovesOuterRows(t *testing.T) {
	function := callable(t, "trim_array", 2277, 23)
	input := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: -2, Length: 2}, {LowerBound: 4, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2), model.Int64Val(3), model.Int64Val(4)}}
	for _, count := range []int64{0, 1} {
		result, err := function.Call(t.Context(), []model.Value{input, model.Int64Val(count)}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
		if err != nil {
			t.Fatal(err)
		}
		want := []model.ArrayDimension{{LowerBound: 1, Length: int32(2 - count)}, {LowerBound: 1, Length: 2}}
		if result.IsNull || !slices.Equal(result.Dimensions, want) || len(result.ArrayVal) != int(4-count*2) {
			t.Fatalf("trim %d = %v", count, result)
		}
		for i, value := range result.ArrayVal {
			if value.IntVal != int64(i+1) {
				t.Fatalf("element %d=%v", i, value)
			}
		}
	}
	if input.Dimensions[0].LowerBound != -2 || input.Dimensions[1].LowerBound != 4 {
		t.Fatal("trim changed input bounds")
	}
	result, err := function.Call(t.Context(), []model.Value{input, model.Int64Val(2)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || result.IsNull || len(result.ArrayVal) != 0 || len(result.Dimensions) != 0 {
		t.Fatalf("full trim = %v, %v", result, err)
	}
}

func TestArrayTrimOwnsOnlyRetainedPayloadAtExactBudget(t *testing.T) {
	function := callable(t, "trim_array", 2277, 23)
	input := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}}, ArrayVal: []model.Value{model.BytesVal([]byte{1, 2}), model.BytesVal(bytes.Repeat([]byte{9}, 1<<20))}}
	want := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}, ArrayVal: []model.Value{model.BytesVal([]byte{1, 2})}}
	size, err := model.ValueBytes(t.Context(), want, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := function.Call(t.Context(), []model.Value{input, model.Int64Val(1)}, builtin.CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	input.ArrayVal[0].BytesVal[0] = 7
	if !bytes.Equal(result.ArrayVal[0].BytesVal, []byte{1, 2}) || cap(result.ArrayVal) != 1 || cap(result.Dimensions) != 1 {
		t.Fatalf("retained result aliases or retains discarded storage: %v", result)
	}
	_, err = function.Call(t.Context(), []model.Value{input, model.Int64Val(1)}, builtin.CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestArrayTrimRejectsInvalidCountsAndPreservesStrictNull(t *testing.T) {
	function := callable(t, "trim_array", 2277, 23)
	array := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}, ArrayVal: []model.Value{model.Int64Val(1)}}
	for _, count := range []int64{-1, 2, math.MaxInt32} {
		_, err := function.Call(t.Context(), []model.Value{array, model.Int64Val(count)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "2202E")
	}
	for _, args := range [][]model.Value{{model.NullVal(model.TypeArray), model.Int64Val(-1)}, {array, model.NullVal(model.TypeInt64)}} {
		value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull || value.Type != model.TypeArray {
			t.Fatalf("NULL trim = %v, %v", value, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := function.Call(ctx, []model.Value{array, model.Int64Val(0)}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled trim=%v", err)
	}
}

func TestTypedArrayTrimRejectsChangingElementIdentity(t *testing.T) {
	input := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}}}
	output := model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64, Modifier: -1}}}}
	_, err := builtin.ResolveTyped(t.Context(), 6172, []model.TypeLayout{input, {Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}}, output, builtin.ResolveOptions{}, 4096)
	requireSQLState(t, err, "42804")
}

func TestArrayTrimCancellationDuringOwnedCopy(t *testing.T) {
	function := callable(t, "trim_array", 2277, 23)
	input := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 4096}}, ArrayVal: make([]model.Value, 4096)}
	for i := range input.ArrayVal {
		input.ArrayVal[i] = model.StringVal("field")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 5000}
	_, err := function.Call(interrupted, []model.Value{input, model.Int64Val(0)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted trim = %v", err)
	}
	if input.ArrayVal[0].StrVal != "field" || input.ArrayVal[len(input.ArrayVal)-1].StrVal != "field" {
		t.Fatal("interrupted trim changed borrowed elements")
	}
}
