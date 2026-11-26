package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func numericRangeQuery(t *testing.T, workBytes uint64) *runtime.Query {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	shared, err := runtime.NewBudget(int64(workBytes), 16)
	if err != nil {
		t.Fatal(err)
	}
	query, err := runtime.NewQuery(t.Context(), pool, shared, nil, runtime.QueryConfig{WorkBytes: int64(workBytes), SpillMode: runtime.SpillDisabled})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
	})
	return query
}

func numericRange(t *testing.T, offset string, allocator columnar.Allocator) *builtin.NumericRange {
	t.Helper()
	r, err := builtin.NewNumericRange(t.Context(), numericDatum(offset), allocator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func TestNumericRangeExactBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, offset, base  string
		subtract            bool
		below, equal, above string
	}{
		{"fractional addition", "0.2", "0.1", false, "0.299999999999999999999999999999", "0.3000", "0.300000000000000000000000000001"},
		{"fractional subtraction", "0.2", "0.1", true, "-0.100000000000000000000000000001", "-0.10", "-0.099999999999999999999999999999"},
		{"large integer fraction", "0.00000000000000000001", "9007199254740993", false, "9007199254740993", "9007199254740993.000000000000000000010", "9007199254740993.00000000000000000002"},
		{"large exponent", "1e1000", "2e1000", false, "2.999e1000", "30e999", "3.001e1000"},
		{"smallest stored scale", "1e-16383", "2e-16383", true, "0", "1e-16383", "2e-16383"},
		{"negative zero offset", "-0.000", "1.20", true, "1.1999", "1.200000", "1.2001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := numericRangeQuery(t, builtin.NumericRangeWorkBytes)
			r := numericRange(t, test.offset, q.Allocator(runtime.WorkMemory))
			if err := r.SetBase(t.Context(), numericDatum(test.base), test.subtract); err != nil {
				t.Fatal(err)
			}
			for i, probe := range []string{test.below, test.equal, test.above} {
				got, err := r.Compare(t.Context(), numericDatum(probe))
				if err != nil || got != i-1 {
					t.Errorf("probe %s: comparison = %d, %v; want %d", probe, got, err, i-1)
				}
			}
		})
	}
}

func TestNumericRangeSpecialValues(t *testing.T) {
	for _, test := range []struct {
		name, offset, base string
		subtract           bool
		want               [5]int
	}{
		{"finite bound", "0.5", "1", false, [5]int{-1, -1, 0, 1, 1}},
		{"NaN base", "1", "NaN", false, [5]int{-1, -1, -1, -1, 0}},
		{"NaN base infinite offset", "Infinity", "NaN", true, [5]int{-1, -1, -1, -1, 0}},
		{"positive infinite base", "1", "Infinity", true, [5]int{-1, -1, -1, 0, 1}},
		{"negative infinite base", "1", "-Infinity", false, [5]int{0, 1, 1, 1, 1}},
		{"infinite addition", "Infinity", "1", false, [5]int{-1, -1, -1, 0, 1}},
		{"infinite subtraction", "Infinity", "1", true, [5]int{0, 1, 1, 1, 1}},
		{"positive infinities addition", "Infinity", "Infinity", false, [5]int{-1, -1, -1, 0, 1}},
		{"negative infinities subtraction", "Infinity", "-Infinity", true, [5]int{0, 1, 1, 1, 1}},
		{"ambiguous positive subtraction", "Infinity", "Infinity", true, [5]int{0, 0, 0, 0, 1}},
		{"ambiguous negative addition", "Infinity", "-Infinity", false, [5]int{0, 0, 0, 0, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := numericRangeQuery(t, builtin.NumericRangeWorkBytes)
			r := numericRange(t, test.offset, q.Allocator(runtime.WorkMemory))
			if err := r.SetBase(t.Context(), numericDatum(test.base), test.subtract); err != nil {
				t.Fatal(err)
			}
			for i, probe := range []string{"-Infinity", "0", "1.5", "Infinity", "NaN"} {
				got, err := r.Compare(t.Context(), numericDatum(probe))
				if err != nil || got != test.want[i] {
					t.Errorf("probe %s: comparison = %d, %v; want %d", probe, got, err, test.want[i])
				}
			}
		})
	}
}

