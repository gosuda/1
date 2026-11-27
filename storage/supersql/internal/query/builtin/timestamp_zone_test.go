package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestIntervalTimezoneWholeSecondsAndNarrowing(t *testing.T) {
	for _, id := range []model.FunctionID{1026, 2070} {
		call := jsonCallable(t, id)
		for _, tc := range []struct{ offset, want int64 }{{1999999, 1000000}, {-1999999, -1000000}, {172800000000, 172800000000}} {
			want := tc.want
			if id == 2070 {
				want = -want
			}
			got, err := call.Call(t.Context(), []model.Value{{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: tc.offset}}, timestampDatum(0)}, builtin.CallBudget{})
			if err != nil || got.IntVal != want {
				t.Fatalf("timezone%d(%d)=%v,%v; want%d", id, tc.offset, got, err, want)
			}
		}
		for _, seconds := range []int64{math.MinInt32, int64(math.MaxInt32) + 1} {
			got, err := call.Call(t.Context(), []model.Value{{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: seconds * 1000000}}, timestampDatum(0)}, builtin.CallBudget{})
			if err != nil || got.IntVal != 2147483648000000 {
				t.Fatalf("int32 offset narrowing%d(%d)=%v,%v", id, seconds, got, err)
			}
		}
	}
}

func TestIntervalTimezoneInfinityAndErrorPrecedence(t *testing.T) {
	for _, id := range []model.FunctionID{1026, 2070} {
		call := jsonCallable(t, id)
		for _, zone := range []model.Interval{{Days: 1}, {Months: 1}, {Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}} {
			args := []model.Value{{Type: model.TypeInterval, IntervalVal: zone}, timestampDatum(math.MaxInt64)}
			got, err := call.Call(t.Context(), args, builtin.CallBudget{})
			if err != nil || got.IntVal != math.MaxInt64 {
				t.Fatalf("infinite timestamp precedence=%v,%v", got, err)
			}
			args[1] = timestampDatum(0)
			_, err = call.Call(t.Context(), args, builtin.CallBudget{})
			requireSQLState(t, err, "22023")
		}
		_, err := call.Call(t.Context(), []model.Value{{Type: model.TypeInterval}, timestampDatum(9223371331200000000)}, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
	}
}
