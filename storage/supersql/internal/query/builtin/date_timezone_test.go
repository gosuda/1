package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestDateTimezonePromotionRangeAndInfinity(t *testing.T) {
	call, err := builtin.Resolve(1174, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "<+14>-14"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ date, want int64 }{{math.MinInt32, math.MinInt64}, {math.MaxInt32, math.MaxInt64}, {0, -50400000000}} {
		got, err := call.Call(t.Context(), []model.Value{timestampDatum(tc.date)}, builtin.CallBudget{})
		if err != nil || got.IntVal != tc.want {
			t.Fatalf("date timezone(%d)=%v,%v", tc.date, got, err)
		}
	}
	for _, date := range []int64{-2451545, 106751983, math.MaxInt64} {
		_, err := call.Call(t.Context(), []model.Value{timestampDatum(date)}, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
	}
}
