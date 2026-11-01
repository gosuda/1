package builtin_test

import (
	"math"
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTemporalOverlapsEndpointSemantics(t *testing.T) {
	n := model.NullVal(model.TypeTimestamp)
	v := func(x int64) model.Value { return model.Value{Type: model.TypeTimestamp, IntVal: x} }
	for _, id := range []model.FunctionID{1304, 1308, 2041} {
		call := jsonCallable(t, id)
		for _, tc := range []struct {
			args []model.Value
			want model.Value
		}{
			{[]model.Value{v(0), v(2), v(2), v(3)}, model.BoolVal(false)},
			{[]model.Value{v(2), v(0), v(1), v(3)}, model.BoolVal(true)},
			{[]model.Value{v(1), v(1), v(1), v(1)}, model.BoolVal(true)},
			{[]model.Value{v(0), v(2), v(1), n}, model.BoolVal(true)},
			{[]model.Value{v(0), v(2), v(2), n}, model.NullVal(model.TypeBoolean)},
			{[]model.Value{n, v(0), v(0), v(2)}, model.NullVal(model.TypeBoolean)},
			{[]model.Value{n, n, v(0), v(2)}, model.NullVal(model.TypeBoolean)},
		} {
			for swap := 0; swap < 2; swap++ {
				got, err := call.Call(t.Context(), tc.args, builtin.CallBudget{})
				if err != nil || got.IsNull != tc.want.IsNull || got.IntVal != tc.want.IntVal {
					t.Fatalf("overlaps%d(%v)=%v,%v; want %v", id, tc.args, got, err, tc.want)
				}
				tc.args[0], tc.args[1], tc.args[2], tc.args[3] = tc.args[2], tc.args[3], tc.args[0], tc.args[1]
			}
		}
	}
}

func TestTemporalOverlapsDurationOverloads(t *testing.T) {
	for _, id := range []model.FunctionID{1309, 1310, 1311, 2042, 2043, 2044} {
		args := []model.Value{{Type: model.TypeTimestamp, IntVal: 10}, {Type: model.TypeTimestamp, IntVal: 0}, {Type: model.TypeTimestamp, IntVal: 5}, {Type: model.TypeTimestamp, IntVal: 15}}
		if id == 1309 || id == 1311 || id == 2042 || id == 2044 {
			args[1] = model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: -10}}
		}
		if id == 1309 || id == 1310 || id == 2042 || id == 2043 {
			args[3] = model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: 10}}
		}
		got, err := jsonCallable(t, id).Call(t.Context(), args, builtin.CallBudget{})
		if err != nil || got.IsNull || got.IntVal != model.BoolVal(true).IntVal {
			t.Fatalf("overlaps%d=%v,%v", id, got, err)
		}
	}
}

func TestTemporalOverlapsEvaluatesDurationBeforeNullResult(t *testing.T) {
	call := jsonCallable(t, 1310)
	args := []model.Value{model.NullVal(model.TypeTimestamp), model.NullVal(model.TypeTimestamp), {Type: model.TypeTimestamp}, {Type: model.TypeInterval, IntervalVal: model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}}}
	_, err := call.Call(t.Context(), args, builtin.CallBudget{})
	requireSQLState(t, err, "22008")
	args[2].IsNull = true
	result, err := call.Call(t.Context(), args, builtin.CallBudget{})
	if err != nil || !result.IsNull {
		t.Fatalf("strict duration addition with NULL start: %v, %v", result, err)
	}
}
