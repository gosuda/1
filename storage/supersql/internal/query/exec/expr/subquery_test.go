package expr_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type subqueryRuntimeFunc func(context.Context, expr.SubqueryID, []model.Value, func(model.Value) error) error

func (f subqueryRuntimeFunc) EvaluateScalar(ctx context.Context, id expr.SubqueryID, captures []model.Value, consume func(expr.SubqueryRow) error) error {
	return f(ctx, id, captures, func(value model.Value) error {
		return consume(expr.SubqueryRow{Values: []model.Value{value}, Present: true})
	})
}

func (f subqueryRuntimeFunc) EvaluateRows(context.Context, expr.SubqueryID, []model.Value, func(expr.SubqueryRow) (bool, error)) error {
	return errors.New("unexpected quantified subquery")
}

func scalarSubquery(t *testing.T, f *fixture, layout ir.LayoutID, storage model.DataType, captures ...model.SlotID) ir.ScalarID {
	t.Helper()
	scope, err := f.b.AddScope(t.Context(), f.scope, captures)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	null, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: scope, Layout: layout, Nullable: true}, Value: model.NullVal(storage)})
	if err != nil {
		t.Fatal(err)
	}
	body, err := f.b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope, Rows: 1}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{null}})
	if err != nil {
		t.Fatal(err)
	}
	return f.add(ir.Scalar{Kind: ir.Subquery, SubqueryKind: ir.ScalarSubquery, Relation: body, Layout: layout, Nullable: true, Effects: ir.Effects{MayError: true}}, nil, model.Value{})
}

func TestSubqueryCaptureFollowsSelectedCaseMask(t *testing.T) {
	f := newFixture(t)
	subquery := scalarSubquery(t, f, f.layouts[20], model.TypeInt64, f.slot)
	zero := f.constant(20, model.Int64Val(0))
	condition := f.call("int8eq", 16, f.x, zero)
	root := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{condition, zero, subquery}, model.Value{})
	options := f.options()
	options.Subqueries = []expr.Subquery{{Scalar: subquery, ID: 1, Captures: []model.SlotID{f.slot}}}
	program := compile(t, f.graph(), []ir.ScalarID{root}, options)
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		s := captureState(t, program, expr.StateOptions{Subqueries: subqueryRuntimeFunc(func(_ context.Context, _ expr.SubqueryID, captures []model.Value, consume func(model.Value) error) error {
			if !captures[0].IsNull && captures[0].IntVal == 0 {
				return errInactiveOperand
			}
			if captures[0].IsNull {
				return consume(model.Int64Val(99))
			}
			return consume(model.Int64Val(captures[0].IntVal + 10))
		})})
		input := columnar.Batch{Length: 4, Selection: []uint32{3, 1, 0}, Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 4, Int64: []int64{7, 0, -1, 0}, Nulls: []byte{8}}}}
		var out columnar.Batch
		if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		for lane, want := range []int64{99, 0, 17} {
			requireInt(t, value(t, &out, 0, lane), want, false)
		}
		input.Selection = []uint32{0}
		row := make([]model.Value, 1)
		err := s.EvalBorrowedRow(t.Context(), &input, nil, row, mode)
		if err != nil {
			t.Fatal(err)
		}
		requireInt(t, row[0], 17, false)
	}
}

func TestSubqueryRejectsBrokenConsumptionWithoutPublishing(t *testing.T) {
	for _, scenario := range []string{"missing", "duplicate", "ignored type error", "ignored cancellation", "cleanup failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			root := scalarSubquery(t, f, f.layouts[20], model.TypeInt64)
			options := f.options()
			options.Subqueries = []expr.Subquery{{Scalar: root, ID: 1}}
			program := compile(t, f.graph(), []ir.ScalarID{root}, options)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := expr.ErrInvalid
			if scenario == "ignored cancellation" {
				want = context.Canceled
			}
			if scenario == "cleanup failure" {
				want = errInactiveOperand
			}
			s := captureState(t, program, expr.StateOptions{Subqueries: subqueryRuntimeFunc(func(_ context.Context, _ expr.SubqueryID, _ []model.Value, consume func(model.Value) error) error {
				switch scenario {
				case "duplicate":
					_ = consume(model.Int64Val(7))
					_ = consume(model.Int64Val(8))
				case "ignored type error":
					_ = consume(model.StringVal("wrong"))
				case "ignored cancellation":
					cancel()
					_ = consume(model.Int64Val(7))
				case "cleanup failure":
					_ = consume(model.Int64Val(7))
					return errInactiveOperand
				}
				return nil
			})})
			input, out := batch(1), columnar.Batch{}
			if err := s.Eval(ctx, &input, nil, &out, expr.Direct); !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			if out.Rows() != 0 || len(out.Columns) != 0 {
				t.Fatal("failed scalar subquery published output")
			}
		})
	}
}

