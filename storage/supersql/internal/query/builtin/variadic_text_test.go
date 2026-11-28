package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestVariadicCallableUsesActualArgumentWidth(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	arguments := make([]model.TypeLayout, builtin.MaxCallArguments)
	types := make([]model.Type, len(arguments))
	values := make([]model.Value, len(arguments))
	for i := range arguments {
		arguments[i] = text
		types[i] = text.Type
		values[i] = model.StringVal("a")
	}
	function, err := builtin.ResolveTyped(t.Context(), 3058, arguments, text, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if function.ArgumentCount() != len(values) {
		t.Fatalf("argument count = %d, want %d", function.ArgumentCount(), len(values))
	}
	if err := function.ValidateTypes(types, text.Type); err != nil {
		t.Fatal(err)
	}
	got, err := function.Call(t.Context(), values, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || got.StrVal != strings.Repeat("a", builtin.MaxCallArguments) {
		t.Fatalf("wide concat = %q, %v", got.StrVal, err)
	}
	types[len(types)-1].ID = 1043
	requireSQLState(t, function.ValidateTypes(types, text.Type), "42804")
	values[len(values)-1] = model.Int64Val(1)
	_, err = function.Call(t.Context(), values, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "42804")
	_, err = function.Call(t.Context(), values[:len(values)-1], builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "42883")
	_, err = builtin.ResolveTyped(t.Context(), 3058, append(arguments, text), text, builtin.ResolveOptions{}, 0)
	requireSQLState(t, err, "42883")
}

func TestVariadicArrayModeDoesNotChangeScalarArrayCalls(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	array := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	arguments := []model.TypeLayout{array}
	scalar, err := builtin.ResolveTyped(t.Context(), 3058, arguments, text, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := builtin.ResolveVariadic(t.Context(), 3058, arguments, text, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	arguments[0].Type.ID = 1009
	arguments[0].Children[0].Type = text.Type
	value := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2)}, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}}
	for _, tc := range []struct {
		name     string
		function builtin.Callable
		want     string
	}{
		{"scalar array", scalar, "{1,2}"},
		{"explicit variadic array", expanded, "12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.function.Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil || got.IsNull || got.StrVal != tc.want {
				t.Fatalf("concat after layout mutation = %q, %v; want %q", got.StrVal, err, tc.want)
			}
		})
	}
}

func TestFormatDefersUnselectedOutputErrors(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	uncatalogued := model.TypeLayout{Type: model.Type{ID: 900001, Modifier: -1, Storage: model.TypeString}}
	function, err := builtin.ResolveTyped(t.Context(), 3539, []model.TypeLayout{text, uncatalogued, text}, text, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, format, want, code string
		value                    model.Value
	}{
		{name: "unreferenced", format: "%2$s", value: model.StringVal("bad"), want: "chosen"},
		{name: "referenced", format: "%1$s", value: model.StringVal("bad"), code: "42804"},
		{name: "null string", format: "%1$s", value: model.NullVal(model.TypeString), want: ""},
		{name: "null literal", format: "%1$L", value: model.NullVal(model.TypeString), want: "NULL"},
		{name: "null identifier", format: "%1$I", value: model.NullVal(model.TypeString), code: "22004"},
		{name: "syntax before output", format: "%1$q", value: model.StringVal("bad"), code: "22023"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.format), tc.value, model.StringVal("chosen")}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if tc.code != "" {
				requireSQLState(t, err, tc.code)
				return
			}
			if err != nil || got.IsNull || got.StrVal != tc.want {
				t.Fatalf("format = %q, %v; want %q", got.StrVal, err, tc.want)
			}
		})
	}
	_, err = builtin.ResolveTyped(t.Context(), 3058, []model.TypeLayout{uncatalogued}, text, builtin.ResolveOptions{}, 1<<20)
	requireSQLState(t, err, "42804")
}

func TestVariadicMetadataBudgetIncludesNestedOutputCodecs(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	array := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	arguments := []model.TypeLayout{array, array}
	function, err := builtin.ResolveTyped(t.Context(), 3058, arguments, text, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, err = builtin.ResolveTyped(t.Context(), 3058, arguments, text, builtin.ResolveOptions{}, function.OwnedBytes()-1)
	requireSQLState(t, err, "54000")
	bounded, err := builtin.ResolveTyped(t.Context(), 3058, arguments, text, builtin.ResolveOptions{}, function.OwnedBytes())
	if err != nil {
		t.Fatal(err)
	}
	value := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(7)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}}
	got, err := bounded.Call(t.Context(), []model.Value{value, value}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || got.StrVal != "{7}{7}" {
		t.Fatalf("budgeted nested output = %q, %v", got.StrVal, err)
	}
}

func TestVariadicResolutionRequiresConcreteInputs(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	for _, id := range []model.FunctionID{3058, 3059, 3539} {
		_, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if !errors.Is(err, builtin.ErrTypeBindingRequired) {
			t.Fatalf("Resolve(%d) = %v, want concrete binding requirement", id, err)
		}
	}
	_, err := builtin.ResolveVariadic(t.Context(), 3540, []model.TypeLayout{text}, text, builtin.ResolveOptions{}, 1<<20)
	requireSQLState(t, err, "42883")
	_, err = builtin.ResolveVariadic(t.Context(), 3058, []model.TypeLayout{text}, text, builtin.ResolveOptions{}, 1<<20)
	requireSQLState(t, err, "42804")
	unknown := text
	unknown.Type.ID = 2276
	_, err = builtin.ResolveTyped(t.Context(), 3058, []model.TypeLayout{unknown}, text, builtin.ResolveOptions{}, 1<<20)
	requireSQLState(t, err, "42P18")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = builtin.ResolveVariadic(ctx, 3058, nil, text, builtin.ResolveOptions{}, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolution = %v", err)
	}
}

func TestVariadicUnknownDatumUsesItsOutputCodec(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	unknown := text
	unknown.Type.ID = 705
	function, err := builtin.ResolveTyped(t.Context(), 3058, []model.TypeLayout{unknown}, text, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	value, err := function.Call(t.Context(), []model.Value{model.StringVal("unknown datum")}, builtin.CallBudget{OutputBytes: 26, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != "unknown datum" {
		t.Fatalf("typed unknown output = %v, %v", value, err)
	}
}
