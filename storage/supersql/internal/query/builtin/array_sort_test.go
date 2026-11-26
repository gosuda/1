package builtin_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestArraySortOwnsOutputAndHonorsInvocationLimits(t *testing.T) {
	layout := arrayComparisonLayout(t, 23, model.TypeInt64)
	call, err := builtin.ResolveTyped(t.Context(), 6388, []model.TypeLayout{layout}, layout, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	input := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(3), model.Int64Val(1), model.Int64Val(2)}, Dimensions: []model.ArrayDimension{{LowerBound: 4, Length: 3}}}
	args := []model.Value{input}
	output, err := call.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Value{model.Int64Val(1), model.Int64Val(2), model.Int64Val(3)}
	if !reflect.DeepEqual(output.ArrayVal, want) || output.Dimensions[0].LowerBound != 4 {
		t.Fatalf("sorted value = %v", output)
	}
	output.ArrayVal[0] = model.Int64Val(99)
	output.Dimensions[0].LowerBound = 10
	if input.ArrayVal[0].IntVal != 3 || input.Dimensions[0].LowerBound != 4 {
		t.Fatal("sorted output aliases caller array")
	}
	_, err = call.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 0})
	requireSQLState(t, err, "54000")
	_, err = call.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 1 << 20})
	requireSQLState(t, err, "54000")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = call.Call(ctx, args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled sort = %v", err)
	}
}
