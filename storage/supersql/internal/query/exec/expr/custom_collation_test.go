package expr_test

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestCustomCallableUsesBoundInputCollation(t *testing.T) {
	f := newFixture(t)
	left := f.collate(f.constant(25, model.StringVal("Z")), 950)
	right := f.collate(f.constant(25, model.StringVal("a")), 951)
	effects := ir.Effects{MayError: true}
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
		Signature:  ir.Signature{Function: 900051, InputCollation: 951, Result: f.layouts[16], Strict: true, Nullable: true, Effects: effects},
		Parameters: []ir.LayoutID{f.nodes[left].Layout, f.nodes[right].Layout},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := f.add(ir.Scalar{Kind: ir.Call, Function: 900051, Signature: signature, Layout: f.layouts[16], Strict: true, Nullable: true, Effects: effects}, []ir.ScalarID{left, right}, model.Value{})
	options := f.options()
	options.Functions = resolverFunc(func(function expr.Function) (expr.ScalarCall, error) {
		comparison, err := builtin.Resolve(740, builtin.ResolveOptions{Collation: function.InputCollation, DefaultCollation: 950})
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, args []model.Value, budget builtin.CallBudget) (model.Value, error) {
			return comparison.Call(ctx, args, budget)
		}, nil
	})
	program := compile(t, f.graph(), []ir.ScalarID{root}, options)
	input, output := batch(1), columnar.Batch{}
	if err := state(t, program).Eval(t.Context(), &input, nil, &output, expr.Direct); err != nil {
		t.Fatal(err)
	}
	requireInt(t, value(t, &output, 0, 0), 1, false)
}
