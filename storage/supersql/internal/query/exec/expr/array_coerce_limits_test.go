package expr_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestArrayCoerceTemplateSharesProgramLimits(t *testing.T) {
	f := newFixture(t)
	f.coercionType(model.Type{ID: 23, Storage: model.TypeInt64})
	f.coercionType(model.Type{ID: 1700, Storage: model.TypeNumeric})
	f.arrayLayout(1007, model.Type{ID: 23, Storage: model.TypeInt64})
	f.arrayLayout(1231, model.Type{ID: 1700, Storage: model.TypeNumeric})
	source := f.constant(1007, stackArray([]model.ArrayDimension{{LowerBound: 1, Length: 1}}, model.Int64Val(7)))
	widen := f.call("int8", 20, f.arrayElement(23))
	template := f.call("numeric", 1700, widen)
	root := f.arrayCoerce(source, 1231, template)
	graph := f.graph()
	for _, tc := range []struct {
		name                         string
		instructions, depth, outputs int
	}{
		{"template instructions", 4, 64, 1},
		{"combined templates", 9, 64, 2},
		{"template depth", 100, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := f.options()
			options.Limits.MaxInstructions = tc.instructions
			options.Limits.MaxDepth = tc.depth
			roots := make([]ir.ScalarID, tc.outputs)
			for i := range roots {
				roots[i] = root
			}
			_, err := expr.Compile(t.Context(), graph, roots, options)
			if !errors.Is(err, expr.ErrLimit) {
				t.Fatalf("array template escaped %s limit: %v", tc.name, err)
			}
		})
	}
}
