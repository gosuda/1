package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimePrecisionRoundingPreservesDayEndpoint(t *testing.T) {
	for _, tc := range []struct{ value, precision, want int64 }{{499999, 0, 0}, {500000, 0, 1000000}, {125000, 2, 130000}, {86399999999, 0, 86400000000}, {86400000000, 3, 86400000000}, {123456, 6, 123456}, {123456, -1, 123456}, {123456, 7, 123456}} {
		value, err := jsonCallable(t, 1968).Call(t.Context(), []model.Value{{Type: model.TypeTimestamp, IntVal: tc.value}, model.Int64Val(tc.precision)}, builtin.CallBudget{})
		if err != nil || value.IntVal != tc.want {
			t.Errorf("time precision value=%d precision=%d result=%v err=%v", tc.value, tc.precision, value, err)
		}
	}
}

func TestTimePrecisionDeclarationClampsWithWarning(t *testing.T) {
	modifier, warning, err := builtin.TimePrecision(7)
	if err != nil || modifier != 6 || warning == nil || warning.Code != "22023" {
		t.Fatalf("time(7)=%d,%v,%v", modifier, warning, err)
	}
	_, _, err = builtin.TimePrecision(-1)
	requireSQLState(t, err, "22023")
}
