package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimestampFromUnixEpochAndRange(t *testing.T) {
	call := jsonCallable(t, 1158)
	for _, tc := range []struct {
		seconds float64
		want    int64
	}{
		{0, -946684800000000}, {946684800, 0}, {-210866803200, -211813488000000000},
		{math.Inf(-1), math.MinInt64}, {math.Inf(1), math.MaxInt64},
		{0.0000005, -946684800000000}, {0.0000015, -946684799999998},
		{-0.0000005, -946684800000000}, {-0.0000015, -946684800000002},
	} {
		got, err := call.Call(t.Context(), []model.Value{model.Float64Val(tc.seconds)}, builtin.CallBudget{})
		if err != nil || got.IntVal != tc.want {
			t.Fatalf("to_timestamp(%g)=%v,%v; want%d", tc.seconds, got, err, tc.want)
		}
	}
	for _, seconds := range []float64{math.NaN(), math.Nextafter(-210866803200, math.Inf(-1)), 9224318016000} {
		_, err := call.Call(t.Context(), []model.Value{model.Float64Val(seconds)}, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
	}
}
