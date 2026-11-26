package expr_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestSubqueryAdmissionFailsClosed(t *testing.T) {
	f := newFixture(t)
	root := scalarSubquery(t, f, f.layouts[20], model.TypeInt64, f.slot)
	graph := f.graph()
	for _, scenario := range []struct {
		name        string
		descriptors []expr.Subquery
		want        error
	}{
		{name: "missing", want: expr.ErrSubquery},
		{name: "missing capture", descriptors: []expr.Subquery{{Scalar: root, ID: 1}}, want: expr.ErrInvalid},
		{name: "wrong capture", descriptors: []expr.Subquery{{Scalar: root, ID: 1, Captures: []model.SlotID{0}}}, want: expr.ErrInvalid},
		{name: "duplicate", descriptors: []expr.Subquery{{Scalar: root, ID: 1, Captures: []model.SlotID{f.slot}}, {Scalar: root, ID: 2, Captures: []model.SlotID{f.slot}}}, want: expr.ErrInvalid},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			options := f.options()
			options.Subqueries = scenario.descriptors
			if _, err := expr.Compile(t.Context(), graph, []ir.ScalarID{root}, options); !errors.Is(err, scenario.want) {
				t.Fatalf("compilation error = %v, want %v", err, scenario.want)
			}
		})
	}
	options := f.options()
	options.Subqueries = []expr.Subquery{{Scalar: root, ID: 1, Captures: []model.SlotID{f.slot}}}
	program := compile(t, graph, []ir.ScalarID{root}, options)
	if _, err := expr.NewState(program, expr.StateOptions{}); !errors.Is(err, expr.ErrInvalid) {
		t.Fatalf("missing runtime = %v", err)
	}
}
