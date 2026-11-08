package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestIntervalComparisonUsesExactThirtyDayMonths(t *testing.T) {
	for _, test := range []struct {
		name        string
		left, right model.Interval
		order       int64
	}{
		{"month equals thirty days", model.Interval{Months: 1}, model.Interval{Days: 30}, 0},
		{"mixed signs cancel", model.Interval{Months: -1, Days: 30}, model.Interval{}, 0},
		{"microsecond above wide positive total", model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: 1}, model.Interval{Months: math.MaxInt32, Days: math.MaxInt32}, 1},
		{"microsecond below wide negative total", model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: -1}, model.Interval{Months: math.MinInt32, Days: math.MinInt32}, -1},
		{"opposite fields retain negative sign", model.Interval{Months: math.MinInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}, model.Interval{}, -1},
		{"opposite fields retain positive sign", model.Interval{Months: math.MaxInt32, Days: math.MinInt32, Microseconds: math.MinInt64}, model.Interval{}, 1},
		{"signed time carry", model.Interval{Days: 1, Microseconds: -1}, model.Interval{}, 1},
		{"signed day borrow", model.Interval{Days: -1, Microseconds: 1}, model.Interval{}, -1},
		{"negative infinity below finite minimum", model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}, model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64 + 1}, -1},
		{"positive infinity above finite maximum", model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}, model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64 - 1}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := intervalArithmeticCallable(t, 1315).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.left), intervalArithmeticDatum(test.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IsNull || got.Type != model.TypeInt64 || got.IntVal != test.order {
				t.Fatalf("interval_cmp(%+v, %+v) = %+v, %v; want %d", test.left, test.right, got, err, test.order)
			}
		})
	}
}

func TestIntervalComparisonPredicatesAgreeOnEquivalentRepresentations(t *testing.T) {
	for _, test := range []struct {
		id                   model.FunctionID
		less, equal, greater bool
	}{
		{1162, false, true, false},
		{1163, true, false, true},
		{1164, true, false, false},
		{1165, true, true, false},
		{1166, false, true, true},
		{1167, false, false, true},
	} {
		function := intervalArithmeticCallable(t, test.id)
		for _, comparison := range []struct {
			days int32
			want bool
		}{
			{31, test.less}, {30, test.equal}, {29, test.greater},
		} {
			got, err := function.Call(t.Context(), []model.Value{
				intervalArithmeticDatum(model.Interval{Months: 1}), intervalArithmeticDatum(model.Interval{Days: comparison.days}),
			}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IsNull || got.Type != model.TypeBoolean || (got.IntVal != 0) != comparison.want {
				t.Fatalf("function %d(1 month, %d days) = %+v, %v; want %t", test.id, comparison.days, got, err, comparison.want)
			}
		}
	}
}

func TestIntervalIsFiniteRecognizesOnlyCompleteSentinels(t *testing.T) {
	for _, test := range []struct {
		name   string
		value  model.Interval
		finite bool
	}{
		{"minimum month", model.Interval{Months: math.MinInt32}, true},
		{"minimum day", model.Interval{Days: math.MinInt32}, true},
		{"minimum time", model.Interval{Microseconds: math.MinInt64}, true},
		{"finite near infinity", model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64 - 1}, true},
		{"positive infinity", model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}, false},
		{"negative infinity", model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := intervalArithmeticCallable(t, 1390).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.value)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IsNull || got.Type != model.TypeBoolean || (got.IntVal != 0) != test.finite {
				t.Fatalf("isfinite(%+v) = %+v, %v; want %t", test.value, got, err, test.finite)
			}
		})
	}
}
