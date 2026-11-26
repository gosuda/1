package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestDateTimeTimezoneConstructorChecksIntermediateTimestamp(t *testing.T) {
	call, err := builtin.Resolve(1176, builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "<-30>30"}})
	if err != nil {
		t.Fatal(err)
	}
	args := []model.Value{timestampDatum(-2451546), timestampDatum(0)}
	_, err = call.Call(t.Context(), args, builtin.CallBudget{})
	requireSQLState(t, err, "22008")
	args[1].IntVal = 86400000000
	got, err := call.Call(t.Context(), args, builtin.CallBudget{})
	if err != nil || got.IntVal != -211813380000000000 {
		t.Fatalf("time restores midnight before zone conversion: %v,%v", got, err)
	}
	args[0].IntVal = math.MaxInt32
	got, err = call.Call(t.Context(), args, builtin.CallBudget{})
	if err != nil || got.IntVal != math.MaxInt64 {
		t.Fatalf("date infinity=%v,%v", got, err)
	}
}