func TestNumericRangeRejectsOffsetsWithoutLeakingAdmission(t *testing.T) {
	q := numericRangeQuery(t, builtin.NumericRangeWorkBytes)
	for _, test := range []struct {
		name  string
		value model.Value
		state string
	}{
		{"negative", numericDatum("-0.01"), "22013"},
		{"negative infinity", numericDatum("-Infinity"), "22013"},
		{"NaN", numericDatum("NaN"), "22013"},
		{"malformed", numericDatum("1.2.3"), "22P02"},
		{"wrong type", model.StringVal("1"), "42804"},
		{"NULL", model.NullVal(model.TypeNumeric), "22004"},
		{"excessive scale", numericDatum("1e-16384"), "22003"},
		{"excessive expansion", numericDatum("1e100000"), "54000"},
		{"excessive input", numericDatum(strings.Repeat("1", 65537)), "54000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, err := builtin.NewNumericRange(t.Context(), test.value, q.Allocator(runtime.WorkMemory))
			requireSQLState(t, err, test.state)
			if r != nil {
				t.Fatal("failed constructor returned a comparator")
			}
			if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
				t.Fatalf("failed constructor retained work: %+v", usage)
			}
		})
	}
}

func TestNumericRangeAdmissionPrecedesParsing(t *testing.T) {
	q := numericRangeQuery(t, 1)
	r, err := builtin.NewNumericRange(t.Context(), numericDatum("not numeric"), q.Allocator(runtime.WorkMemory))
	if r != nil || !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("unadmitted malformed offset: comparator = %v, error = %v", r, err)
	}
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("failed admission retained work: %+v", usage)
	}
	_, err = builtin.NewNumericRange(t.Context(), numericDatum("1"), nil)
	requireSQLState(t, err, "XX000")
}

func TestNumericRangeFailedBaseInvalidatesBoundary(t *testing.T) {
	q := numericRangeQuery(t, builtin.NumericRangeWorkBytes)
	r := numericRange(t, "0.25", q.Allocator(runtime.WorkMemory))
	for _, test := range []struct {
		name  string
		value model.Value
		state string
	}{
		{"malformed", numericDatum("broken"), "22P02"},
		{"wrong type", model.Int64Val(1), "42804"},
		{"NULL", model.NullVal(model.TypeNumeric), "22004"},
		{"alignment work limit", numericDatum("1e32767"), "54000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := r.SetBase(t.Context(), numericDatum("1"), false); err != nil {
				t.Fatal(err)
			}
			requireSQLState(t, r.SetBase(t.Context(), test.value, false), test.state)
			_, err := r.Compare(t.Context(), numericDatum("1.25"))
			requireSQLState(t, err, "XX000")
			if err := r.SetBase(t.Context(), numericDatum("1"), true); err != nil {
				t.Fatal(err)
			}
			if got, err := r.Compare(t.Context(), numericDatum("0.75")); err != nil || got != 0 {
				t.Fatalf("recovered boundary: comparison = %d, %v; want 0", got, err)
			}
		})
	}
}

func TestNumericRangeProbeErrorsPreserveBoundary(t *testing.T) {
	q := numericRangeQuery(t, builtin.NumericRangeWorkBytes)
	r := numericRange(t, "0.25", q.Allocator(runtime.WorkMemory))
	if err := r.SetBase(t.Context(), numericDatum("1"), false); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		value model.Value
		state string
	}{
		{numericDatum("broken"), "22P02"},
		{model.Int64Val(1), "42804"},
		{model.NullVal(model.TypeNumeric), "22004"},
	} {
		_, err := r.Compare(t.Context(), test.value)
		requireSQLState(t, err, test.state)
	}
	if got, err := r.Compare(t.Context(), numericDatum("1.2500")); err != nil || got != 0 {
		t.Fatalf("boundary after failed probes: comparison = %d, %v; want 0", got, err)
	}
	if got, err := r.Compare(t.Context(), numericDatum("1e32767")); err != nil || got <= 0 {
		t.Fatalf("large comparison required alignment workspace: comparison=%d error=%v", got, err)
	}
}

