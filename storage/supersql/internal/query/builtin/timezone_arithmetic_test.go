package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimezoneArithmeticLookupPrecedesNegationAndInfinity(t *testing.T) {
	call := jsonCallable(t, 6273)
	args := []model.Value{timestampDatum(math.MaxInt64), {Type: model.TypeInterval, IntervalVal: model.Interval{Months: math.MinInt32}}, model.StringVal("missing/zone")}
	_, err := call.Call(t.Context(), args, builtin.CallBudget{})
	requireSQLState(t, err, "22023")
	args[2] = model.StringVal("UTC")
	_, err = call.Call(t.Context(), args, builtin.CallBudget{})
	requireSQLState(t, err, "22008")
	args[1].IntervalVal = model.Interval{Months: 1}
	got, err := call.Call(t.Context(), args, builtin.CallBudget{})
	if err != nil || got.IntVal != math.MaxInt64 {
		t.Fatalf("finite subtraction from infinity=%v,%v", got, err)
	}
}

func TestTimezoneArithmeticRejectsOppositeInfinities(t *testing.T) {
	negative := model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}}
	_, err := jsonCallable(t, 1189).Call(t.Context(), []model.Value{timestampDatum(math.MaxInt64), negative}, builtin.CallBudget{})
	requireSQLState(t, err, "22008")
}
