package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestNumericRangeLowerOwnsItsReturnedText(t *testing.T) {
	typ := model.TypeLayout{Type: model.Type{ID: 3906, Storage: model.TypeBytes, Modifier: -1}}
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: -1}}
	inputs := []model.TypeLayout{{Type: model.Type{ID: 2275, Storage: model.TypeString, Modifier: -1}}, {Type: model.Type{ID: 26, Storage: model.TypeInt64, Modifier: -1}}, {Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}}
	parse, err := builtin.ResolveTyped(t.Context(), 3834, inputs, typ, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	budget := builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20}
	value, err := parse.Call(t.Context(), []model.Value{model.StringVal("[1.00,3.000)"), model.Int64Val(3906), model.Int64Val(-1)}, budget)
	if err != nil {
		t.Fatal(err)
	}
	lower, err := builtin.ResolveTyped(t.Context(), 3848, []model.TypeLayout{typ}, numeric, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	result, err := lower.Call(t.Context(), []model.Value{value}, budget)
	if err != nil || result.StrVal != "1.00" {
		t.Fatalf("lower range bound = %v, %v", result, err)
	}
	_, err = lower.Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 1 << 20})
	requireSQLState(t, err, "54000")
	_, err = lower.Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 3})
	requireSQLState(t, err, "54000")
	clear(value.BytesVal)
	if result.StrVal != "1.00" {
		t.Fatalf("lower bound retained borrowed range payload: %q", result.StrVal)
	}
}