type numericRangeCancelAllocator struct {
	columnar.Allocator
	cancel context.CancelFunc
}

func (a numericRangeCancelAllocator) Reserve(bytes uint64) (columnar.Allocation, error) {
	claim, err := a.Allocator.Reserve(bytes)
	a.cancel()
	return claim, err
}

func TestNumericRangeCancellationAfterAdmissionReleasesWork(t *testing.T) {
	q := numericRangeQuery(t, builtin.NumericRangeWorkBytes)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	allocator := numericRangeCancelAllocator{Allocator: q.Allocator(runtime.WorkMemory), cancel: cancel}
	r, err := builtin.NewNumericRange(ctx, numericDatum("1"), allocator)
	if r != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admitted constructor: comparator = %v, error = %v", r, err)
	}
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("canceled admitted constructor retained work: %+v", usage)
	}
}

func TestNumericRangeCancellation(t *testing.T) {
	q := numericRangeQuery(t, builtin.NumericRangeWorkBytes)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r, err := builtin.NewNumericRange(ctx, numericDatum("Infinity"), q.Allocator(runtime.WorkMemory)); r != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled constructor: comparator = %v, error = %v", r, err)
	}
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("canceled constructor retained work: %+v", usage)
	}
	r := numericRange(t, "Infinity", q.Allocator(runtime.WorkMemory))
	if err := r.SetBase(t.Context(), numericDatum("Infinity"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Compare(ctx, numericDatum("NaN")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled special probe: %v", err)
	}
	if err := r.SetBase(ctx, numericDatum("NaN"), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled base: %v", err)
	}
	_, err := r.Compare(t.Context(), numericDatum("0"))
	requireSQLState(t, err, "XX000")
	if err := r.SetBase(t.Context(), numericDatum("0"), true); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Compare(t.Context(), numericDatum("0")); err != nil || got != 1 {
		t.Fatalf("recovered canceled boundary: comparison = %d, %v; want 1", got, err)
	}
}

func TestNumericRangeCloseReleasesAdmission(t *testing.T) {
	q := numericRangeQuery(t, builtin.NumericRangeWorkBytes)
	r := numericRange(t, "0.25", q.Allocator(runtime.WorkMemory))
	_, err := r.Compare(t.Context(), numericDatum("0"))
	requireSQLState(t, err, "XX000")
	if err := r.SetBase(t.Context(), numericDatum("1"), true); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Compare(t.Context(), numericDatum("0.75")); err != nil || got != 0 {
		t.Fatalf("subtracted boundary: comparison = %d, %v; want 0", got, err)
	}
	if err := r.SetBase(t.Context(), numericDatum("1"), false); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Compare(t.Context(), numericDatum("1.25")); err != nil || got != 0 {
		t.Fatalf("reused offset: comparison = %d, %v; want 0", got, err)
	}
	other := numericRange(t, "1", q.Allocator(runtime.WorkMemory))
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("closed range retained work: %+v", usage)
	}
	_, err = r.Compare(t.Context(), numericDatum("1.25"))
	requireSQLState(t, err, "XX000")
	requireSQLState(t, r.SetBase(t.Context(), numericDatum("1"), false), "XX000")
	replacement := numericRange(t, "1", q.Allocator(runtime.WorkMemory))
	if err := replacement.SetBase(t.Context(), numericDatum("2"), false); err != nil {
		t.Fatal(err)
	}
	if got, err := replacement.Compare(t.Context(), numericDatum("3")); err != nil || got != 0 {
		t.Fatalf("replacement boundary: comparison = %d, %v; want 0", got, err)
	}
}
