package builtin_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func advanceNumericMoments(t *testing.T, moments *builtin.NumericMoments, values ...model.Value) {
	t.Helper()
	for _, value := range values {
		if err := moments.Advance(t.Context(), value, 1<<20); err != nil {
			t.Fatalf("advance numeric moments with %v: %v", value, err)
		}
	}
}

func requireNumericMoment(t *testing.T, moments *builtin.NumericMoments, statistic builtin.NumericStatistic, want string) model.Value {
	t.Helper()
	got, err := moments.Result(t.Context(), statistic, 1<<20)
	if err != nil || got.Type != model.TypeNumeric || got.IsNull || got.StrVal != want {
		t.Fatalf("numeric statistic %d = %v, %v; want %s", statistic, got, err, want)
	}
	return got
}

func TestNumericMomentsPreserveDifferencesBeyondFloatPrecision(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []model.Value
	}{
		{"integers", []model.Value{model.Int64Val(math.MaxInt64 - 2), model.Int64Val(math.MaxInt64 - 1), model.Int64Val(math.MaxInt64)}},
		{"numeric", []model.Value{numericDatum("1000000000000000000000000000001"), numericDatum("1000000000000000000000000000002"), numericDatum("1000000000000000000000000000003")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var moments builtin.NumericMoments
			advanceNumericMoments(t, &moments, test.values...)
			requireNumericMoment(t, &moments, builtin.NumericVariancePopulation, "0.66666666666666666667")
			requireNumericMoment(t, &moments, builtin.NumericVarianceSample, "1.00000000000000000000")
			requireNumericMoment(t, &moments, builtin.NumericStddevPopulation, "0.81649658092772603273")
			requireNumericMoment(t, &moments, builtin.NumericStddevSample, "1.00000000000000000000")
		})
	}
}

func TestNumericMomentsRetainSquaredScalesBeyondNumericStorage(t *testing.T) {
	for _, test := range []struct {
		name, left, right, variance, deviation string
	}{
		{"tiny numerator survives", "1e-16383", "2e-16383", "0." + strings.Repeat("0", 1000), "0." + strings.Repeat("0", 1000)},
		{"independent moment alignment", "1e-16383", "1", "0.25" + strings.Repeat("0", 998), "0.5" + strings.Repeat("0", 999)},
		{"large display scale", "1." + strings.Repeat("0", 9000), "3", "1." + strings.Repeat("0", 1000), "1." + strings.Repeat("0", 1000)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var moments builtin.NumericMoments
			advanceNumericMoments(t, &moments, numericDatum(test.left), numericDatum(test.right))
			requireNumericMoment(t, &moments, builtin.NumericVariancePopulation, test.variance)
			requireNumericMoment(t, &moments, builtin.NumericStddevPopulation, test.deviation)
		})
	}
}

func TestNumericMomentsZeroHasCanonicalScale(t *testing.T) {
	var moments builtin.NumericMoments
	advanceNumericMoments(t, &moments, numericDatum("-12.3400"), numericDatum("-12.34"))
	for _, statistic := range []builtin.NumericStatistic{
		builtin.NumericVariancePopulation, builtin.NumericVarianceSample,
		builtin.NumericStddevPopulation, builtin.NumericStddevSample,
	} {
		requireNumericMoment(t, &moments, statistic, "0")
	}
}

