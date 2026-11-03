package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimestamptzFieldsPreserveSignedOffsetParts(t *testing.T) {
	for _, tc := range []struct{ unit, want string }{{"timezone", "-12630"}, {"timezone_hour", "-3"}, {"timezone_minute", "-30"}} {
		call, err := builtin.Resolve(6203, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "<-03:30:30>03:30:30", Fixed: true, OffsetSeconds: -12630}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := call.Call(t.Context(), []model.Value{model.StringVal(tc.unit), timestampDatum(0)}, builtin.CallBudget{OutputBytes: 64, MemoryBytes: 4096})
		if err != nil || got.StrVal != tc.want {
			t.Fatalf("%s=%v,%v; want%s", tc.unit, got, err, tc.want)
		}
	}
}

func TestTimestamptzFieldsInfinityAndCalendarErrorPrecedence(t *testing.T) {
	for _, id := range []model.FunctionID{1171, 6203} {
		call := jsonCallable(t, id)
		for _, unit := range []string{"timezone", "timezone_hour", "timezone_minute", "second", "dow"} {
			got, err := call.Call(t.Context(), []model.Value{model.StringVal(unit), timestampDatum(math.MaxInt64)}, builtin.CallBudget{})
			if err != nil || !got.IsNull {
				t.Fatalf("infinite%s=%v,%v", unit, got, err)
			}
		}
		_, err := call.Call(t.Context(), []model.Value{model.StringVal("unknown_unit"), timestampDatum(-211813488000000001)}, builtin.CallBudget{})
		requireSQLState(t, err, "22023")
		_, err = call.Call(t.Context(), []model.Value{model.StringVal("timezone"), timestampDatum(-211813488000000001)}, builtin.CallBudget{})
		requireSQLState(t, err, "22008")
	}
}
