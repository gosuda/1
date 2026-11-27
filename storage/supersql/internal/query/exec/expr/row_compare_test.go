package expr_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func rowComparisonFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	for _, typ := range []model.Type{{ID: 23, Storage: model.TypeInt64, Modifier: -1}, {ID: 16, Storage: model.TypeBoolean, Modifier: -1}} {
		layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		f.layouts[typ.ID] = layout
	}
	return f
}

func (f *fixture) rowCompare(kind ir.RowComparisonKind, comparisons ...ir.ScalarID) ir.ScalarID {
	f.t.Helper()
	return f.add(ir.Scalar{Kind: ir.RowCompare, RowComparison: kind, Layout: f.layouts[16], Nullable: true, Effects: ir.Effects{MayError: true}}, comparisons, model.Value{})
}

func TestRowCompareUsesComparatorSignAndInclusiveEquality(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		for _, scenario := range []struct {
			kind ir.RowComparisonKind
			want []int64
		}{
			{ir.RowCompareLess, []int64{1, 0, 0}},
			{ir.RowCompareLessEqual, []int64{1, 1, 0}},
			{ir.RowCompareGreaterEqual, []int64{0, 1, 1}},
			{ir.RowCompareGreater, []int64{0, 0, 1}},
		} {
			f := rowComparisonFixture(t)
			zero := f.constant(20, model.Int64Val(0))
			comparison := f.call("btint8cmp", 23, f.x, zero)
			root := f.rowCompare(scenario.kind, comparison, comparison)
			input := columnar.Batch{Length: 3, Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 3, Int64: []int64{-99, 0, 99}}}}
			var out columnar.Batch
			if err := state(t, compile(t, f.graph(), []ir.ScalarID{root}, f.options())).Eval(t.Context(), &input, nil, &out, mode); err != nil {
				t.Fatal(err)
			}
			for lane, want := range scenario.want {
				requireInt(t, value(t, &out, 0, lane), want, false)
			}
		}
	}
}

func TestRowCompareDemandsLaterFieldsOnlyOnRemainingSparseLanes(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := rowComparisonFixture(t)
		zero := f.constant(20, model.Int64Val(0))
		first := f.call("btint8cmp", 23, f.x, zero)
		left := f.function(900010, 20, false, ir.Effects{Volatility: ir.Volatile}, f.x)
		right := f.function(900011, 20, false, ir.Effects{Volatility: ir.Volatile}, f.x)
		second := f.call("btint8cmp", 23, left, right)
		root := f.rowCompare(ir.RowCompareLess, first, second)
		var calls []model.FunctionID
		options := f.options()
		options.Functions = resolverFunc(func(function expr.Function) (expr.ScalarCall, error) {
			return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
				if args[0].IsNull || args[0].IntVal != 0 {
					t.Fatalf("later field reached decided lane: %+v", args[0])
				}
				calls = append(calls, function.ID)
				if function.ID == 900010 {
					return model.Int64Val(1), nil
				}
				return model.Int64Val(2), nil
			}, nil
		})
		program := compile(t, f.graph(), []ir.ScalarID{root}, options)
		s := state(t, program)
		input := columnar.Batch{Length: 6, Selection: []uint32{4, 0, 5, 2, 3}, Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 6, Offset: 1, Int64: []int64{99, 0, 77, -1, 0, 2, 0}, Nulls: []byte{1 << 6}}}}
		var out columnar.Batch
		if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		for lane, want := range []int64{0, 1, 0, 1, 1} {
			requireInt(t, value(t, &out, 0, lane), want, lane == 2)
		}
		if !slices.Equal(calls, []model.FunctionID{900010, 900010, 900011, 900011}) {
			t.Fatalf("mode %d: field effects = %v", mode, calls)
		}
		calls = nil
		input.Selection = []uint32{}
		if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 0 || out.Rows() != 0 {
			t.Fatalf("empty selection invoked fields %v or published %d rows", calls, out.Rows())
		}
	}
}

