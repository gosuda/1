package builtin_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func intervalArithmeticCallable(t *testing.T, id model.FunctionID) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func intervalArithmeticDatum(value model.Interval) model.Value {
	return model.Value{Type: model.TypeInterval, IntervalVal: value}
}

func requireIntervalArithmeticResult(t *testing.T, got model.Value, err error, want model.Interval) {
	t.Helper()
	if err != nil || got.IsNull || got.Type != model.TypeInterval || got.IntervalVal != want {
		t.Fatalf("interval = %+v, %v; want %+v", got, err, want)
	}
}

func TestIntervalAdditionPreservesIndependentFields(t *testing.T) {
	left := intervalArithmeticDatum(model.Interval{Months: 1, Days: 29, Microseconds: 90_000_000_000})
	right := intervalArithmeticDatum(model.Interval{Months: -2, Days: 3, Microseconds: -3_600_000_000})
	got, err := intervalArithmeticCallable(t, 1169).Call(t.Context(), []model.Value{left, right}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireIntervalArithmeticResult(t, got, err, model.Interval{Months: -1, Days: 32, Microseconds: 86_400_000_000})
}

func TestIntervalArithmeticPreservesIndividualMinima(t *testing.T) {
	for _, test := range []struct {
		name  string
		value model.Interval
	}{
		{"month", model.Interval{Months: math.MinInt32}},
		{"day", model.Interval{Days: math.MinInt32}},
		{"microsecond", model.Interval{Microseconds: math.MinInt64}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := intervalArithmeticDatum(test.value)
			got, err := intervalArithmeticCallable(t, 1169).Call(t.Context(), []model.Value{value, intervalArithmeticDatum(model.Interval{})}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireIntervalArithmeticResult(t, got, err, test.value)
			got, err = intervalArithmeticCallable(t, 1170).Call(t.Context(), []model.Value{value, value}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireIntervalArithmeticResult(t, got, err, model.Interval{})
			_, err = intervalArithmeticCallable(t, 1168).Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22008")
		})
	}
}

func TestIntervalArithmeticRejectsComponentOverflow(t *testing.T) {
	for _, test := range []struct {
		name        string
		id          model.FunctionID
		left, right model.Interval
	}{
		{"month addition", 1169, model.Interval{Months: math.MaxInt32}, model.Interval{Months: 1}},
		{"day addition", 1169, model.Interval{Days: math.MaxInt32}, model.Interval{Days: 1}},
		{"time addition", 1169, model.Interval{Microseconds: math.MaxInt64}, model.Interval{Microseconds: 1}},
		{"month subtraction", 1170, model.Interval{Months: math.MinInt32}, model.Interval{Months: 1}},
		{"day subtraction", 1170, model.Interval{Days: math.MinInt32}, model.Interval{Days: 1}},
		{"time subtraction", 1170, model.Interval{Microseconds: math.MinInt64}, model.Interval{Microseconds: 1}},
		{"subtract minimum time", 1170, model.Interval{}, model.Interval{Microseconds: math.MinInt64}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := intervalArithmeticCallable(t, test.id).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.left), intervalArithmeticDatum(test.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22008")
		})
	}
}

func TestIntervalFiniteArithmeticCannotProduceInfinitySentinels(t *testing.T) {
	for _, test := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
	}{
		{"positive addition", 1169, []model.Value{
			intervalArithmeticDatum(model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64 - 1}),
			intervalArithmeticDatum(model.Interval{Microseconds: 1}),
		}},
		{"negative addition", 1169, []model.Value{
			intervalArithmeticDatum(model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64 + 1}),
			intervalArithmeticDatum(model.Interval{Microseconds: -1}),
		}},
		{"negation", 1168, []model.Value{
			intervalArithmeticDatum(model.Interval{Months: -math.MaxInt32, Days: -math.MaxInt32, Microseconds: -math.MaxInt64}),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := intervalArithmeticCallable(t, test.id).Call(t.Context(), test.args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22008")
		})
	}
}

func TestIntervalInfinityAdditionAndSubtraction(t *testing.T) {
	positive := model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}
	negative := model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}
	for _, test := range []struct {
		name              string
		id                model.FunctionID
		left, right, want model.Interval
		invalid           bool
	}{
		{"same infinities add", 1169, positive, positive, positive, false},
		{"opposite infinities add", 1169, positive, negative, model.Interval{}, true},
		{"same infinities subtract", 1170, negative, negative, model.Interval{}, true},
		{"opposite infinities subtract", 1170, negative, positive, negative, false},
		{"finite minus infinity", 1170, model.Interval{Months: math.MinInt32}, positive, negative, false},
		{"finite plus infinity", 1169, model.Interval{Microseconds: math.MaxInt64}, negative, negative, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := intervalArithmeticCallable(t, test.id).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.left), intervalArithmeticDatum(test.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if test.invalid {
				requireSQLState(t, err, "22008")
				return
			}
			requireIntervalArithmeticResult(t, got, err, test.want)
		})
	}
	got, err := intervalArithmeticCallable(t, 1168).Call(t.Context(), []model.Value{intervalArithmeticDatum(negative)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireIntervalArithmeticResult(t, got, err, positive)
}

func TestIntervalScalingCascadesFractionsWithoutPromotingTime(t *testing.T) {
	for _, test := range []struct {
		name   string
		id     model.FunctionID
		input  model.Interval
		factor float64
		want   model.Interval
	}{
		{"multiply", 1618, model.Interval{Months: 41, Days: 12, Microseconds: 1_296_000_000_000}, 0.3, model.Interval{Months: 12, Days: 12, Microseconds: 440_640_000_000}},
		{"reversed operands", 1624, model.Interval{Months: 41, Days: 12, Microseconds: 1_296_000_000_000}, 0.3, model.Interval{Months: 12, Days: 12, Microseconds: 440_640_000_000}},
		{"divide mixed signs", 1326, model.Interval{Months: 9, Days: -27, Microseconds: 45_296_000_000}, 10, model.Interval{Days: 25, Microseconds: -55_950_400_000}},
		{"rounded whole month fraction", 1618, model.Interval{Months: 14}, 0.3, model.Interval{Months: 4, Days: 6}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []model.Value{intervalArithmeticDatum(test.input), model.Float64Val(test.factor)}
			if test.id == 1624 {
				args[0], args[1] = args[1], args[0]
			}
			got, err := intervalArithmeticCallable(t, test.id).Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireIntervalArithmeticResult(t, got, err, test.want)
		})
	}
}

