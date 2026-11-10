package normalize_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestNormalizationDistinguishesArrayValueFromVariadicSpread(t *testing.T) {
	for _, scenario := range []struct {
		name, sql        string
		ordinary, spread string
	}{
		{"concat", "SELECT concat(ARRAY[1,2]), concat(VARIADIC ARRAY[1,2])", "{1,2}", "12"},
		{"concat separator", "SELECT concat_ws(',',ARRAY[1,2]), concat_ws(',',VARIADIC ARRAY[1,2])", "{1,2}", "1,2"},
		{"format", "SELECT format('%s',ARRAY[1,2]), format('%s',VARIADIC ARRAY[1,2])", "{1,2}", "1"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := normalized(t, bound(t, scenario.sql))
			roots := make([]ir.ScalarID, 2)
			for i := range roots {
				binding, ok := graph.Binding(graph.Root(), uint32(i))
				if !ok {
					t.Fatal("missing projected call")
				}
				roots[i] = binding.Expr
				call, _ := graph.Scalar(binding.Expr)
				signature, ok := graph.Signature(call.Signature)
				if !ok || signature.VariadicArray != (i == 1) {
					t.Fatalf("call %d signature = %+v", i, signature)
				}
				layout, _ := graph.SignatureParameter(call.Signature, signature.ParameterCount()-1)
				element, ok := graph.LayoutChild(layout, 0)
				typ, _ := graph.Type(element)
				if !ok || typ.ID != 23 {
					t.Fatalf("call %d lost array element type: %+v", i, typ)
				}
			}
			values, err := evaluate(t, graph, roots, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			for i, expected := range []string{scenario.ordinary, scenario.spread} {
				if got := values[i][0]; got.IsNull || got.StrVal != expected {
					t.Fatalf("call %d = %+v, want %q", i, got, expected)
				}
			}
		})
	}
}

func TestNormalizationPreservesNullArrayVersusNullValue(t *testing.T) {
	graph := normalized(t, bound(t, "SELECT concat(NULL), concat(NULL::text[]), concat(VARIADIC NULL::text[]), concat(VARIADIC ARRAY[]::text[])"))
	roots := make([]ir.ScalarID, 4)
	for i := range roots {
		binding, _ := graph.Binding(graph.Root(), uint32(i))
		roots[i] = binding.Expr
	}
	values, err := evaluate(t, graph, roots, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range roots {
		got := values[i][0]
		if got.IsNull != (i == 2) || !got.IsNull && got.StrVal != "" {
			t.Fatalf("call %d = %+v, want NULL only for explicit NULL array", i, got)
		}
	}
}
