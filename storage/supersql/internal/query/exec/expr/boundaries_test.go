package expr_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestSubqueryRequiresPhysicalLowering(t *testing.T) {
	f := newFixture(t)
	child, err := f.b.AddScope(t.Context(), f.scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: child, Layout: f.layouts[20]})
	if err != nil {
		t.Fatal(err)
	}
	one, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: child, Layout: f.layouts[20]}, Value: model.Int64Val(1)})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := f.b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: child, Rows: 1}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{one}})
	if err != nil {
		t.Fatal(err)
	}
	root := f.add(ir.Scalar{Kind: ir.Subquery, Layout: f.layouts[20], Nullable: true, Effects: ir.Effects{MayError: true}, SubqueryKind: ir.ScalarSubquery, Relation: rel}, nil, model.Value{})
	_, err = expr.Compile(t.Context(), f.graph(), []ir.ScalarID{root}, f.options())
	if !errors.Is(err, expr.ErrSubquery) {
		t.Fatalf("unlowered subquery=%v", err)
	}
}

func TestNullTestsAndNestedMasks(t *testing.T) {
	f := newFixture(t)
	isNull := f.add(ir.Scalar{Kind: ir.NullTest, Layout: f.layouts[16]}, []ir.ScalarID{f.x}, model.Value{})
	isNotNull := f.add(ir.Scalar{Kind: ir.NullTest, Layout: f.layouts[16], Negated: true}, []ir.ScalarID{f.x}, model.Value{})
	zero, ten := f.constant(20, model.Int64Val(0)), f.constant(20, model.Int64Val(10))
	eqZero := f.call("int8eq", 16, f.x, zero)
	div := f.call("int8div", 20, ten, f.x)
	inner := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{eqZero, zero, div}, model.Value{})
	outer := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{isNull, ten, inner}, model.Value{})
	p := compile(t, f.graph(), []ir.ScalarID{isNull, isNotNull, outer}, f.options())
	input, out := batch(17), columnar.Batch{}
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		if err := state(t, p).Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		for row := range input.Rows() {
			wantNull := int64(0)
			if row%3 == 2 {
				wantNull = 1
			}
			requireInt(t, value(t, &out, 0, row), wantNull, false)
			requireInt(t, value(t, &out, 1, row), 1-wantNull, false)
			want := []int64{0, 5, 10}[row%3]
			requireInt(t, value(t, &out, 2, row), want, false)
		}
	}
}

func TestCustomResultCannotExceedStateBudget(t *testing.T) {
	f := newFixture(t)
	root := f.function(900000, 25, false, ir.Effects{Volatility: ir.Volatile})
	options := f.options()
	options.Limits.MaxStateBytes = 4096
	options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
		return func(_ context.Context, _ []model.Value, budget builtin.CallBudget) (model.Value, error) {
			return model.StringVal(string(make([]byte, budget.OutputBytes+1))), nil
		}, nil
	})
	s := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options))
	input, out := batch(1), columnar.Batch{}
	err := s.Eval(t.Context(), &input, nil, &out, expr.Direct)
	if !errors.Is(err, expr.ErrLimit) {
		t.Fatalf("custom result byte bound=%v", err)
	}
	if len(out.Columns) != 0 {
		t.Fatal("oversized custom result published")
	}
}

func TestRetainedResultSurvivesNextEvaluation(t *testing.T) {
	f := newFixture(t)
	param := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[25], Parameter: 1, Nullable: true}, nil, model.Value{})
	options := f.options()
	layout := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString}}
	options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
	p := compile(t, f.graph(), []ir.ScalarID{param}, options)
	s, input, out := state(t, p), batch(1), columnar.Batch{}
	params := []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.StringVal("first")}}
	if err := s.Eval(t.Context(), &input, params, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	retained, err := expr.Retain(t.Context(), &out, p.OutputLayouts(), expr.RetainOptions{MaxRows: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := retained.Close(); err != nil {
			t.Error(err)
		}
	})
	params[0].Value = model.StringVal("second")
	if err := s.Eval(t.Context(), &input, params, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	if got := value(t, &out, 0, 0).StrVal; got != "second" {
		t.Fatalf("new output=%q", got)
	}
	var held columnar.Batch
	if err := retained.Publish(&held); err != nil {
		t.Fatal(err)
	}
	if got := value(t, &held, 0, 0).StrVal; got != "first" {
		t.Fatalf("retained output=%q", got)
	}
}

func TestOperatorIdentityUsesBuiltinKernel(t *testing.T) {
	f := newFixture(t)
	one := f.constant(20, model.Int64Val(1))
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: 684, Result: f.layouts[20], Strict: true, Nullable: true, Effects: ir.Effects{MayError: true}}, Parameters: []ir.LayoutID{f.layouts[20], f.layouts[20]}})
	if err != nil {
		t.Fatal(err)
	}
	root := f.add(ir.Scalar{Kind: ir.Call, Layout: f.layouts[20], Operator: 684, Signature: signature, Strict: true, Nullable: true, Effects: ir.Effects{MayError: true}}, []ir.ScalarID{f.x, one}, model.Value{})
	p := compile(t, f.graph(), []ir.ScalarID{root}, f.options())
	input, out := batch(17), columnar.Batch{}
	if err := state(t, p).Eval(t.Context(), &input, nil, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	for row := range input.Rows() {
		want := int64(1)
		if row%3 == 1 {
			want = 3
		}
		requireInt(t, value(t, &out, 0, row), want, row%3 == 2)
	}
}

func TestCompositeFieldEvaluatesParentOnce(t *testing.T) {
	f := newFixture(t)
	layout := model.TypeLayout{Type: model.Type{ID: 900001, Storage: model.TypeComposite}, Children: []model.TypeLayout{{Type: model.Type{ID: 25, Storage: model.TypeString}}, {Type: model.Type{ID: 20, Storage: model.TypeInt64}}}}
	rowLayout, err := f.b.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	f.layouts[900001] = rowLayout
	parent := f.function(900000, 900001, false, ir.Effects{Volatility: ir.Volatile}, f.x)
	field := f.add(ir.Scalar{Kind: ir.Field, Layout: f.layouts[20], FieldIndex: 1, Nullable: true}, []ir.ScalarID{parent}, model.Value{})
	calls := 0
	options := f.options()
	options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
		return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
			calls++
			if args[0].IsNull {
				return model.NullVal(model.TypeComposite), nil
			}
			return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal("unselected"), model.Int64Val(args[0].IntVal + 7)}}, nil
		}, nil
	})
	p := compile(t, f.graph(), []ir.ScalarID{field}, options)
	input, out := batch(17), columnar.Batch{}
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		calls = 0
		if err := state(t, p).Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		if calls != 17 {
			t.Fatalf("parent calls=%d want 17", calls)
		}
		for row := range input.Rows() {
			want := int64(7)
			if row%3 == 1 {
				want = 9
			}
			requireInt(t, value(t, &out, 0, row), want, row%3 == 2)
		}
	}
}
