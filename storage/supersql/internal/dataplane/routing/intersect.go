package routing

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Intersect returns the key-ordered, disjoint intersection of two span unions.
// Empty input means no coverage, but both inputs and the full layout are still
// validated. Results own every bound independently; any error returns no output.
// Limits apply to the whole operation; MaxSpans bounds each input and the result.
// MaxPartitions and MaxMorsels must be positive but otherwise do not apply.
func Intersect(ctx context.Context, left, right []storage.KeySpan, layout []storage.KeyColumn, encoder storage.KeyEncoder, limits Limits) ([]storage.KeySpan, error) {
	budget, err := newBudget(ctx, limits)
	if err != nil {
		return nil, err
	}
	if encoder == nil || len(layout) == 0 {
		return nil, ErrInvalidSpec
	}
	if uint64(len(left)) > limits.MaxSpans || uint64(len(right)) > limits.MaxSpans || uint64(len(layout)) > limits.MaxNodes {
		return nil, ErrLimit
	}
	if err := budget.work(ctx, uint64(len(layout))); err != nil {
		return nil, err
	}
	for _, key := range layout {
		if key.ColumnID == 0 || key.Type.ID == 0 || !key.Type.Storage.Valid() {
			return nil, storage.ErrInvalidLayout
		}
	}
	if _, err := budget.encode(ctx, encoder, layout, nil); err != nil {
		return nil, err
	}
	leftIntervals, err := compileIntersectionInput(ctx, left, layout, encoder, budget)
	if err != nil {
		return nil, err
	}
	rightIntervals, err := compileIntersectionInput(ctx, right, layout, encoder, budget)
	if err != nil {
		return nil, err
	}
	var count uint64
	if err := intersectIntervals(ctx, leftIntervals, rightIntervals, budget, func(_ interval) error {
		if count == limits.MaxSpans {
			return ErrLimit
		}
		count++
		return nil
	}); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ctx.Err()
	}
	if err := budget.allocate(count, uint64(unsafe.Sizeof(storage.KeySpan{}))); err != nil {
		return nil, err
	}
	result := make([]storage.KeySpan, int(count))
	index := 0
	if err := intersectIntervals(ctx, leftIntervals, rightIntervals, budget, func(span interval) error {
		owned, err := budget.copySpan(ctx, storage.KeySpan{Lower: span.lower.bound, Upper: span.upper.bound})
		if err != nil {
			return err
		}
		result[index] = owned
		index++
		return nil
	}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func compileIntersectionInput(ctx context.Context, spans []storage.KeySpan, layout []storage.KeyColumn, encoder storage.KeyEncoder, budget *budget) ([]interval, error) {
	if err := budget.allocate(uint64(len(spans)), uint64(unsafe.Sizeof(interval{}))); err != nil {
		return nil, err
	}
	intervals := make([]interval, len(spans))
	for i, span := range spans {
		compiled, err := compileSpan(ctx, span, layout, encoder, budget)
		if err != nil {
			return nil, err
		}
		intervals[i] = compiled
	}
	return normalize(ctx, intervals, budget)
}

func intersectIntervals(ctx context.Context, left, right []interval, budget *budget, emit func(interval) error) error {
	for l, r := 0, 0; l < len(left) && r < len(right); {
		lower, upper := left[l].lower, left[l].upper
		order, err := budget.compare(ctx, lower, right[r].lower)
		if err != nil {
			return err
		}
		if order < 0 {
			lower = right[r].lower
		}
		endOrder, err := budget.compare(ctx, upper, right[r].upper)
		if err != nil {
			return err
		}
		if endOrder > 0 {
			upper = right[r].upper
		}
		order, err = budget.compare(ctx, lower, upper)
		if err != nil {
			return err
		}
		if order < 0 {
			if err := emit(interval{lower: lower, upper: upper}); err != nil {
				return err
			}
		}
		if endOrder <= 0 {
			l++
		}
		if endOrder >= 0 {
			r++
		}
	}
	return nil
}