func TestRowCompareNullStopsAfterBothCurrentArguments(t *testing.T) {
	rightError := errors.New("current right operand failed")
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		for _, scenario := range []string{"strict null", "comparator null", "right error"} {
			t.Run(scenario, func(t *testing.T) {
				f := rowComparisonFixture(t)
				effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
				left := f.function(900020, 20, false, effects)
				right := f.function(900021, 20, false, effects)
				first := f.function(900022, 23, true, effects, left, right)
				later := f.function(900023, 20, false, effects)
				second := f.call("btint8cmp", 23, later, later)
				root := f.rowCompare(ir.RowCompareLess, first, second)
				var calls []model.FunctionID
				options := f.options()
				options.Functions = resolverFunc(func(function expr.Function) (expr.ScalarCall, error) {
					return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
						calls = append(calls, function.ID)
						switch function.ID {
						case 900020:
							if scenario != "comparator null" {
								return model.NullVal(model.TypeInt64), nil
							}
						case 900021:
							if scenario == "right error" {
								return model.Value{}, rightError
							}
						case 900022:
							return model.NullVal(model.TypeInt64), nil
						}
						return model.Int64Val(1), nil
					}, nil
				})
				input, out := batch(1), columnar.Batch{}
				err := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options)).Eval(t.Context(), &input, nil, &out, mode)
				if scenario == "right error" {
					if !errors.Is(err, rightError) || out.Rows() != 0 {
						t.Fatalf("right operand error=%v, published rows=%d", err, out.Rows())
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					requireInt(t, value(t, &out, 0, 0), 0, true)
				}
				want := []model.FunctionID{900020, 900021}
				if scenario == "comparator null" {
					want = append(want, 900022)
				}
				if !slices.Equal(calls, want) {
					t.Fatalf("current-pair effects=%v, want %v", calls, want)
				}
			})
		}
	}
}

func TestRowCompareCancellationSkipsLaterFieldsAndPublishesNothing(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := rowComparisonFixture(t)
		one := f.constant(20, model.Int64Val(1))
		effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
		first := f.function(900030, 23, true, effects, one, one)
		later := f.function(900031, 20, false, effects)
		second := f.call("btint8cmp", 23, later, later)
		root := f.rowCompare(ir.RowCompareLessEqual, first, second)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var calls []model.FunctionID
		options := f.options()
		options.Functions = resolverFunc(func(function expr.Function) (expr.ScalarCall, error) {
			return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
				calls = append(calls, function.ID)
				cancel()
				return model.Int64Val(0), nil
			}, nil
		})
		input, out := batch(1), columnar.Batch{}
		err := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options)).Eval(ctx, &input, nil, &out, mode)
		if !errors.Is(err, context.Canceled) || !slices.Equal(calls, []model.FunctionID{900030}) || out.Rows() != 0 || len(out.Columns) != 0 {
			t.Fatalf("cancellation=%v, calls=%v, output=%+v", err, calls, out)
		}
	}
}

func TestRowCompareRequiresSelectedLaterKernelAtCompilation(t *testing.T) {
	f := rowComparisonFixture(t)
	// enum_cmp is a real catalog comparator with no executable route, so the
	// compile-time gap is proven without depending on a kernel that may land.
	layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 3500, Storage: model.TypeInt64, Modifier: -1}})
	if err != nil {
		t.Fatal(err)
	}
	f.layouts[3500] = layout
	first := f.call("btint8cmp", 23, f.constant(20, model.Int64Val(1)), f.constant(20, model.Int64Val(2)))
	second := f.call("enum_cmp", 23, f.constant(3500, model.Int64Val(1)), f.constant(3500, model.Int64Val(2)))
	root := f.rowCompare(ir.RowCompareLess, first, second)
	_, err = expr.Compile(t.Context(), f.graph(), []ir.ScalarID{root}, f.options())
	var missing *builtin.MissingImplementationError
	if !errors.As(err, &missing) {
		t.Fatalf("missing selected support kernel=%v", err)
	}
}

func TestRowCompareResolvesArrayElementLayoutAndNullOrdering(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := rowComparisonFixture(t)
		layout := f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		left := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 1, Nullable: true}, nil, model.Value{})
		right := f.constant(1016, stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.Int64Val(1)))
		first := f.function(382, 23, true, ir.Effects{MayError: true}, left, right)
		bad := f.call("int8div", 20, f.constant(20, model.Int64Val(1)), f.x)
		second := f.call("btint8cmp", 23, bad, bad)
		root := f.rowCompare(ir.RowCompareGreater, first, second)
		options := f.options()
		options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
		program := compile(t, f.graph(), []ir.ScalarID{root}, options)
		input, out := batch(1), columnar.Batch{}
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.NullVal(model.TypeInt64))}}
		s := state(t, program)
		if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireInt(t, value(t, &out, 0, 0), 1, false)
		parameters[0].Value = model.NullVal(model.TypeArray)
		if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireInt(t, value(t, &out, 0, 0), 0, true)
	}
}

func TestRowCompareAcceptsNonUnitSupportComparisonResults(t *testing.T) {
	f := rowComparisonFixture(t)
	f.coercionType(model.Type{ID: 21, Storage: model.TypeInt64, Modifier: -1})
	low, high := f.constant(21, model.Int64Val(-32768)), f.constant(21, model.Int64Val(32767))
	first := f.call("btint2cmp", 23, low, high)
	second := f.call("btint2cmp", 23, high, low)
	root := f.rowCompare(ir.RowCompareLess, first, second)
	input, out := batch(1), columnar.Batch{}
	if err := state(t, compile(t, f.graph(), []ir.ScalarID{root}, f.options())).Eval(t.Context(), &input, nil, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	requireInt(t, value(t, &out, 0, 0), 1, false)
}
