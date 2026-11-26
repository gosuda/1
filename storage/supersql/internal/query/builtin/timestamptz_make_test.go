package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestMakeTimestamptzNumericZoneUsesISOSign(t *testing.T) {
	call, err := builtin.Resolve(3463, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		zone string
		want int64
	}{
		{"+03", -10800000000},
		{"UTC+3", 10800000000},
		{"-03:30:30", 12630000000},
		{"+123", -4980000000},
		{"+ 05", -300000000},
		{"+::", 0},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			args := []model.Value{model.Int64Val(2000), model.Int64Val(1), model.Int64Val(1), model.Int64Val(0), model.Int64Val(0), model.Float64Val(0), model.StringVal(tc.zone)}
			got, err := call.Call(t.Context(), args, builtin.CallBudget{})
			if err != nil || got.IntVal != tc.want {
				t.Fatalf("zone %q: got %v,%v; want instant %d", tc.zone, got, err, tc.want)
			}
		})
	}
}

func TestMakeTimestamptzValidatesCivilDateBeforeZone(t *testing.T) {
	call, err := builtin.Resolve(3463, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	args := []model.Value{model.Int64Val(2024), model.Int64Val(2), model.Int64Val(30), model.Int64Val(0), model.Int64Val(0), model.Float64Val(0), model.StringVal("+16x")}
	_, err = call.Call(t.Context(), args, builtin.CallBudget{})
	requireSQLState(t, err, "22008")
	args[2].IntVal = 29
	_, err = call.Call(t.Context(), args, builtin.CallBudget{})
	requireSQLState(t, err, "22023")
}
