package builtin_test

import (
	"encoding/hex"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestIntervalExtractionPreservesFractionalScaleAndEpochYears(t *testing.T) {
	function := intervalArithmeticCallable(t, 6204)
	for _, tc := range []struct {
		name, unit string
		value      model.Interval
		want       string
	}{
		{"negative fractional seconds", "second", model.Interval{Microseconds: -1}, "-0.000001"},
		{"fractional milliseconds", "milliseconds", model.Interval{Microseconds: 61_234_567}, "1234.567"},
		{"year epoch", "epoch", model.Interval{Months: 12, Microseconds: -1}, "31557599.999999"},
		{"equal comparison span different epoch", "epoch", model.Interval{Days: 360}, "31104000.000000"},
		{"negative year quarter", "quarter", model.Interval{Months: -12}, "-1"},
		{"minimum month quarter width", "quarter", model.Interval{Months: math.MinInt32}, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []model.Value{model.StringVal(tc.unit), intervalArithmeticDatum(tc.value)}
			result, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if result.Type != model.TypeNumeric || result.StrVal != tc.want {
				t.Fatalf("extract %s=%v, want %s", tc.unit, result, tc.want)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestIntervalTruncationDoesNotRoundNegativeFractions(t *testing.T) {
	function := intervalArithmeticCallable(t, 1218)
	input := model.Interval{Months: -14, Days: 3, Microseconds: -1_234_567}
	result, err := function.Call(t.Context(), []model.Value{model.StringVal("millisecond"), intervalArithmeticDatum(input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireIntervalArithmeticResult(t, result, err, model.Interval{Months: -14, Days: 3, Microseconds: -1_234_000})
	result, err = function.Call(t.Context(), []model.Value{model.StringVal("quarter"), intervalArithmeticDatum(input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireIntervalArithmeticResult(t, result, err, model.Interval{Months: -12})
}

func TestIntervalInfiniteExtractionDistinguishesOscillatingFields(t *testing.T) {
	infinity := model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}
	function := intervalArithmeticCallable(t, 6204)
	result, err := function.Call(t.Context(), []model.Value{model.StringVal("second"), intervalArithmeticDatum(infinity)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsNull || result.Type != model.TypeNumeric {
		t.Fatalf("infinite seconds=%v, want numeric NULL", result)
	}
	result, err = function.Call(t.Context(), []model.Value{model.StringVal("hour"), intervalArithmeticDatum(infinity)}, builtin.CallBudget{OutputBytes: 9, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.StrVal != "-Infinity" {
		t.Fatalf("infinite hours=%v", result)
	}
	_, err = intervalArithmeticCallable(t, 1218).Call(t.Context(), []model.Value{model.StringVal("week"), intervalArithmeticDatum(infinity)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "0A000")
}

func TestIntervalExtremaChooseRightRepresentationOnEquality(t *testing.T) {
	left, right := model.Interval{Months: 1}, model.Interval{Days: 30}
	for _, id := range []model.FunctionID{1197, 1198} {
		result, err := intervalArithmeticCallable(t, id).Call(t.Context(), []model.Value{intervalArithmeticDatum(left), intervalArithmeticDatum(right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireIntervalArithmeticResult(t, result, err, right)
	}
}

func TestIntervalBinaryOutputPreservesIndependentSignedFields(t *testing.T) {
	function, err := builtin.Resolve(2479, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	args := []model.Value{intervalArithmeticDatum(model.Interval{Months: 1, Days: -2, Microseconds: -1})}
	result, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 16, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(result.BytesVal); got != "fffffffffffffffffffffffe00000001" {
		t.Fatalf("interval binary=%s", got)
	}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 15, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestIntervalExtractionAliasDoesNotExtendTruncationUnits(t *testing.T) {
	arguments := []model.Value{model.StringVal("mm"), intervalArithmeticDatum(model.Interval{Microseconds: 120_000_000})}
	value, err := intervalArithmeticCallable(t, 1172).Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if value.FloatVal != 2 {
		t.Fatalf("extracted minutes=%v, want 2", value)
	}
	_, err = intervalArithmeticCallable(t, 1218).Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22023")
}
