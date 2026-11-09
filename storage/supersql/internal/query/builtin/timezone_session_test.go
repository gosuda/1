package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestSessionTimezoneCastInfinityAndRange(t *testing.T) {
	for _, id := range []model.FunctionID{2027, 2028, 6334, 6335} {
		call, err := builtin.Resolve(id, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "<+14>-14"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []int64{math.MinInt64, math.MaxInt64} {
			got, err := call.Call(t.Context(), []model.Value{timestampDatum(value)}, builtin.CallBudget{})
			if err != nil || got.IntVal != value {
				t.Fatalf("session cast%d infinity=%v,%v", id, got, err)
			}
		}
		value := int64(-211813488000000000)
		if id == 2027 || id == 6334 {
			value = 9223371331199999999
		}
		_, err = call.Call(t.Context(), []model.Value{timestampDatum(value)}, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
	}
}
