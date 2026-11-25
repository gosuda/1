package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestIntervalJustificationAlignsOnlyRequestedFields(t *testing.T) {
	for _, test := range []struct {
		name        string
		id          model.FunctionID
		value, want model.Interval
	}{
		{"hours retain months", 1175, model.Interval{Months: 2, Days: -1, Microseconds: 1}, model.Interval{Months: 2, Microseconds: -86_399_999_999}},
		{"days retain time", 1295, model.Interval{Months: -1, Days: 1, Microseconds: 1}, model.Interval{Days: -29, Microseconds: 1}},
		{"full positive month negative time", 2711, model.Interval{Months: 1, Microseconds: -3_600_000_000}, model.Interval{Days: 29, Microseconds: 82_800_000_000}},
		{"full negative month positive time", 2711, model.Interval{Months: -1, Microseconds: 3_600_000_000}, model.Interval{Days: -29, Microseconds: -82_800_000_000}},
		{"minimum time", 1175, model.Interval{Microseconds: math.MinInt64}, model.Interval{Days: -106_751_991, Microseconds: -14_454_775_808}},
		{"minimum day", 1295, model.Interval{Days: math.MinInt32}, model.Interval{Months: -71_582_788, Days: -8}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := intervalArithmeticCallable(t, test.id).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.value)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireIntervalArithmeticResult(t, got, err, test.want)
		})
	}
}

func TestIntervalJustificationReducesDaysBeforeOverflow(t *testing.T) {
	for _, test := range []struct {
		name        string
		value, want model.Interval
	}{
		{"maximum days plus whole day", model.Interval{Days: math.MaxInt32, Microseconds: 86_400_000_000}, model.Interval{Months: 71_582_788, Days: 8}},
		{"minimum days minus whole day", model.Interval{Days: math.MinInt32, Microseconds: -86_400_000_000}, model.Interval{Months: -71_582_788, Days: -9}},
		{"negative time avoids positive month overflow", model.Interval{Months: math.MaxInt32, Days: 30, Microseconds: -86_400_000_000}, model.Interval{Months: math.MaxInt32, Days: 29}},
		{"positive time avoids negative month overflow", model.Interval{Months: math.MinInt32, Days: -30, Microseconds: 86_400_000_000}, model.Interval{Months: math.MinInt32, Days: -29}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := intervalArithmeticCallable(t, 2711).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.value)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireIntervalArithmeticResult(t, got, err, test.want)
		})
	}
}

func TestIntervalJustificationRejectsUnrepresentableComponents(t *testing.T) {
	for _, test := range []struct {
		name  string
		id    model.FunctionID
		value model.Interval
	}{
		{"hours positive day overflow", 1175, model.Interval{Days: math.MaxInt32, Microseconds: 86_400_000_000}},
		{"hours negative day overflow", 1175, model.Interval{Days: math.MinInt32, Microseconds: -86_400_000_000}},
		{"days positive month overflow", 1295, model.Interval{Months: math.MaxInt32, Days: 30}},
		{"days negative month overflow", 1295, model.Interval{Months: math.MinInt32, Days: -30}},
		{"full month overflow after time promotion", 2711, model.Interval{Months: math.MaxInt32, Days: -30, Microseconds: 5_184_000_000_000}},
		{"full negative month overflow after time promotion", 2711, model.Interval{Months: math.MinInt32, Days: 30, Microseconds: -5_184_000_000_000}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := intervalArithmeticCallable(t, test.id).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.value)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22008")
		})
	}
}

func TestIntervalJustificationPreservesInfinities(t *testing.T) {
	for _, id := range []model.FunctionID{1175, 1295, 2711} {
		for _, value := range []model.Interval{
			{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64},
			{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64},
		} {
			got, err := intervalArithmeticCallable(t, id).Call(t.Context(), []model.Value{intervalArithmeticDatum(value)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireIntervalArithmeticResult(t, got, err, value)
		}
	}
}
