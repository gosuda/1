package runtime

import (
	"context"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
)

// Allocator borrows the query. Its allocations must be released before Query.Close.
func (q *Query) Allocator(kind MemoryKind) columnar.Allocator {
	return queryAllocator{query: q, kind: kind}
}

type queryAllocator struct {
	query *Query
	kind  MemoryKind
}

func (a queryAllocator) Reserve(bytes uint64) (columnar.Allocation, error) {
	if bytes > math.MaxInt64 {
		return nil, resourceError("53200", "reserve query bytes", ErrBudget)
	}
	q := a.query
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.Context().Err() != nil {
		return nil, errors.Join(ErrClosed, context.Cause(q.Context()))
	}
	budget, err := q.memory(a.kind)
	if err != nil {
		return nil, err
	}
	claim, err := budget.reserve(int64(bytes))
	if err != nil {
		return nil, err
	}
	return columnarAllocation{claim}, nil
}

type columnarAllocation struct{ claim *Reservation }

func (a columnarAllocation) Resize(bytes uint64) error {
	if bytes > math.MaxInt64 {
		return resourceError("53200", "resize query bytes", ErrBudget)
	}
	return a.claim.Resize(int64(bytes))
}

func (a columnarAllocation) GrowUpTo(maximum uint64) (uint64, error) {
	growth, err := a.claim.GrowUpTo(int64(min(maximum, math.MaxInt64)))
	return uint64(growth), err
}

func (a columnarAllocation) Release() { a.claim.Release() }
