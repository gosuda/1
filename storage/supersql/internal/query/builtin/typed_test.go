package builtin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTypedArrayCodecOwnsResolvedIdentity(t *testing.T) {
	arguments := []model.TypeLayout{
		{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}},
		{Type: model.Type{ID: 26, Modifier: -1, Storage: model.TypeInt64}},
		{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}},
	}
	result := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}}}
	function, err := builtin.ResolveTyped(t.Context(), 750, arguments, result, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	result.Children[0].Type.ID = 25
	arguments[0].Type.ID = 25
	value, err := function.Call(t.Context(), []model.Value{model.StringVal("{7,NULL}"), model.Int64Val(23), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || len(value.ArrayVal) != 2 || value.ArrayVal[0].IntVal != 7 || !value.ArrayVal[1].IsNull {
		t.Fatalf("bound codec changed after layout mutation: %v, %v", value, err)
	}
	_, err = function.Call(t.Context(), []model.Value{model.StringVal("{7}"), model.Int64Val(25), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "42804")
}

func TestTypedArrayCodecRejectsMismatchedLayoutAndBudgets(t *testing.T) {
	arguments := []model.TypeLayout{{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}, {Type: model.Type{ID: 26, Modifier: -1, Storage: model.TypeInt64}}, {Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}}
	result := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}}}
	_, err := builtin.ResolveTyped(t.Context(), 750, arguments, result, builtin.ResolveOptions{}, 1)
	requireSQLState(t, err, "54000")
	result.Children[0].Type = model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}
	_, err = builtin.ResolveTyped(t.Context(), 750, arguments, result, builtin.ResolveOptions{}, 1<<20)
	requireSQLState(t, err, "42804")
	_, err = builtin.Resolve(750, builtin.ResolveOptions{})
	requireSQLState(t, err, "42804")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = builtin.ResolveTyped(ctx, 750, arguments, result, builtin.ResolveOptions{}, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolution=%v", err)
	}
}

func TestTypeCatalogPreservesArrayDelimiters(t *testing.T) {
	for _, id := range []model.TypeID{603, 1020} {
		info, found := builtin.LookupType(id)
		if !found || info.Delimiter != ';' {
			t.Fatalf("box delimiter metadata: %+v, %v", info, found)
		}
	}
	info, found := builtin.LookupType(23)
	if !found || info.Delimiter != ',' {
		t.Fatalf("integer delimiter metadata: %+v, %v", info, found)
	}
}
