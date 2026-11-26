package decimalint_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/decimalint"
)

func TestDecimalCodecExactSignedAndBaseBoundaries(t *testing.T) {
	workspace, err := decimalint.NewWorkspace(t.Context(), nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	for _, tc := range []struct {
		input string
		base  int
		want  string
	}{
		{"-000000", 10, "0"}, {"9999999991000000000", 10, "9999999991000000000"}, {"-9223372036854775808", 10, "-9223372036854775808"},
		{"ffffffffffffffff", 16, "18446744073709551615"}, {"10000000000000000000000000000000000000000000000000000000000000000", 2, "18446744073709551616"}, {"777777777777777777777", 8, "9223372036854775807"},
	} {
		value, err := workspace.Parse(t.Context(), tc.input, tc.base)
		if err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 0, len(tc.want))
		result, err := value.AppendDecimal(t.Context(), buffer)
		if err != nil || string(result) != tc.want {
			t.Fatalf("base %d %q = %q, %v; want %q", tc.base, tc.input, result, err, tc.want)
		}
		if value.Digits() != len(strings.TrimPrefix(tc.want, "-")) {
			t.Fatalf("digits for %q = %d", tc.want, value.Digits())
		}
		if err := value.Close(); err != nil {
			t.Fatal(err)
		}
	}
	min, err := workspace.FromInt64(t.Context(), math.MinInt64)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := min.Int64(); !ok || value != math.MinInt64 {
		t.Fatalf("min int64 = %d %v", value, ok)
	}
	if _, ok := min.Uint64(); ok {
		t.Fatal("negative integer converted to uint64")
	}
}

func TestDecimalAppendDoesNotGrowOrAlterCallerPrefix(t *testing.T) {
	workspace, err := decimalint.NewWorkspace(t.Context(), nil, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	value, err := workspace.Parse(t.Context(), "12345678901234567890", 10)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 3, 8)
	copy(buffer, "pre")
	result, err := value.AppendDecimal(t.Context(), buffer)
	if !errors.Is(err, decimalint.ErrLimit) || string(result) != "pre" || string(buffer) != "pre" {
		t.Fatalf("short output changed prefix: %q, %v", result, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := value.AppendDecimal(ctx, make([]byte, 0, 32)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled append = %v", err)
	}
}

func TestDecimalWorkspaceAdmitsBeforeAllocationAndReleasesAllValues(t *testing.T) {
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	}()
	budget, err := runtime.NewBudget(4096, 8)
	if err != nil {
		t.Fatal(err)
	}
	query, err := runtime.NewQuery(t.Context(), pool, budget, nil, runtime.QueryConfig{WorkBytes: 4096, OutputBytes: 4096, SpillMode: runtime.SpillDisabled})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
	}()
	workspace, err := decimalint.NewWorkspace(t.Context(), query.Allocator(runtime.WorkMemory), 4096)
	if err != nil {
		t.Fatal(err)
	}
	first, err := workspace.Parse(t.Context(), strings.Repeat("9", 3000), 10)
	if err != nil {
		t.Fatal(err)
	}
	used := workspace.UsedBytes()
	_, err = workspace.Parse(t.Context(), strings.Repeat("8", 9000), 10)
	if err == nil || workspace.UsedBytes() != used {
		t.Fatalf("failed parse retained allocation: bytes=%d before=%d err=%v", workspace.UsedBytes(), used, err)
	}
	second, err := workspace.Clone(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if second.Digits() != 3000 {
		t.Fatal("closing source invalidated clone")
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("workspace retained shared claims: %+v", usage)
	}
	if _, err := workspace.FromInt64(t.Context(), 1); !errors.Is(err, decimalint.ErrClosed) {
		t.Fatalf("closed workspace accepted value: %v", err)
	}
}
