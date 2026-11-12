package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestIntervalToTimeUsesOnlyFractionalDay(t *testing.T) {
	for _, tc := range []struct{ micros, want int64 }{{0, 0}, {86400000000, 0}, {-1, 86399999999}, {-7200000000, 79200000000}, {math.MinInt64, 71945224192}, {math.MaxInt64, 14454775807}} {
		input := model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Months: 14, Days: -4, Microseconds: tc.micros}}
		value, err := jsonCallable(t, 1419).Call(t.Context(), []model.Value{input}, builtin.CallBudget{})
		if err != nil || value.Type != model.TypeTimestamp || value.IntVal != tc.want {
			t.Errorf("interval %d to time=%v,%v; want%d", tc.micros, value, err, tc.want)
		}
	}
}

func TestTimeIntervalConversionPreservesDayEndpointUntilConvertedBack(t *testing.T) {
	source := model.Value{Type: model.TypeTimestamp, IntVal: 86400000000}
	interval, err := jsonCallable(t, 1370).Call(t.Context(), []model.Value{source}, builtin.CallBudget{})
	if err != nil || interval.IntervalVal != (model.Interval{Microseconds: 86400000000}) {
		t.Fatalf("time to interval=%v,%v", interval, err)
	}
	value, err := jsonCallable(t, 1419).Call(t.Context(), []model.Value{interval}, builtin.CallBudget{})
	if err != nil || value.IntVal != 0 {
		t.Fatalf("day interval to time=%v,%v", value, err)
	}
	infinite := model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}}
	_, err = jsonCallable(t, 1419).Call(t.Context(), []model.Value{infinite}, builtin.CallBudget{})
	requireSQLState(t, err, "22008")
}
