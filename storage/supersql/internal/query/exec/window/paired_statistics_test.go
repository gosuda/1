package window_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestPairedStatisticsFramesAndPartitionReset(t *testing.T) {
	nullFloat := model.NullVal(model.TypeFloat64)
	for _, tc := range []struct {
		name      string
		moving    bool
		exclusion ir.FrameExclusion
		want      [][]model.Value
	}{
		{
			name: "growing",
			want: [][]model.Value{
				{iv(1), nullFloat, fv(0)}, {iv(2), fv(2), fv(2)}, {iv(2), fv(2), fv(2)},
				{iv(3), fv(2), fv(16.0 / 3)}, {iv(3), fv(2), fv(16.0 / 3)}, {iv(1), nullFloat, fv(0)},
			},
		},
		{
			name: "moving", moving: true,
			want: [][]model.Value{
				{iv(1), nullFloat, fv(0)}, {iv(2), fv(2), fv(2)}, {iv(1), nullFloat, fv(0)},
				{iv(1), nullFloat, fv(0)}, {iv(1), nullFloat, fv(0)}, {iv(1), nullFloat, fv(0)},
			},
		},
		{
			name: "exclude current", exclusion: ir.ExcludeCurrent,
			want: [][]model.Value{
				{iv(0), nullFloat, nullFloat}, {iv(1), nullFloat, fv(0)}, {iv(2), fv(2), fv(2)},
				{iv(2), fv(2), fv(2)}, {iv(3), fv(2), fv(16.0 / 3)}, {iv(0), nullFloat, nullFloat},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, memory := range []uint64{0, 1 << 20} {
				q, dir := query(t, false)
				calls := []window.Call{
					{Function: 2818, Arguments: []int{1, 2}},
					{Function: 2825, Arguments: []int{1, 2}},
					{Function: 2827, Arguments: []int{1, 2}},
				}
				o := options(t, q, []model.TypeLayout{int8, float8, float8}, []model.TypeLayout{int8, float8, float8}, calls)
				o.PartitionColumns, o.PartitionMemoryBytes = []int{0}, memory
				o.Frame.Mode, o.Frame.Exclusion = ir.RowsFrame, tc.exclusion
				if tc.moving {
					o.Frame.Start.Kind = ir.OffsetPreceding
					o.StartOffset = new(iv(1))
				}
				input := newSource(t, o.InputLayouts, [][]model.Value{
					{iv(1), fv(2), fv(1)}, {iv(1), fv(6), fv(3)}, {iv(1), nullFloat, fv(5)},
					{iv(1), fv(10), fv(5)}, {iv(1), fv(14), nullFloat}, {iv(2), fv(20), fv(10)},
				})
				assertRows(t, open(t, input, o), tc.want)
				released(t, q, dir)
			}
		})
	}
}

func TestPairedStatisticsFilterGuardsBothVolatileArguments(t *testing.T) {
	for _, memory := range []uint64{0, 1 << 20} {
		q, dir := query(t, false)
		boolean := model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}}
		filter := aggregateExpression{boolean, func(_ context.Context, row []model.Value) (model.Value, error) {
			return row[1], nil
		}}
		nextY, nextX := float64(0), float64(0)
		y := aggregateExpression{float8, func(_ context.Context, row []model.Value) (model.Value, error) {
			if row[1].IsNull || row[1].IntVal != 1 {
				return model.Value{}, errFilteredWindowArgument
			}
			nextY += 2
			return fv(nextY), nil
		}}
		x := aggregateExpression{float8, func(_ context.Context, row []model.Value) (model.Value, error) {
			if row[1].IsNull || row[1].IntVal != 1 {
				return model.Value{}, errFilteredWindowArgument
			}
			nextX++
			return fv(nextX), nil
		}}
		o := options(t, q, []model.TypeLayout{int8, boolean}, []model.TypeLayout{float8}, []window.Call{{
			Function: 2827, Arguments: []int{-1, -1}, Expressions: []window.ArgumentExpression{y, x}, Filter: filter,
		}})
		o.OrderBy, o.PartitionMemoryBytes = []window.OrderKey{{Column: 0}}, memory
		input := newSource(t, o.InputLayouts, [][]model.Value{
			{iv(1), model.BoolVal(true)}, {iv(1), model.BoolVal(false)},
			{iv(1), model.NullVal(model.TypeBoolean)}, {iv(2), model.BoolVal(true)},
		})
		assertRows(t, open(t, input, o), [][]model.Value{{fv(0)}, {fv(0)}, {fv(0)}, {fv(0.5)}})
		released(t, q, dir)
	}
}

var errSecondPairedWindowArgument = errors.New("second paired window argument failed")

func TestPairedStatisticsNullFirstArgumentPreservesSecondError(t *testing.T) {
	for _, memory := range []uint64{0, 1 << 20} {
		q, dir := query(t, false)
		second := aggregateExpression{float8, func(context.Context, []model.Value) (model.Value, error) {
			return model.Value{}, errSecondPairedWindowArgument
		}}
		o := options(t, q, []model.TypeLayout{float8}, []model.TypeLayout{int8}, []window.Call{{
			Function: 2818, Arguments: []int{0, -1}, Expressions: []window.ArgumentExpression{nil, second},
		}})
		o.PartitionMemoryBytes = memory
		input := newSource(t, o.InputLayouts, [][]model.Value{{model.NullVal(model.TypeFloat64)}})
		_, err := drain(t, open(t, input, o))
		if !errors.Is(err, errSecondPairedWindowArgument) {
			t.Fatalf("NULL first argument suppressed second argument error: %v", err)
		}
		if !input.closed {
			t.Fatal("argument failure left input open")
		}
		released(t, q, dir)
	}
}

func TestPairedStatisticsInvalidArityRejectsBeforeInput(t *testing.T) {
	for _, arguments := range [][]int{nil, {0}, {0, 0, 0}} {
		q, dir := query(t, false)
		o := options(t, q, []model.TypeLayout{float8}, []model.TypeLayout{int8}, []window.Call{{Function: 2818, Arguments: arguments}})
		if err := window.CheckSupport(t.Context(), o); !errors.Is(err, columnar.ErrValueLayout) {
			t.Fatalf("CheckSupport arity %d: %v", len(arguments), err)
		}
		input := newSource(t, o.InputLayouts, [][]model.Value{{fv(1)}})
		c, err := window.New(t.Context(), input, o)
		if c != nil {
			if closeErr := c.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if !errors.Is(err, columnar.ErrValueLayout) {
			t.Fatalf("New arity %d: %v", len(arguments), err)
		}
		if input.pulls != 0 || !input.closed {
			t.Fatalf("invalid arity consumed input: pulls=%d closed=%v", input.pulls, input.closed)
		}
		released(t, q, dir)
	}
}
