package expr_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (f *fixture) minMax(kind ir.ScalarKind, comparator string, args ...ir.ScalarID) ir.ScalarID {
	f.t.Helper()
	var selected builtin.Signature
	for sig := range builtin.SignaturesNamed(comparator) {
		selected = sig
	}
	if selected.ID == 0 {
		f.t.Fatalf("missing comparator %s", comparator)
	}
	layout := f.nodes[args[0]].Layout
	effects := ir.Effects{MayError: true}
	signature, err := f.b.AddSignature(f.t.Context(), ir.SignatureSpec{
		Signature:  ir.Signature{Function: selected.ID, Result: f.layouts[16], Strict: selected.Strict, Nullable: true, Effects: effects},
		Parameters: []ir.LayoutID{layout, layout},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	nullable := true
	for _, id := range args {
		nullable = nullable && f.nodes[id].Nullable
	}
	return f.add(ir.Scalar{Kind: kind, Layout: layout, Signature: signature, Function: selected.ID, Strict: selected.Strict, Nullable: nullable, Effects: effects}, args, model.Value{})
}

func TestMinMaxEvaluatesEveryVolatileArgumentOnlyOnActiveLanes(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		for _, scenario := range []struct {
			kind       ir.ScalarKind
			comparator string
			want       int64
		}{{ir.Least, "int8lt", 1}, {ir.Greatest, "int8gt", 3}} {
			f := newFixture(t)
			zero := f.constant(20, model.Int64Val(0))
			active := f.call("int8eq", 16, f.x, zero)
			volatile := f.function(900000, 20, false, ir.Effects{Volatility: ir.Volatile})
			minmax := f.minMax(scenario.kind, scenario.comparator, volatile, volatile, volatile)
			root := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{active, minmax, zero}, model.Value{})
			calls := int64(0)
			options := f.options()
			options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
				return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					calls++
					return model.Int64Val(calls), nil
				}, nil
			})
			input, out := batch(3), columnar.Batch{}
			if err := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options)).Eval(t.Context(), &input, nil, &out, mode); err != nil {
				t.Fatal(err)
			}
			if calls != 3 {
				t.Fatalf("mode %d kind %d: volatile calls=%d, want 3", mode, scenario.kind, calls)
			}
			requireInt(t, value(t, &out, 0, 0), scenario.want, false)
			requireInt(t, value(t, &out, 0, 1), 0, false)
			requireInt(t, value(t, &out, 0, 2), 0, false)
		}
	}
}

func TestMinMaxBorrowsSelectedPayloadUntilPublication(t *testing.T) {
	f := newFixture(t)
	payload := strings.Repeat("z", 64<<10)
	left := f.collate(f.constant(25, model.StringVal(payload)), 950)
	right := f.collate(f.constant(25, model.StringVal("a")), 950)
	root := f.minMax(ir.Greatest, "text_gt", left, right)
	options := f.options()
	options.Limits.MaxStateBytes = 32 << 10
	program := compile(t, f.graph(), []ir.ScalarID{root}, options)
	s, input, out := state(t, program), batch(1), columnar.Batch{}
	if err := s.Eval(t.Context(), &input, nil, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	retained, err := expr.Retain(t.Context(), &out, program.OutputLayouts(), expr.RetainOptions{MaxRows: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := retained.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var held columnar.Batch
	if err := retained.Publish(&held); err != nil {
		t.Fatal(err)
	}
	if got := value(t, &held, 0, 0); got.IsNull || got.StrVal != payload {
		t.Fatal("retained min/max value changed after producer close")
	}
}

func TestMinMaxCancellationDuringComparisonPublishesNothing(t *testing.T) {
	f := newFixture(t)
	left, right := f.constant(20, model.Int64Val(1)), f.constant(20, model.Int64Val(2))
	comparison := f.function(900001, 16, true, ir.Effects{MayError: true}, left, right)
	meta := f.nodes[comparison]
	meta.Kind, meta.Layout, meta.Nullable = ir.Greatest, f.layouts[20], false
	root := f.add(meta, []ir.ScalarID{left, right}, model.Value{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	options := f.options()
	options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			cancel()
			return model.BoolVal(true), nil
		}, nil
	})
	input, out := batch(1), columnar.Batch{}
	err := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options)).Eval(ctx, &input, nil, &out, expr.Direct)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("comparison cancellation=%v", err)
	}
	if out.Rows() != 0 || len(out.Columns) != 0 {
		t.Fatal("canceled min/max published output")
	}
}