func TestNumericMomentsNullCountPrecedesNonfiniteResults(t *testing.T) {
	for _, special := range []string{"NaN", "Infinity", "-Infinity"} {
		t.Run(special, func(t *testing.T) {
			var moments builtin.NumericMoments
			advanceNumericMoments(t, &moments, model.NullVal(model.TypeInt64), model.NullVal(model.TypeNumeric))
			for _, statistic := range []builtin.NumericStatistic{
				builtin.NumericVariancePopulation, builtin.NumericVarianceSample,
				builtin.NumericStddevPopulation, builtin.NumericStddevSample,
			} {
				got, err := moments.Result(t.Context(), statistic, 0)
				if err != nil || !got.IsNull || got.Type != model.TypeNumeric {
					t.Fatalf("empty statistic %d = %v, %v; want numeric NULL", statistic, got, err)
				}
			}
			advanceNumericMoments(t, &moments, numericDatum(special), model.NullVal(model.TypeNumeric))
			for _, statistic := range []builtin.NumericStatistic{builtin.NumericVarianceSample, builtin.NumericStddevSample} {
				got, err := moments.Result(t.Context(), statistic, 0)
				if err != nil || !got.IsNull || got.Type != model.TypeNumeric {
					t.Fatalf("singleton sample %d = %v, %v; want numeric NULL", statistic, got, err)
				}
			}
			requireNumericMoment(t, &moments, builtin.NumericVariancePopulation, "NaN")
			requireNumericMoment(t, &moments, builtin.NumericStddevPopulation, "NaN")
			_, err := moments.Result(t.Context(), builtin.NumericVariancePopulation, 2)
			requireSQLState(t, err, "54000")
			advanceNumericMoments(t, &moments, model.Int64Val(7))
			requireNumericMoment(t, &moments, builtin.NumericVarianceSample, "NaN")
			requireNumericMoment(t, &moments, builtin.NumericStddevSample, "NaN")
		})
	}
}

func TestNumericMomentsFailedAdvancePreservesState(t *testing.T) {
	for _, test := range []struct {
		name   string
		value  model.Value
		budget uint64
		state  string
	}{
		{"struct budget", model.Int64Val(5), uint64(unsafe.Sizeof(builtin.NumericMoments{})) - 1, "54000"},
		{"nonfinite state budget", numericDatum("Infinity"), 0, "54000"},
		{"coefficient capacity budget", numericDatum("1e1000"), 1024, "54000"},
		{"squared work limit", numericDatum("1e20000"), 1 << 20, "54000"},
		{"alignment work limit", numericDatum("1e-16383"), 1 << 20, "54000"},
		{"malformed numeric", numericDatum("1.2.3"), 1 << 20, "22P02"},
		{"external scale limit", numericDatum("1e-20000"), 1 << 20, "22003"},
		{"wrong storage", model.Float64Val(5), 1 << 20, "42804"},
		{"wrong null storage", model.NullVal(model.TypeString), 1 << 20, "42804"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var moments builtin.NumericMoments
			advanceNumericMoments(t, &moments, model.Int64Val(10), model.Int64Val(12))
			before := requireNumericMoment(t, &moments, builtin.NumericVariancePopulation, "1.00000000000000000000")
			err := moments.Advance(t.Context(), test.value, test.budget)
			requireSQLState(t, err, test.state)
			requireNumericMoment(t, &moments, builtin.NumericVariancePopulation, before.StrVal)
			advanceNumericMoments(t, &moments, model.Int64Val(14))
			requireNumericMoment(t, &moments, builtin.NumericVarianceSample, "4.0000000000000000")
			if before.StrVal != "1.00000000000000000000" {
				t.Fatalf("advancing mutated prior output: %v", before)
			}
		})
	}
}

func TestNumericMomentsCancellationAndResultBudgetPreserveState(t *testing.T) {
	var moments builtin.NumericMoments
	advanceNumericMoments(t, &moments, model.Int64Val(1), numericDatum("3.00"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := moments.Advance(ctx, model.Int64Val(5), 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Advance error = %v", err)
	}
	if _, err := moments.Result(ctx, builtin.NumericStddevSample, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Result error = %v", err)
	}
	_, err := moments.Result(t.Context(), builtin.NumericVariancePopulation, 1)
	requireSQLState(t, err, "54000")
	_, err = moments.Result(t.Context(), builtin.NumericStatistic(255), 1<<20)
	requireSQLState(t, err, "XX000")
	requireNumericMoment(t, &moments, builtin.NumericVarianceSample, "2.0000000000000000")
}

func TestNumericMomentsFinalizationEnforcesWorkLimit(t *testing.T) {
	var moments builtin.NumericMoments
	for range 10 {
		advanceNumericMoments(t, &moments, numericDatum("9e16382"))
	}
	_, err := moments.Result(t.Context(), builtin.NumericVariancePopulation, 1<<20)
	requireSQLState(t, err, "54000")
}