func TestIntervalScalingRoundsMicrosecondTiesToEven(t *testing.T) {
	for _, test := range []struct {
		input, want int64
	}{
		{1, 0}, {3, 2}, {-3, -2},
	} {
		got, err := intervalArithmeticCallable(t, 1618).Call(t.Context(), []model.Value{
			intervalArithmeticDatum(model.Interval{Microseconds: test.input}), model.Float64Val(0.5),
		}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireIntervalArithmeticResult(t, got, err, model.Interval{Microseconds: test.want})
	}
}

func TestIntervalScalingDistinguishesFiniteMinimaAndPositiveOverflow(t *testing.T) {
	for _, value := range []model.Interval{{Months: math.MinInt32}, {Days: math.MinInt32}, {Microseconds: math.MinInt64}} {
		got, err := intervalArithmeticCallable(t, 1618).Call(t.Context(), []model.Value{intervalArithmeticDatum(value), model.Float64Val(1)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireIntervalArithmeticResult(t, got, err, value)
	}
	for _, test := range []struct {
		name   string
		id     model.FunctionID
		value  model.Interval
		factor float64
	}{
		{"positive time boundary", 1618, model.Interval{Microseconds: 256}, 0x1p55},
		{"positive month boundary", 1618, model.Interval{Months: 1}, 0x1p31},
		{"fractional cascade day overflow", 1618, model.Interval{Months: 1, Days: 2_146_410}, 1000.5002},
		{"division overflow", 1326, model.Interval{Microseconds: 4_611_686_018_427_387_904}, 0.1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := intervalArithmeticCallable(t, test.id).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.value), model.Float64Val(test.factor)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22008")
		})
	}
}

func TestIntervalScalingSpecialValueErrorPrecedence(t *testing.T) {
	positive := model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}
	for _, test := range []struct {
		name   string
		id     model.FunctionID
		value  model.Interval
		factor float64
		state  string
	}{
		{"infinity divided by zero", 1326, positive, 0, "22012"},
		{"infinity divided by negative zero", 1326, positive, math.Copysign(0, -1), "22012"},
		{"infinity times zero", 1618, positive, 0, "22008"},
		{"infinity divided by infinity", 1326, positive, math.Inf(1), "22008"},
		{"infinity times NaN", 1618, positive, math.NaN(), "22008"},
		{"finite divided by NaN", 1326, model.Interval{Days: 1}, math.NaN(), "22008"},
		{"net zero times infinity", 1618, model.Interval{Months: 1, Days: -30}, math.Inf(1), "22008"},
		{"finite result rounds to infinity", 1618, model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64 + 1}, 1, "22008"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := intervalArithmeticCallable(t, test.id).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.value), model.Float64Val(test.factor)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, test.state)
		})
	}
}

func TestIntervalScalingInfinityUsesNetIntervalSign(t *testing.T) {
	positive := model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}
	negative := model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}
	for _, test := range []struct {
		name   string
		id     model.FunctionID
		value  model.Interval
		factor float64
		want   model.Interval
	}{
		{"net negative", 1618, model.Interval{Months: 1, Days: -31}, math.Inf(1), negative},
		{"negative factor", 1618, model.Interval{Months: 1, Days: -31}, math.Inf(-1), positive},
		{"infinite interval", 1618, positive, math.Inf(-1), negative},
		{"infinite dividend", 1326, negative, -2, positive},
		{"infinite divisor", 1326, model.Interval{Months: math.MinInt32, Microseconds: math.MinInt64}, math.Inf(-1), model.Interval{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := intervalArithmeticCallable(t, test.id).Call(t.Context(), []model.Value{intervalArithmeticDatum(test.value), model.Float64Val(test.factor)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireIntervalArithmeticResult(t, got, err, test.want)
		})
	}
}

func TestIntervalDivisionHonorsCancellationBeforeArithmeticErrors(t *testing.T) {
	function := intervalArithmeticCallable(t, 1326)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := function.Call(ctx, []model.Value{intervalArithmeticDatum(model.Interval{}), model.Float64Val(0)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled division error = %v; want context.Canceled", err)
	}
}

func TestIntervalScalingPreservesPinnedFusedRemainders(t *testing.T) {
	input := model.Interval{Months: math.MaxInt32, Days: math.MinInt32, Microseconds: 1}
	got, err := intervalArithmeticCallable(t, 1618).Call(t.Context(), []model.Value{intervalArithmeticDatum(input), model.Float64Val(0.1)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireIntervalArithmeticResult(t, got, err, model.Interval{Months: 214748364, Days: -214748343, Microseconds: -69120001030})
}
