package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestNumericSeriesExactScaleAndBounds(t *testing.T) {
	for _, test := range []struct {
		name, start, stop, step string
		want                    []string
	}{
		{"display scale grows", "1.0", "1.04", "0.020", []string{"1.0", "1.020", "1.040"}},
		{"descending through zero", "0.10", "-0.2", "-0.1", []string{"0.10", "0.00", "-0.10", "-0.20"}},
		{"fraction beyond float precision", "9007199254740993", "9007199254740993.00000000000000000002", "0.00000000000000000001", []string{"9007199254740993", "9007199254740993.00000000000000000001", "9007199254740993.00000000000000000002"}},
		{"fractional stop is not rounded", "0.0", "0.299999999999999999999999", "0.1", []string{"0.0", "0.1", "0.2"}},
		{"opposite direction", "1", "0", "0.1", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := numericRangeQuery(t, 64<<10)
			series, err := builtin.NewNumericSeries(t.Context(), test.start, test.stop, test.step, builtin.NumericSeriesOptions{MemoryBytes: 64 << 10, ValueBytes: 1024, Allocator: query.Allocator(runtime.WorkMemory)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := series.Close(); err != nil {
					t.Error(err)
				}
			})
			var got []string
			for {
				value, ok, err := series.Peek(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					break
				}
				if len(got) >= len(test.want) {
					t.Fatalf("unexpected additional value: %s", value.StrVal)
				}
				got = append(got, strings.Clone(value.StrVal))
				value = model.Value{}
				if err := series.Advance(); err != nil {
					t.Fatal(err)
				}
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("series = %v, want %v", got, test.want)
			}
			if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
				t.Fatalf("exhaustion retained work: %+v", usage)
			}
		})
	}
}

func TestNumericSeriesRejectsNonfiniteBoundsAndSteps(t *testing.T) {
	query := numericRangeQuery(t, 64<<10)
	for _, test := range []struct {
		start, stop, step string
	}{
		{"NaN", "1", "1"},
		{"-Infinity", "1", "1"},
		{"0", "NaN", "1"},
		{"0", "Infinity", "1"},
		{"0", "1", "NaN"},
		{"0", "1", "-Infinity"},
		{"1", "0", "-0.000"},
	} {
		t.Run(test.start+"/"+test.stop+"/"+test.step, func(t *testing.T) {
			series, err := builtin.NewNumericSeries(t.Context(), test.start, test.stop, test.step, builtin.NumericSeriesOptions{MemoryBytes: 64 << 10, ValueBytes: 1024, Allocator: query.Allocator(runtime.WorkMemory)})
			requireSQLState(t, err, "22023")
			if series != nil {
				t.Fatal("failed constructor returned state")
			}
			if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
				t.Fatalf("failed constructor retained work: %+v", usage)
			}
		})
	}
}

func TestNumericSeriesPeekBackpressureAndBoundedLifetime(t *testing.T) {
	query := numericRangeQuery(t, 4096)
	series, err := builtin.NewNumericSeries(t.Context(), "0.000", "99.999", "0.001", builtin.NumericSeriesOptions{MemoryBytes: 4096, ValueBytes: 6, Allocator: query.Allocator(runtime.WorkMemory)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := series.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := series.Advance(); err == nil {
		t.Fatal("Advance accepted an unprepared row")
	}
	for i := range 2000 {
		value, ok, err := series.Peek(t.Context())
		if err != nil || !ok {
			t.Fatalf("row %d: %v, %v", i, ok, err)
		}
		usage := query.Usage().Work
		repeated, ok, err := series.Peek(t.Context())
		if err != nil || !ok || repeated.StrVal != value.StrVal {
			t.Fatalf("repeated Peek changed row: %v, %v, %v", repeated, ok, err)
		}
		if after := query.Usage().Work; after.Live != usage.Live || after.Reservations != usage.Reservations {
			t.Fatalf("repeated Peek changed live admission: before %+v, after %+v", usage, after)
		}
		value, repeated = model.Value{}, model.Value{}
		if err := series.Advance(); err != nil {
			t.Fatal(err)
		}
		if err := series.Advance(); err == nil {
			t.Fatal("Advance consumed the same row twice")
		}
	}
	value, ok, err := series.Peek(t.Context())
	if err != nil || !ok || value.StrVal != "2.000" {
		t.Fatalf("2000 advances = %v, %v, %v", value, ok, err)
	}
	value = model.Value{}
	if err := series.Close(); err != nil {
		t.Fatal(err)
	}
	if err := series.Close(); err != nil {
		t.Fatal(err)
	}
	if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("early Close retained work: %+v", usage)
	}
}

func TestNumericSeriesCancellationReleasesPendingValue(t *testing.T) {
	query := numericRangeQuery(t, 4096)
	options := builtin.NumericSeriesOptions{MemoryBytes: 4096, ValueBytes: 10, Allocator: query.Allocator(runtime.WorkMemory)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if series, err := builtin.NewNumericSeries(ctx, "0", "1", "1", options); !errors.Is(err, context.Canceled) || series != nil {
		t.Fatalf("canceled constructor = %v, %v", series, err)
	}
	series, err := builtin.NewNumericSeries(t.Context(), "0", "1", "1", options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := series.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, ok, err := series.Peek(t.Context()); err != nil || !ok {
		t.Fatalf("initial Peek = %v, %v", ok, err)
	}
	if _, ok, err := series.Peek(ctx); !errors.Is(err, context.Canceled) || ok {
		t.Fatalf("canceled Peek = %v, %v", ok, err)
	}
	if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("cancellation retained work: %+v", usage)
	}
}

func TestNumericSeriesQuotaFailuresReleaseAdmission(t *testing.T) {
	for _, test := range []struct {
		name                  string
		memory, value, shared uint64
		start, stop, step     string
		allocatorFailure      bool
	}{
		{"state header", 1, 10, 4096, "0", "1", "1", false},
		{"retained coefficients", 600, 10, 4096, strings.Repeat("9", 1000), "0", "1", false},
		{"payload", 4096, 2, 4096, "1.00", "2", "1", false},
		{"pending output", 4096, 3000, 8192, "0." + strings.Repeat("0", 1999) + "1", "1", "1", false},
		{"allocator admission", 64 << 10, 4096, 1024, strings.Repeat("9", 3000), "0", "1", true},
		{"last increment", 1 << 20, 64 << 10, 1 << 20, strings.Repeat("9", 32768), strings.Repeat("9", 32768), "1", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := numericRangeQuery(t, test.shared)
			series, err := builtin.NewNumericSeries(t.Context(), test.start, test.stop, test.step, builtin.NumericSeriesOptions{MemoryBytes: test.memory, ValueBytes: test.value, Allocator: query.Allocator(runtime.WorkMemory)})
			if err == nil {
				var ok bool
				_, ok, err = series.Peek(t.Context())
				if ok {
					t.Fatal("quota failure exposed a value")
				}
			}
			if test.allocatorFailure {
				if !errors.Is(err, runtime.ErrBudget) {
					t.Fatalf("allocator failure = %v", err)
				}
			} else {
				requireSQLState(t, err, "54000")
			}
			if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
				t.Fatalf("quota failure retained work: %+v", usage)
			}
		})
	}
}
