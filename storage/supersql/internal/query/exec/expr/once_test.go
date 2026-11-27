package expr_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var errInactiveOperand = errors.New("inactive operand evaluated")

func TestOnceComputesDisjointMasksAndResetsPerInvocation(t *testing.T) {
	for _, rows := range []int{1, 17, 1024} {
		for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
			t.Run(fmt.Sprintf("rows%d/mode%d", rows, mode), func(t *testing.T) {
				f := newFixture(t)
				zero := f.constant(20, model.Int64Val(0))
				condition := f.call("int8eq", 16, f.x, zero)
				call := f.function(900000, 20, false, ir.Effects{Volatility: ir.Volatile}, f.x)
				once := f.add(ir.Scalar{Kind: ir.Once, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{call}, model.Value{})
				first := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{condition, once, zero}, model.Value{})
				second := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{condition, zero, once}, model.Value{})
				distinct := f.function(900000, 20, false, ir.Effects{Volatility: ir.Volatile}, f.x)
				calls := 0
				options := f.options()
				options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
					return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
						calls++
						if args[0].IsNull {
							return model.NullVal(model.TypeInt64), nil
						}
						return model.Int64Val(int64(calls)), nil
					}, nil
				})
				// Reaching the ordinary call again must not reuse Once's value.
				p := compile(t, f.graph(), []ir.ScalarID{first, second, once, call, distinct}, options)
				s, input, out := state(t, p), batch(rows), columnar.Batch{}
				for invocation := 1; invocation <= 2; invocation++ {
					if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
						t.Fatal(err)
					}
					if calls != invocation*3*rows {
						t.Fatalf("calls=%d want %d", calls, invocation*3*rows)
					}
					for lane := range rows {
						column := 1
						if lane%3 == 0 {
							column = 0
						}
						cached := value(t, &out, 2, lane)
						requireInt(t, value(t, &out, column, lane), cached.IntVal, cached.IsNull)
						if lane%3 != 2 {
							requireInt(t, value(t, &out, 3, lane), int64((invocation-1)*3*rows+rows+lane+1), false)
							requireInt(t, value(t, &out, 4, lane), int64((invocation-1)*3*rows+2*rows+lane+1), false)
						}
					}
				}
			})
		}
	}
}

func TestOnceRemainsInsideShortCircuitMasks(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			f := newFixture(t)
			bad := f.function(900000, 20, false, ir.Effects{Volatility: ir.Volatile, MayError: true})
			once := f.add(ir.Scalar{Kind: ir.Once, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{bad}, model.Value{})
			zero := f.constant(20, model.Int64Val(0))
			truth, falsity := f.constant(16, model.BoolVal(true)), f.constant(16, model.BoolVal(false))
			nested := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{truth, once, zero}, model.Value{})
			outer := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{falsity, nested, zero}, model.Value{})
			coalesce := f.add(ir.Scalar{Kind: ir.Coalesce, Layout: f.layouts[20]}, []ir.ScalarID{zero, once}, model.Value{})
			comparison := f.call("int8eq", 16, once, zero)
			and := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.And, Layout: f.layouts[16], Nullable: true}, []ir.ScalarID{falsity, comparison}, model.Value{})
			or := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.Or, Layout: f.layouts[16], Nullable: true}, []ir.ScalarID{truth, comparison}, model.Value{})
			calls := 0
			options := f.options()
			options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
				return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					calls++
					return model.Value{}, errInactiveOperand
				}, nil
			})
			p := compile(t, f.graph(), []ir.ScalarID{outer, coalesce, and, or}, options)
			input, out := batch(17), columnar.Batch{}
			if err := state(t, p).Eval(t.Context(), &input, nil, &out, mode); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("inactive calls=%d", calls)
			}
			for lane := range input.Rows() {
				for column, want := range []int64{0, 0, 0, 1} {
					requireInt(t, value(t, &out, column, lane), want, false)
				}
			}
		})
	}
}

func TestOnceLimitsAndCancellation(t *testing.T) {
	f := newFixture(t)
	call := f.function(900000, 20, false, ir.Effects{Volatility: ir.Volatile})
	once := f.add(ir.Scalar{Kind: ir.Once, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{call}, model.Value{})
	graph, options := f.graph(), f.options()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			calls++
			cancel()
			return model.Int64Val(1), nil
		}, nil
	})
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		ctx, cancel = context.WithCancel(t.Context())
		calls = 0
		input, out := batch(17), columnar.Batch{}
		s := state(t, compile(t, graph, []ir.ScalarID{once, once}, options))
		if err := s.Eval(ctx, &input, nil, &out, mode); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation=%v", err)
		}
		cancel()
		if calls != 1 || len(out.Columns) != 0 {
			t.Fatalf("calls=%d output columns=%d", calls, len(out.Columns))
		}
	}
	for _, resource := range []string{"instructions", "registers", "bytes", "depth"} {
		bounded := options
		switch resource {
		case "instructions":
			bounded.Limits.MaxInstructions = 8
		case "registers":
			bounded.Limits.MaxRegisters = 3
		case "bytes":
			bounded.Limits.MaxBytes = 1
		case "depth":
			bounded.Limits.MaxDepth = 1
		}
		if _, err := expr.Compile(t.Context(), graph, []ir.ScalarID{once, once}, bounded); !errors.Is(err, expr.ErrLimit) {
			t.Fatalf("%s budget=%v", resource, err)
		}
	}
	options.Limits.MaxStateBytes = 1
	input, out := batch(1024), columnar.Batch{}
	if err := state(t, compile(t, graph, []ir.ScalarID{once}, options)).Eval(t.Context(), &input, nil, &out, expr.Direct); !errors.Is(err, expr.ErrLimit) {
		t.Fatalf("cache state budget=%v", err)
	}
}