func TestSubqueryCopiesBeforeSourceReleaseWithoutCallbackReservation(t *testing.T) {
	q := allocationQuery(t)
	f := newFixture(t)
	layout := model.TypeLayout{Type: model.Type{ID: 1009, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 25, Storage: model.TypeString}}}}
	id, err := f.b.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	root := scalarSubquery(t, f, id, model.TypeArray)
	options := f.options()
	options.Subqueries = []expr.Subquery{{Scalar: root, ID: 1}}
	program := compile(t, f.graph(), []ir.ScalarID{root}, options)
	blocker, err := q.TryReserve(runtime.WorkMemory, q.Usage().Work.Limit-8192)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	s := captureState(t, program, expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory), Subqueries: subqueryRuntimeFunc(func(_ context.Context, _ expr.SubqueryID, _ []model.Value, consume func(model.Value) error) error {
		claim, err := q.TryReserve(runtime.WorkMemory, 1024)
		if err != nil {
			return err
		}
		defer claim.Release()
		backing := []model.Value{model.StringVal("still alive")}
		value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -2}}, ArrayVal: backing}
		err = consume(value)
		backing[0] = model.StringVal("released")
		value.Dimensions[0].LowerBound = 1
		return err
	})})
	input := batch(1)
	row := make([]model.Value, 1)
	err = s.EvalBorrowedRow(t.Context(), &input, nil, row, expr.Direct)
	if err != nil {
		t.Fatal(err)
	}
	if row[0].ArrayVal[0].StrVal != "still alive" || row[0].Dimensions[0].LowerBound != -2 {
		t.Fatalf("result references released subquery storage: %+v", row[0])
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	blocker.Release()
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("subquery retained work allocations: %+v", usage)
	}
}

type quantifiedRuntimeFunc func(context.Context, expr.SubqueryID, []model.Value, func(model.Value) (bool, error)) error

func (f quantifiedRuntimeFunc) EvaluateScalar(context.Context, expr.SubqueryID, []model.Value, func(expr.SubqueryRow) error) error {
	return errors.New("unexpected scalar subquery")
}

func (f quantifiedRuntimeFunc) EvaluateRows(ctx context.Context, id expr.SubqueryID, captures []model.Value, visit func(expr.SubqueryRow) (bool, error)) error {
	return f(ctx, id, captures, func(value model.Value) (bool, error) {
		return visit(expr.SubqueryRow{Values: []model.Value{value}, Present: true})
	})
}

func TestQuantifiedLeftEffectsFollowStreamDemand(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		for _, empty := range []bool{false, true} {
			f := newFixture(t)
			scalar := scalarSubquery(t, f, f.layouts[20], model.TypeInt64)
			body := f.nodes[scalar].Relation
			left := f.function(900000, 20, false, ir.Effects{Volatility: ir.Volatile})
			var equality model.OperatorID
			for operator := range builtin.OperatorsNamed("=") {
				if operator.Left == 20 && operator.Right == 20 {
					equality = operator.ID
					break
				}
			}
			signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
				Signature: ir.Signature{Operator: equality, Result: f.layouts[16], Strict: true, Nullable: true},
				Parameters: []ir.LayoutID{f.layouts[20], f.layouts[20]},
			})
			if err != nil {
				t.Fatal(err)
			}
			node := ir.Scalar{Kind: ir.Subquery, Scope: f.scope, SubqueryKind: ir.AnySubquery, Relation: body, Layout: f.layouts[16], Nullable: true, Effects: f.nodes[left].Effects}
			root, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: node, Operands: []ir.ScalarID{left}, Comparisons: []ir.SignatureID{signature}})
			if err != nil {
				t.Fatal(err)
			}
			f.nodes[root] = node
			options := f.options()
			options.Subqueries = []expr.Subquery{{Scalar: root, ID: 1}}
			calls := int64(0)
			options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
				return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					calls++
					return model.Int64Val(calls), nil
				}, nil
			})
			p := compile(t, f.graph(), []ir.ScalarID{root}, options)
			s := captureState(t, p, expr.StateOptions{Subqueries: quantifiedRuntimeFunc(func(_ context.Context, _ expr.SubqueryID, _ []model.Value, visit func(model.Value) (bool, error)) error {
				if empty {
					return nil
				}
				for range 3 {
					more, err := visit(model.Int64Val(2))
					if err != nil || !more {
						return err
					}
				}
				return nil
			})})
			input := batch(1)
			var out [1]model.Value
			if err := s.EvalBorrowedRow(t.Context(), &input, nil, out[:], mode); err != nil {
				t.Fatal(err)
			}
			wantCalls := int64(2)
			if empty {
				wantCalls = 0
			}
			if calls != wantCalls || out[0].IsNull || (out[0].IntVal != 0) != !empty {
				t.Fatalf("empty=%v calls=%d result=%v", empty, calls, out[0])
			}
		}
	}
}
