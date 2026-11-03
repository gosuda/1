package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func makeIntervalValues() []model.Value {
	return []model.Value{model.Int64Val(0), model.Int64Val(0), model.Int64Val(0), model.Int64Val(0), model.Int64Val(0), model.Int64Val(0), model.Float64Val(0)}
}

func TestMakeIntervalPreservesComponentsAndRoundsSeconds(t *testing.T) {
	values := []model.Value{model.Int64Val(1), model.Int64Val(-2), model.Int64Val(3), model.Int64Val(-4), model.Int64Val(5), model.Int64Val(-6), model.Float64Val(0.0000015)}
	result, err := intervalArithmeticCallable(t, 3464).Call(t.Context(), values, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireIntervalArithmeticResult(t, result, err, model.Interval{Months: 10, Days: 17, Microseconds: 17_640_000_002})
}

func TestMakeIntervalRejectsIntermediateOverflowBeforeCancellation(t *testing.T) {
	function := intervalArithmeticCallable(t, 3464)
	for _, tc := range []struct {
		name                string
		position            int
		value, compensation int64
	}{
		{"year conversion", 0, 178956971, -20},
		{"week conversion", 2, 306783379, -20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := makeIntervalValues()
			values[tc.position] = model.Int64Val(tc.value)
			values[tc.position+1] = model.Int64Val(tc.compensation)
			_, err := function.Call(t.Context(), values, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22008")
		})
	}
}

func TestMakeIntervalDistinguishesFloatAndIntervalOverflow(t *testing.T) {
	function := intervalArithmeticCallable(t, 3464)
	for _, tc := range []struct {
		name    string
		seconds float64
		code    string
	}{
		{"nonfinite", math.Inf(1), "22008"},
		{"NaN", math.NaN(), "22008"},
		{"float multiplication", math.MaxFloat64, "22003"},
		{"microsecond range", 0x1p63 / 1e6, "22008"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := makeIntervalValues()
			values[6] = model.Float64Val(tc.seconds)
			_, err := function.Call(t.Context(), values, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.code)
		})
	}
}

func TestMakeIntervalNullSkipsNonfiniteSeconds(t *testing.T) {
	values := makeIntervalValues()
	values[0] = model.NullVal(model.TypeInt64)
	values[6] = model.Float64Val(math.NaN())
	result, err := intervalArithmeticCallable(t, 3464).Call(t.Context(), values, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsNull || result.Type != model.TypeInterval {
		t.Fatalf("NULL constructor=%v", result)
	}
}
