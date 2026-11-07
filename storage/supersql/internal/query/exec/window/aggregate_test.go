package window_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestAggregateFrameModesAndExclusions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      ir.FrameMode
		exclusion ir.FrameExclusion
		moving    bool
		want      [][]model.Value
	}{
		{"peer range", ir.RangeFrame, ir.ExcludeNone, false, [][]model.Value{{iv(2), iv(6), iv(2)}, {iv(2), iv(6), iv(2)}, {iv(3), iv(6), iv(2)}, {iv(4), iv(14), iv(3)}}},
		{"moving rows", ir.RowsFrame, ir.ExcludeNone, true, [][]model.Value{{iv(1), iv(2), iv(1)}, {iv(2), iv(6), iv(2)}, {iv(2), iv(4), iv(1)}, {iv(2), iv(8), iv(1)}}},
		{"moving groups", ir.GroupsFrame, ir.ExcludeNone, true, [][]model.Value{{iv(2), iv(6), iv(2)}, {iv(2), iv(6), iv(2)}, {iv(3), iv(6), iv(2)}, {iv(2), iv(8), iv(1)}}},
		{"exclude current", ir.RangeFrame, ir.ExcludeCurrent, false, [][]model.Value{{iv(1), iv(4), iv(1)}, {iv(1), iv(2), iv(1)}, {iv(2), iv(6), iv(2)}, {iv(3), iv(6), iv(2)}}},
		{"exclude group", ir.RangeFrame, ir.ExcludeGroup, false, [][]model.Value{{iv(0), null(), iv(0)}, {iv(0), null(), iv(0)}, {iv(2), iv(6), iv(2)}, {iv(3), iv(6), iv(2)}}},
		{"exclude ties", ir.RangeFrame, ir.ExcludeTies, false, [][]model.Value{{iv(1), iv(2), iv(1)}, {iv(1), iv(4), iv(1)}, {iv(3), iv(6), iv(2)}, {iv(4), iv(14), iv(3)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, memory := range []uint64{0, 1 << 20} {
				q, dir := query(t, false)
				calls := []window.Call{{Function: 2803}, {Function: 2108, Arguments: []int{1}}, {Function: 2147, Arguments: []int{1}}}
				o := options(t, q, []model.TypeLayout{int8, int4}, []model.TypeLayout{int8, int8, int8}, calls)
				o.Frame.Mode, o.Frame.Exclusion = tc.mode, tc.exclusion
				o.PartitionMemoryBytes = memory
				o.OrderBy = []window.OrderKey{{Column: 0}}
				if tc.moving {
					o.Frame.Start.Kind = ir.OffsetPreceding
					o.StartOffset = new(iv(1))
				}
				input := newSource(t, o.InputLayouts, [][]model.Value{{iv(1), iv(2)}, {iv(1), iv(4)}, {iv(2), null()}, {iv(3), iv(8)}})
				assertRows(t, open(t, input, o), tc.want)
				released(t, q, dir)
			}
		})
	}
}

func TestAggregateNumericPrecisionAndPartitionReset(t *testing.T) {
	compare, err := builtin.Resolve(1769, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	number := func(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }
	for _, memory := range []uint64{0, 1 << 20} {
		q, dir := query(t, false)
		calls := []window.Call{{Function: 2114, Arguments: []int{1}}, {Function: 2103, Arguments: []int{1}}, {Function: 2132, Arguments: []int{2}}, {Function: 2116, Arguments: []int{2}}}
		o := options(t, q, []model.TypeLayout{int8, numeric, int4}, []model.TypeLayout{numeric, numeric, int4, int4}, calls)
		o.PartitionColumns, o.PartitionMemoryBytes = []int{0}, memory
		input := newSource(t, o.InputLayouts, [][]model.Value{{iv(1), number("9007199254740993.25"), iv(9)}, {iv(1), number("9007199254740993.75"), iv(-2)}, {iv(2), model.NullVal(model.TypeNumeric), null()}})
		got, err := drain(t, open(t, input, o))
		if err != nil || len(got) != 3 {
			t.Fatalf("rows=%v error=%v", got, err)
		}
		for _, row := range got[:2] {
			for i, want := range []string{"18014398509481987", "9007199254740993.5"} {
				cmp, err := compare.Call(t.Context(), []model.Value{row[i], number(want)}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 4 << 20})
				if err != nil || cmp.IntVal != 0 || row[i].IsNull {
					t.Fatalf("numeric=%v want=%s error=%v", row[i], want, err)
				}
			}
			if row[2].IsNull || row[2].IntVal != -2 || row[3].IsNull || row[3].IntVal != 9 {
				t.Fatalf("extrema=%v", row[2:])
			}
		}
		for _, value := range got[2] {
			if !value.IsNull {
				t.Fatalf("all-NULL partition returned %v", got[2])
			}
		}
		released(t, q, dir)
	}
}

type aggregateExpression struct {
	layout model.TypeLayout
	eval   func(context.Context, []model.Value) (model.Value, error)
}

