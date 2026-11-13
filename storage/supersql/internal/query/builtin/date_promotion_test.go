package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestDatePartPromotesBeforeExtractingUnits(t *testing.T) {
	call := jsonCallable(t, 1384)
	for _, tc := range []struct {
		days       int64
		unit, code string
	}{{-2451546, "hour", "22008"}, {-2451546, "unknown_unit", "22023"}, {106751983, "unknown_unit", "22008"}} {
		_, err := call.Call(t.Context(), []model.Value{model.StringVal(tc.unit), timestampDatum(tc.days)}, builtin.CallBudget{})
		requireSQLState(t, err, tc.code)
	}
	got, err := call.Call(t.Context(), []model.Value{model.StringVal("epoch"), timestampDatum(-2451546)}, builtin.CallBudget{})
	if err != nil || got.FloatVal != -210866889600 {
		t.Fatalf("projected date epoch=%v,%v", got, err)
	}
}
