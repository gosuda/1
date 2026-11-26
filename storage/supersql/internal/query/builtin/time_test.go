package builtin_test

import (
	"math"
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestMakeTimeRoundsBeforeCheckingSecondBoundaries(t *testing.T) {
	for _, tc := range []struct {
		hour, minute int64
		seconds      float64
		want         int64
		fail         bool
	}{
		{0, 0, -0.0000004, 0, false}, {0, 0, 0.0000005, 0, false}, {0, 0, 0.0000015, 2, false}, {23, 59, 59.9999996, 86400000000, false}, {24, 0, 0, 86400000000, false}, {24, 0, 0.000001, 0, true}, {0, 0, 60.000001, 0, true}, {0, 0, math.NaN(), 0, true}, {0, 0, math.Inf(1), 0, true},
	} {
		value, err := jsonCallable(t, 3847).Call(t.Context(), []model.Value{model.Int64Val(tc.hour), model.Int64Val(tc.minute), model.Float64Val(tc.seconds)}, builtin.CallBudget{})
		if tc.fail {
			requireSQLState(t, err, "22008")
			continue
		}
		if err != nil || value.IntVal != tc.want {
			t.Errorf("make_time(%d,%d,%g)=%v,%v; want%d", tc.hour, tc.minute, tc.seconds, value, err, tc.want)
		}
	}
}
func TestTimeOutputAndIntervalDayWrap(t *testing.T) {
	time := model.Value{Type: model.TypeTimestamp, IntVal: 86400000000}
	value, err := jsonCallable(t, 1144).Call(t.Context(), []model.Value{time}, builtin.CallBudget{OutputBytes: 8, MemoryBytes: 8})
	if err != nil || value.StrVal != "24:00:00" {
		t.Fatalf("day endpoint output=%v,%v", value, err)
	}
	span := model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Months: 4, Days: 2, Microseconds: 1}}
	value, err = jsonCallable(t, 1747).Call(t.Context(), []model.Value{time, span}, builtin.CallBudget{})
	if err != nil || value.IntVal != 1 {
		t.Fatalf("day wrap=%v,%v", value, err)
	}
	_, err = jsonCallable(t, 1144).Call(t.Context(), []model.Value{time}, builtin.CallBudget{OutputBytes: 7, MemoryBytes: 7})
	requireSQLState(t, err, "54000")
}