func (e aggregateExpression) Layout() model.TypeLayout { return e.layout }
func (e aggregateExpression) Eval(ctx context.Context, row []model.Value) (model.Value, error) {
	return e.eval(ctx, row)
}

var errFilteredWindowArgument = errors.New("filtered argument evaluated")
var errExcludedWindowArgument = errors.New("excluded row evaluated")

func TestAggregateFilterPrecedesVolatileArguments(t *testing.T) {
	for _, memory := range []uint64{0, 1 << 20} {
		q, dir := query(t, false)
		boolean := model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}}
		filterCalls, argumentCalls := 0, int64(0)
		filter := aggregateExpression{boolean, func(_ context.Context, row []model.Value) (model.Value, error) {
			filterCalls++
			return row[1], nil
		}}
		argument := aggregateExpression{int4, func(_ context.Context, row []model.Value) (model.Value, error) {
			if row[1].IsNull || row[1].IntVal != 1 {
				return model.Value{}, errFilteredWindowArgument
			}
			argumentCalls++
			return iv(argumentCalls), nil
		}}
		o := options(t, q, []model.TypeLayout{int8, boolean}, []model.TypeLayout{int8}, []window.Call{{Function: 2108, Arguments: []int{-1}, Expressions: []window.ArgumentExpression{argument}, Filter: filter}})
		o.OrderBy, o.PartitionMemoryBytes = []window.OrderKey{{Column: 0}}, memory
		input := newSource(t, o.InputLayouts, [][]model.Value{{iv(1), model.BoolVal(true)}, {iv(1), model.BoolVal(false)}, {iv(1), model.NullVal(model.TypeBoolean)}, {iv(2), model.BoolVal(true)}})
		assertRows(t, open(t, input, o), [][]model.Value{{iv(1)}, {iv(1)}, {iv(1)}, {iv(3)}})
		if filterCalls != 4 || argumentCalls != 2 {
			t.Fatalf("filter calls=%d argument calls=%d", filterCalls, argumentCalls)
		}
		released(t, q, dir)
	}
}

func TestAggregateExcludedRowsNeverEvaluateArguments(t *testing.T) {
	q, dir := query(t, false)
	argument := aggregateExpression{int4, func(context.Context, []model.Value) (model.Value, error) {
		return model.Value{}, errExcludedWindowArgument
	}}
	calls := []window.Call{
		{Function: 2108, Arguments: []int{-1}, Expressions: []window.ArgumentExpression{argument}},
		{Function: 2101, Arguments: []int{-1}, Expressions: []window.ArgumentExpression{argument}},
		{Function: 2132, Arguments: []int{-1}, Expressions: []window.ArgumentExpression{argument}},
		{Function: 2116, Arguments: []int{-1}, Expressions: []window.ArgumentExpression{argument}},
	}
	o := options(t, q, []model.TypeLayout{int4}, []model.TypeLayout{int8, numeric, int4, int4}, calls)
	o.Frame = ir.Frame{Mode: ir.RowsFrame, Start: ir.FrameBound{Kind: ir.CurrentRow}, End: ir.FrameBound{Kind: ir.CurrentRow}, Exclusion: ir.ExcludeCurrent}
	o.PartitionMemoryBytes = 0
	assertRows(t, open(t, newSource(t, o.InputLayouts, [][]model.Value{{iv(0)}, {iv(0)}}), o), [][]model.Value{{null(), model.NullVal(model.TypeNumeric), null(), null()}, {null(), model.NullVal(model.TypeNumeric), null(), null()}})
	released(t, q, dir)
}

func TestAggregateFailureCancellationAndEarlyCloseReleaseSpill(t *testing.T) {
	for _, mode := range []string{"error", "cancel", "early close"} {
		t.Run(mode, func(t *testing.T) {
			q, dir := query(t, false)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("aggregate argument failed")
			argument := aggregateExpression{int4, func(context.Context, []model.Value) (model.Value, error) {
				if mode == "error" {
					return model.Value{}, failure
				}
				if mode == "cancel" {
					cancel()
				}
				return iv(1), nil
			}}
			o := options(t, q, []model.TypeLayout{int4}, []model.TypeLayout{int8}, []window.Call{{Function: 2108, Arguments: []int{-1}, Expressions: []window.ArgumentExpression{argument}}})
			o.PartitionMemoryBytes = 0
			input := newSource(t, o.InputLayouts, [][]model.Value{{iv(1)}, {iv(2)}})
			c := open(t, input, o)
			batch := columnar.Batch{MaxRows: 1}
			err := c.Next(ctx, &batch)
			switch mode {
			case "error":
				if !errors.Is(err, failure) {
					t.Fatalf("error=%v", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			case "early close":
				if err != nil || batch.Rows() != 1 {
					t.Fatalf("Next error=%v rows=%d", err, batch.Rows())
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if !input.closed {
				t.Fatal("aggregate window did not close input")
			}
			released(t, q, dir)
		})
	}
}
