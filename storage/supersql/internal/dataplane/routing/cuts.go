package routing

import (
	"bytes"
	"cmp"
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Prefix cuts bracket all complete tuples extending a leading tuple. In
// particular, after(tenant) sorts after before(tenant, id), in either direction.
type cut struct {
	bound    storage.KeyBound
	prefix   []byte
	infinity int8
	after    bool
}

type interval struct{ lower, upper cut }

func (b *budget) compare(ctx context.Context, a, c cut) (int, error) {
	if err := b.work(ctx, 1); err != nil {
		return 0, err
	}
	length := uint64(min(len(a.prefix), len(c.prefix)))
	if length > (b.limits.MaxWork-b.steps)/4 {
		return 0, ErrLimit
	}
	if err := b.work(ctx, length*4); err != nil {
		return 0, err
	}
	if a.infinity != 0 || c.infinity != 0 {
		return cmp.Compare(a.infinity, c.infinity), nil
	}
	if bytes.Equal(a.prefix, c.prefix) {
		if a.after == c.after {
			return 0, nil
		}
		if a.after {
			return 1, nil
		}
		return -1, nil
	}
	if bytes.HasPrefix(c.prefix, a.prefix) {
		if a.after {
			return 1, nil
		}
		return -1, nil
	}
	if bytes.HasPrefix(a.prefix, c.prefix) {
		if c.after {
			return -1, nil
		}
		return 1, nil
	}
	return bytes.Compare(a.prefix, c.prefix), nil
}

func compileSpan(ctx context.Context, span storage.KeySpan, layout []storage.KeyColumn, encoder storage.KeyEncoder, budget *budget) (interval, error) {
	lower, err := compileCut(ctx, span.Lower, true, layout, encoder, budget)
	if err != nil {
		return interval{}, err
	}
	upper, err := compileCut(ctx, span.Upper, false, layout, encoder, budget)
	if err != nil {
		return interval{}, err
	}
	order, err := budget.compare(ctx, lower, upper)
	if err != nil {
		return interval{}, err
	}
	if order >= 0 {
		return interval{}, storage.ErrRangeInvalid
	}
	return interval{lower: lower, upper: upper}, nil
}

func compileCut(ctx context.Context, bound storage.KeyBound, lower bool, layout []storage.KeyColumn, encoder storage.KeyEncoder, budget *budget) (cut, error) {
	if err := budget.work(ctx, 1); err != nil {
		return cut{}, err
	}
	if bound.Unbounded {
		if len(bound.Values) != 0 || bound.Inclusive {
			return cut{}, storage.ErrRangeInvalid
		}
		infinity := int8(1)
		if lower {
			infinity = -1
		}
		return cut{bound: bound, infinity: infinity}, nil
	}
	if len(bound.Values) == 0 || len(bound.Values) > len(layout) {
		return cut{}, storage.ErrRangeInvalid
	}
	for i, value := range bound.Values {
		if value.Type != layout[i].Type.Storage {
			return cut{}, storage.ErrRangeInvalid
		}
		if err := budget.visit(ctx, value, 0); err != nil {
			return cut{}, err
		}
	}
	prefix, err := budget.encode(ctx, encoder, layout, bound.Values)
	if err != nil {
		return cut{}, err
	}
	if len(prefix) == 0 {
		return cut{}, storage.ErrRangeInvalid
	}
	after := bound.Inclusive
	if lower {
		after = !after
	}
	return cut{bound: bound, prefix: prefix, after: after}, nil
}

func normalize(ctx context.Context, spans []interval, budget *budget) ([]interval, error) {
	if len(spans) < 2 {
		return spans, nil
	}
	if err := budget.allocate(uint64(len(spans)), uint64(unsafe.Sizeof(interval{}))); err != nil {
		return nil, err
	}
	scratch := make([]interval, len(spans))
	// Error-aware mergesort never turns cancellation into an inconsistent sort
	// comparator, and has bounded O(n log n) comparisons on adversarial input.
	for width := 1; width < len(spans); {
		for start := 0; start < len(spans); {
			middle := start + min(width, len(spans)-start)
			end := middle + min(width, len(spans)-middle)
			if err := mergeRun(ctx, spans, scratch, start, middle, end, budget); err != nil {
				return nil, err
			}
			start = end
		}
		spans, scratch = scratch, spans
		if width >= len(spans)-width {
			break
		}
		width *= 2
	}
	count := 1
	for _, span := range spans[1:] {
		previous := &spans[count-1]
		order, err := budget.compare(ctx, previous.upper, span.lower)
		if err != nil {
			return nil, err
		}
		if order < 0 {
			spans[count] = span
			count++
			continue
		}
		order, err = budget.compare(ctx, previous.upper, span.upper)
		if err != nil {
			return nil, err
		}
		if order < 0 {
			previous.upper = span.upper
		}
	}
	return spans[:count], nil
}

func mergeRun(ctx context.Context, spans, scratch []interval, start, middle, end int, budget *budget) error {
	left, right := start, middle
	for out := start; out < end; out++ {
		order := -1
		if left < middle && right < end {
			var err error
			order, err = budget.compare(ctx, spans[left].lower, spans[right].lower)
			if err != nil {
				return err
			}
		} else if err := budget.work(ctx, 1); err != nil {
			return err
		}
		if left < middle && (right == end || order <= 0) {
			scratch[out] = spans[left]
			left++
		} else {
			scratch[out] = spans[right]
			right++
		}
	}
	return nil
}
