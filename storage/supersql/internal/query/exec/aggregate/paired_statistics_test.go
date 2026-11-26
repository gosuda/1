package aggregate_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func pair(y, x float64) []model.Value {
	return []model.Value{model.Float64Val(y), model.Float64Val(x)}
}

func pairedAccumulator(t *testing.T, q *runtime.Query, function model.FunctionID) *aggregate.Accumulator {
	t.Helper()
	output := float8
	if function == 2818 {
		output = int8
	}
	o := options(t, q, []model.TypeLayout{float8, float8}, []model.TypeLayout{output}, nil, nil)
	a, err := aggregate.NewAccumulator(t.Context(), aggregate.AccumulatorOptions{Function: function, InputLayouts: o.InputLayouts, OutputLayout: output, Encoder: o.Encoder, Query: q, MaxRecordBytes: 1024, WorkBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func pairedResult(t *testing.T, a *aggregate.Accumulator, want model.Value) {
	t.Helper()
	got, err := a.Result(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.IsNull != want.IsNull || got.Type != want.Type {
		t.Fatalf("paired result=%+v, want %+v", got, want)
	}
	if want.IsNull {
		return
	}
	if want.Type == model.TypeInt64 {
		if got.IntVal != want.IntVal {
			t.Fatalf("paired count=%d, want %d", got.IntVal, want.IntVal)
		}
		return
	}
	if got.FloatVal != want.FloatVal && !(math.IsNaN(got.FloatVal) && math.IsNaN(want.FloatVal)) {
		t.Fatalf("paired result=%g, want %g", got.FloatVal, want.FloatVal)
	}
}

func TestPairedStatisticsUseSQLYXOrderAndCompletePairs(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{float8, float8}
	rows := [][]model.Value{pair(1, 0), pair(5, 2), {model.NullVal(model.TypeFloat64), model.Float64Val(100)}, {model.Float64Val(100), model.NullVal(model.TypeFloat64)}}
	var calls []aggregate.Call
	var outputs []model.TypeLayout
	for id := model.FunctionID(2818); id <= 2829; id++ {
		calls = append(calls, aggregate.Call{Function: id, Arguments: []int{0, 1}})
		output := float8
		if id == 2818 {
			output = int8
		}
		outputs = append(outputs, output)
	}
	got, err := drain(t, open(t, newSource(t, layouts, rows), options(t, q, layouts, outputs, nil, calls)))
	if err != nil || len(got) != 1 {
		t.Fatalf("paired rows=%v error=%v", got, err)
	}
	if got[0][0].IsNull || got[0][0].IntVal != 2 {
		t.Fatalf("complete pair count=%v, want 2", got[0][0])
	}
	for i, want := range []float64{2, 8, 4, 1, 3, 1, 2, 1, 2, 4, 1} {
		if got[0][i+1].IsNull || got[0][i+1].FloatVal != want {
			t.Errorf("function %d=%v, want %g", 2819+i, got[0][i+1], want)
		}
	}
	released(t, q, dir)
}

func TestPairedFinalizersHandleDegenerateAndNonfiniteInputs(t *testing.T) {
	null := model.NullVal(model.TypeFloat64)
	for _, scenario := range []struct {
		name     string
		function model.FunctionID
		rows     [][]model.Value
		want     model.Value
	}{
		{"empty covariance", 2827, nil, null},
		{"empty count", 2818, nil, model.Int64Val(0)},
		{"singleton population", 2827, [][]model.Value{pair(3, 2)}, model.Float64Val(0)},
		{"singleton sample", 2828, [][]model.Value{pair(math.Inf(1), 2)}, null},
		{"horizontal correlation", 2829, [][]model.Value{pair(3, 0), pair(3, 2)}, null},
		{"vertical correlation", 2829, [][]model.Value{pair(1, 2), pair(5, 2)}, null},
		{"horizontal r2", 2824, [][]model.Value{pair(3, 0), pair(3, 2)}, model.Float64Val(1)},
		{"vertical r2", 2824, [][]model.Value{pair(1, 2), pair(5, 2)}, null},
		{"vertical slope", 2825, [][]model.Value{pair(1, 2), pair(5, 2)}, null},
		{"vertical intercept", 2826, [][]model.Value{pair(1, 2), pair(5, 2)}, null},
		{"negative covariance", 2827, [][]model.Value{pair(5, 0), pair(1, 2)}, model.Float64Val(-2)},
		{"infinite singleton covariance", 2827, [][]model.Value{pair(1, math.Inf(1))}, model.Float64Val(math.NaN())},
		{"nan covariance", 2827, [][]model.Value{pair(math.NaN(), 0), pair(1, 2)}, model.Float64Val(math.NaN())},
		{"infinite x leaves y variance", 2820, [][]model.Value{pair(1, math.Inf(1)), pair(5, 2)}, model.Float64Val(8)},
		{"infinite y leaves x variance", 2819, [][]model.Value{pair(math.Inf(1), 0), pair(5, 2)}, model.Float64Val(2)},
		{"zero x takes precedence over nan y", 2824, [][]model.Value{pair(math.NaN(), 2)}, null},
		{"horizontal takes precedence over nan x", 2824, [][]model.Value{pair(1, math.NaN())}, model.Float64Val(1)},
		{"nan correlation", 2829, [][]model.Value{pair(math.NaN(), math.NaN())}, model.Float64Val(math.NaN())},
		{"count avoids overflowing moments", 2818, [][]model.Value{pair(1e200, 1e200), pair(-1e200, -1e200), pair(math.NaN(), math.Inf(1))}, model.Int64Val(3)},
		{"correlation preserves product overflow", 2829, [][]model.Value{pair(0, 0), pair(1e150, 1e150)}, model.Float64Val(0)},
		{"r2 preserves product overflow", 2824, [][]model.Value{pair(0, 0), pair(1e150, 1e150)}, model.Float64Val(math.NaN())},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			a := pairedAccumulator(t, q, scenario.function)
			for _, row := range scenario.rows {
				if err := a.Advance(t.Context(), row); err != nil {
					t.Fatal(err)
				}
			}
			pairedResult(t, a, scenario.want)
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestPairedAccumulatorRejectsInvalidArgumentsWithoutChangingState(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := pairedAccumulator(t, q, 2827)
	if err := a.Advance(t.Context(), pair(1, 0)); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]model.Value{nil, {model.Float64Val(1)}, {model.Float64Val(1), model.Float64Val(2), model.Float64Val(3)}, {model.Int64Val(1), model.Float64Val(2)}, {model.Float64Val(1), model.Int64Val(2)}} {
		if err := a.Advance(t.Context(), row); !errors.Is(err, columnar.ErrValueLayout) {
			t.Fatalf("arguments=%v error=%v, want layout error", row, err)
		}
		pairedResult(t, a, model.Float64Val(0))
	}
	for _, row := range [][]model.Value{{model.NullVal(model.TypeFloat64), model.Float64Val(1e200)}, {model.Float64Val(1e200), model.NullVal(model.TypeFloat64)}} {
		if err := a.Advance(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Advance(t.Context(), pair(5, 2)); err != nil {
		t.Fatal(err)
	}
	pairedResult(t, a, model.Float64Val(2))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestPairedOverflowPreservesMomentsAndResetReusesBoundedState(t *testing.T) {
	for _, scenario := range []struct {
		name            string
		first, rejected []model.Value
	}{
		{"x squares", pair(1, 1e200), pair(1, -1e200)},
		{"y squares", pair(1e200, 1), pair(-1e200, 1)},
		{"x sum", pair(1, math.MaxFloat64), pair(1, math.MaxFloat64)},
		{"y sum", pair(math.MaxFloat64, 1), pair(math.MaxFloat64, 1)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			a := pairedAccumulator(t, q, 2827)
			if err := a.Advance(t.Context(), scenario.first); err != nil {
				t.Fatal(err)
			}
			var sql *model.Error
			if err := a.Advance(t.Context(), scenario.rejected); !errors.As(err, &sql) || sql.Code != "22003" {
				t.Fatalf("finite overflow=%v", err)
			}
			pairedResult(t, a, model.Float64Val(0))
			usage := q.Usage().Work.Live
			if err := a.Reset(); err != nil {
				t.Fatal(err)
			}
			pairedResult(t, a, model.NullVal(model.TypeFloat64))
			for _, row := range [][]model.Value{pair(1, 0), pair(5, 2)} {
				if err := a.Advance(t.Context(), row); err != nil {
					t.Fatal(err)
				}
			}
			pairedResult(t, a, model.Float64Val(2))
			if q.Usage().Work.Live != usage {
				t.Fatalf("reset/advance changed admitted workspace: before=%d after=%d", usage, q.Usage().Work.Live)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestPairedSignaturesRejectBothInvalidArgumentLayoutsBeforePull(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		layouts   []model.TypeLayout
		arguments []int
	}{
		{"missing x", []model.TypeLayout{float8, float8}, []int{0}},
		{"extra argument", []model.TypeLayout{float8, float8}, []int{0, 1, 0}},
		{"invalid x column", []model.TypeLayout{float8, float8}, []int{0, 2}},
		{"integer y", []model.TypeLayout{int8, float8}, []int{0, 1}},
		{"integer x", []model.TypeLayout{float8, int8}, []int{0, 1}},
		{"invalid x storage", []model.TypeLayout{float8, {Type: model.Type{ID: 701, Storage: model.TypeInt64}}}, []int{0, 1}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			s := newSource(t, []model.TypeLayout{float8, float8}, nil)
			o := options(t, q, scenario.layouts, []model.TypeLayout{float8}, nil, []aggregate.Call{{Function: 2827, Arguments: scenario.arguments}})
			_, err := aggregate.New(t.Context(), s, o)
			if !errors.Is(err, columnar.ErrValueLayout) || s.pulls != 0 || !s.closed {
				t.Fatalf("invalid paired call: error=%v pulls=%d closed=%v", err, s.pulls, s.closed)
			}
			released(t, q, dir)
		})
	}
}

func TestPairedDistinctUsesBothArgumentsWithIndependentFilterLanes(t *testing.T) {
	for _, spill := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "spill"}[spill], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			layouts := []model.TypeLayout{float8, float8, boolean}
			var rows [][]model.Value
			for range 12 {
				rows = append(rows,
					[]model.Value{model.Float64Val(1), model.Float64Val(0), model.BoolVal(false)},
					[]model.Value{model.Float64Val(1), model.Float64Val(0), model.BoolVal(true)},
					[]model.Value{model.Float64Val(1), model.Float64Val(2), model.BoolVal(true)},
					[]model.Value{model.Float64Val(5), model.Float64Val(2), model.BoolVal(true)},
					[]model.Value{model.Float64Val(9), model.Float64Val(9), model.BoolVal(false)},
					[]model.Value{model.NullVal(model.TypeFloat64), model.Float64Val(2), model.BoolVal(true)},
					[]model.Value{model.Float64Val(1), model.NullVal(model.TypeFloat64), model.BoolVal(true)},
				)
			}
			calls := []aggregate.Call{{Function: 2818, Arguments: []int{0, 1}, Distinct: true, FilterColumn: new(2)}, {Function: 2818, Arguments: []int{0, 1}, Distinct: true}, {Function: 2818, Arguments: []int{0, 1}, FilterColumn: new(2)}}
			o := options(t, q, layouts, []model.TypeLayout{int8, int8, int8}, nil, calls)
			o.PartitionMemoryBytes = 1 << 20
			if spill {
				o.PartitionMemoryBytes = 1024
			}
			got, err := drain(t, open(t, newSource(t, layouts, rows), o))
			if err != nil || len(got) != 1 || got[0][0].IntVal != 3 || got[0][1].IntVal != 4 || got[0][2].IntVal != 36 {
				t.Fatalf("joint DISTINCT counts=%v error=%v, want 3,4,36", got, err)
			}
			if spill != (q.Usage().Spill.Peak != 0) {
				t.Fatalf("spill=%v peak=%d", spill, q.Usage().Spill.Peak)
			}
			released(t, q, dir)
		})
	}
}

func TestPairedOrderingPreservesStableSourceAcrossDistinct(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{float8, float8, int8, boolean}
	rows := [][]model.Value{
		{model.Float64Val(1e16), model.Float64Val(0), model.Int64Val(0), model.BoolVal(true)},
		{model.Float64Val(-1e16), model.Float64Val(0), model.Int64Val(0), model.BoolVal(true)},
		{model.Float64Val(1), model.Float64Val(0), model.Int64Val(0), model.BoolVal(false)},
		{model.Float64Val(1), model.Float64Val(0), model.Int64Val(0), model.BoolVal(true)},
	}
	calls := []aggregate.Call{
		{Function: 2818, Arguments: []int{0, 1}, Distinct: true, FilterColumn: new(3), OrderBy: []aggregate.OrderKey{{Column: 0}}},
		{Function: 2823, Arguments: []int{0, 1}, FilterColumn: new(3), OrderBy: []aggregate.OrderKey{{Column: 2}}},
		{Function: 2823, Arguments: []int{0, 1}, FilterColumn: new(3)},
		{Function: 2823, Arguments: []int{0, 1}, FilterColumn: new(3), OrderBy: []aggregate.OrderKey{{Column: 0}}},
	}
	o := options(t, q, layouts, []model.TypeLayout{int8, float8, float8, float8}, nil, calls)
	o.PartitionMemoryBytes = 1024
	got, err := drain(t, open(t, newSource(t, layouts, rows), o))
	if err != nil || len(got) != 1 {
		t.Fatalf("ordered paired rows=%v error=%v", got, err)
	}
	if got[0][0].IntVal != 3 || got[0][1].FloatVal != 1.0/3 || got[0][2].FloatVal != 1.0/3 || got[0][3].FloatVal != 0 {
		t.Fatalf("ordered paired results=%v, want 3,1/3,1/3,0", got)
	}
	released(t, q, dir)
}

func TestPairedSpillEarlyCloseAndCancelReleaseResources(t *testing.T) {
	for _, cancelQuery := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "cancel"}[cancelQuery], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			layouts := []model.TypeLayout{int8, float8, float8}
			var rows [][]model.Value
			for i := range 40 {
				rows = append(rows, []model.Value{model.Int64Val(int64(i)), model.Float64Val(1), model.Float64Val(2)})
			}
			s := newSource(t, layouts, rows)
			o := options(t, q, layouts, []model.TypeLayout{int8, float8}, []int{0}, []aggregate.Call{{Function: 2827, Arguments: []int{1, 2}, Distinct: true, OrderBy: []aggregate.OrderKey{{Column: 1}}}})
			o.PartitionMemoryBytes = 1024
			c, err := aggregate.New(ctx, s, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := c.Close(); err != nil {
					t.Error(err)
				}
			})
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Rows() != 1 || out.Columns[1].Float64[0] != 0 || q.Usage().Spill.Live == 0 {
				t.Fatalf("first paired result=%v usage=%+v", out, q.Usage())
			}
			if cancelQuery {
				cancel()
				if err := c.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled paired aggregate=%v", err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !s.closed {
				t.Fatal("paired aggregate retained source")
			}
			released(t, q, dir)
		})
	}
}

func TestPairedMomentsSurviveDeferredGroupSpill(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{int8, float8, float8}
	var rows [][]model.Value
	for x := range 6 {
		for id := range 9 {
			rows = append(rows, []model.Value{model.Int64Val(int64(id)), model.Float64Val(float64(2*x + 1)), model.Float64Val(float64(x))})
		}
	}
	calls := []aggregate.Call{{Function: 2818, Arguments: []int{1, 2}}, {Function: 2825, Arguments: []int{1, 2}}}
	o := options(t, q, layouts, []model.TypeLayout{int8, int8, float8}, []int{0}, calls)
	o.GroupBytes = 32 << 10
	got, err := drain(t, open(t, newSource(t, layouts, rows), o))
	if err != nil || len(got) != 9 {
		t.Fatalf("spilled paired groups=%v error=%v", got, err)
	}
	seen := make(map[int64]bool)
	for _, row := range got {
		id := row[0].IntVal
		invalidGroup := id < 0 || id >= 9 || seen[id]
		wrongSlope := row[2].IsNull || row[2].FloatVal != 2
		if invalidGroup || row[1].IntVal != 6 || wrongSlope {
			t.Fatalf("spilled group=%v, want unique id and count=6 slope=2", row)
		}
		seen[id] = true
	}
	if q.Usage().Spill.Peak == 0 {
		t.Fatal("deferred paired groups did not spill")
	}
	released(t, q, dir)
}

func TestPairedAccumulatorRejectsInsufficientWorkspaceBeforeRetainingState(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := pairedAccumulator(t, q, 2827)
	work := a.WorkBytes()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	o := options(t, q, []model.TypeLayout{float8, float8}, []model.TypeLayout{float8}, nil, nil)
	_, err := aggregate.NewAccumulator(t.Context(), aggregate.AccumulatorOptions{Function: 2827, InputLayouts: o.InputLayouts, OutputLayout: float8, Encoder: o.Encoder, Query: q, MaxRecordBytes: 1024, WorkBytes: work - 1})
	if !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("insufficient paired workspace=%v, want budget error", err)
	}
	released(t, q, dir)
}
