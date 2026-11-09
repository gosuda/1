package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimestampBinsRoundTowardNegativeInfinity(t *testing.T) {
	for _, id := range []model.FunctionID{6177, 6178} {
		for _, tc := range []struct{ value, origin, stride, want int64 }{{9, 0, 10, 0}, {10, 0, 10, 10}, {-1, 0, 10, -10}, {-10, 0, 10, -10}, {-11, 0, 10, -20}, {5, 7, 10, -3}, {27, 7, 10, 27}} {
			args := []model.Value{{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: tc.stride}}, {Type: model.TypeTimestamp, IntVal: tc.value}, {Type: model.TypeTimestamp, IntVal: tc.origin}}
			result, err := jsonCallable(t, id).Call(t.Context(), args, builtin.CallBudget{})
			if err != nil || result.IntVal != tc.want {
				t.Fatalf("date_bin%d(%d,%d,%d)=%v,%v; want%d", id, tc.stride, tc.value, tc.origin, result, err, tc.want)
			}
		}
	}
}

func TestTimestampBinErrorOrderAndOverflow(t *testing.T) {
	for _, id := range []model.FunctionID{6177, 6178} {
		args := []model.Value{{Type: model.TypeInterval, IntervalVal: model.Interval{Months: 1}}, {Type: model.TypeTimestamp, IntVal: math.MaxInt64}, {Type: model.TypeTimestamp, IntVal: math.MinInt64}}
		result, err := jsonCallable(t, id).Call(t.Context(), args, builtin.CallBudget{})
		if err != nil || result.IntVal != math.MaxInt64 {
			t.Fatalf("infinite source did not precede origin/stride errors: %v,%v", result, err)
		}
		args[1].IntVal = 0
		_, err = jsonCallable(t, id).Call(t.Context(), args, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
		args[2].IntVal = 0
		_, err = jsonCallable(t, id).Call(t.Context(), args, builtin.CallBudget{})
		requireSQLState(t, err, "0A000")
		args[0].IntervalVal = model.Interval{Days: math.MaxInt32, Microseconds: math.MinInt64}
		_, err = jsonCallable(t, id).Call(t.Context(), args, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
		args[0].IntervalVal = model.Interval{Microseconds: 1}
		args[1].IntVal = 9223371331199999999
		args[2].IntVal = -211813488000000000
		_, err = jsonCallable(t, id).Call(t.Context(), args, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
	}
}
