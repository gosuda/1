package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestIntervalModifiersPreserveHigherFields(t *testing.T) {
	function, err := builtin.Resolve(1200, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	value := model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Months: -25, Days: 9, Microseconds: -3_661_234_567}}
	for _, tc := range []struct {
		name              string
		fields, precision int32
		want              model.Interval
	}{
		{"year", 4, 65535, model.Interval{Months: -24}},
		{"month", 2, 65535, model.Interval{Months: -25}},
		{"day", 8, 65535, model.Interval{Months: -25, Days: 9}},
		{"hour", 1024, 65535, model.Interval{Months: -25, Days: 9, Microseconds: -3_600_000_000}},
		{"minute", 2048, 65535, model.Interval{Months: -25, Days: 9, Microseconds: -3_660_000_000}},
		{"second", 4096, 3, model.Interval{Months: -25, Days: 9, Microseconds: -3_661_235_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := function.Call(t.Context(), []model.Value{value, model.Int64Val(int64(tc.fields<<16 | tc.precision))}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if result.IntervalVal != tc.want {
				t.Fatalf("interval fields=%+v, want %+v", result.IntervalVal, tc.want)
			}
		})
	}
}

func TestIntervalPrecisionRoundsTiesAwayFromZero(t *testing.T) {
	function, err := builtin.Resolve(1200, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, micros := range []int64{1_234_500, -1_234_500} {
		value := model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: micros}}
		result, err := function.Call(t.Context(), []model.Value{value, model.Int64Val(0x7fff0003)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil {
			t.Fatal(err)
		}
		want := int64(1_235_000)
		if micros < 0 {
			want = -want
		}
		if result.IntervalVal.Microseconds != want {
			t.Fatalf("round(%d)=%d, want %d", micros, result.IntervalVal.Microseconds, want)
		}
	}
}

func TestIntervalPrecisionRejectsRoundingOverflow(t *testing.T) {
	function, err := builtin.Resolve(1200, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, micros := range []int64{math.MinInt64, math.MaxInt64} {
		value := model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: micros}}
		_, err := function.Call(t.Context(), []model.Value{value, model.Int64Val(0x7fff0000)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22008")
	}
}

func TestIntervalInfinityBypassesInvalidTypmod(t *testing.T) {
	function, err := builtin.Resolve(1200, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infinity := model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}
	result, err := function.Call(t.Context(), []model.Value{{Type: model.TypeInterval, IntervalVal: infinity}, model.Int64Val(0)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.IntervalVal != infinity {
		t.Fatalf("infinite interval changed to %+v", result.IntervalVal)
	}
	_, err = function.Call(t.Context(), []model.Value{{Type: model.TypeInterval}, model.Int64Val(0)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "XX000")
}
