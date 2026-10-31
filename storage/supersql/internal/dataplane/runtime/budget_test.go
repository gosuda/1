package runtime

import (
	"errors"
	"math"
	"sync"
	"testing"
)

func TestBudgetRejectsOverflowWithoutChangingLiveBytes(t *testing.T) {
	budget, err := NewBudget(math.MaxInt64, 2)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := budget.TryReserve(math.MaxInt64 - 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := budget.TryReserve(2); !errors.Is(err, ErrBudget) {
		t.Fatalf("overflow admission = %v", err)
	}
	if err := claim.Resize(math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := budget.TryReserve(1); !errors.Is(err, ErrBudget) {
		t.Fatalf("full admission = %v", err)
	}
	var closeCalls sync.WaitGroup
	for range 8 {
		closeCalls.Go(claim.Release)
	}
	closeCalls.Wait()
	if got := budget.Usage(); got.Live != 0 || got.Reservations != 0 || got.Peak != math.MaxInt64 {
		t.Fatalf("usage after duplicate releases = %+v", got)
	}
	if err := claim.Resize(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("resize released claim = %v", err)
	}
}

func TestQueryWorkAndOutputShareDatabaseCap(t *testing.T) {
	pool, err := NewPool(PoolConfig{Parallelism: 1, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	shared, err := NewBudget(10, 4)
	if err != nil {
		t.Fatal(err)
	}
	config := QueryConfig{WorkBytes: 8, OutputBytes: 8, SpillMode: SpillDisabled}
	first, err := NewQuery(t.Context(), pool, shared, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := NewQuery(t.Context(), pool, shared, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	work, err := first.TryReserve(WorkMemory, 6)
	if err != nil {
		t.Fatal(err)
	}
	output, err := second.NewBuffer(OutputMemory, 4)
	if err != nil {
		t.Fatal(err)
	}
	copy(output.Bytes(), "rows")
	if _, err := second.TryReserve(WorkMemory, 1); !errors.Is(err, ErrBudget) {
		t.Fatalf("cross-query excess = %v", err)
	}
	if err := work.Resize(9); !errors.Is(err, ErrBudget) {
		t.Fatalf("query work excess = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if got := shared.Usage(); got.Live != 4 || got.Peak != 10 {
		t.Fatalf("shared usage = %+v", got)
	}
	if string(output.Bytes()) != "rows" {
		t.Fatal("closing other query released output")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if output.Bytes() != nil || shared.Usage().Live != 0 {
		t.Fatal("query close retained output allocation/claim")
	}
}

func TestReservationCountBoundsTinyClaims(t *testing.T) {
	budget, err := NewBudget(100, 1)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := budget.TryReserve(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim.Resize(0); err != nil {
		t.Fatal(err)
	}
	if _, err := budget.TryReserve(1); !errors.Is(err, ErrBudget) {
		t.Fatalf("zero-byte live claim did not consume slot: %v", err)
	}
	claim.Release()
	replacement, err := budget.TryReserve(100)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Release()
}

func TestInvalidResourceLimitsRejected(t *testing.T) {
	for _, bytes := range []int64{-1, 0} {
		if _, err := NewBudget(bytes, 1); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("bytes %d: %v", bytes, err)
		}
	}
	if _, err := NewBudget(1, 0); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	for _, config := range []PoolConfig{{Parallelism: -1}, {QueueCapacity: -1}, {MaxGroups: -1}, {Parallelism: math.MaxInt}} {
		if _, err := NewPool(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %+v: %v", config, err)
		}
	}
}
