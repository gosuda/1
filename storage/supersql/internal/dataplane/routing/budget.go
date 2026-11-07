package routing

import (
	"context"
	"fmt"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type budget struct {
	limits Limits
	bytes  uint64
	nodes  uint64
	steps  uint64
}

func newBudget(ctx context.Context, limits Limits) (*budget, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	emptyCounts := limits.MaxNodes == 0 || limits.MaxSpans == 0 || limits.MaxPartitions == 0 || limits.MaxMorsels == 0
	if emptyCounts || limits.MaxWork == 0 || limits.MaxBytes == 0 {
		return nil, ErrLimit
	}
	limits.MaxBytes = min(limits.MaxBytes, uint64(math.MaxInt))
	return &budget{limits: limits}, nil
}

func (b *budget) allocate(count, size uint64) error {
	if count > (b.limits.MaxBytes-b.bytes)/size {
		return ErrLimit
	}
	b.bytes += count * size
	return nil
}

func (b *budget) work(ctx context.Context, count uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if count > b.limits.MaxWork-b.steps {
		return ErrLimit
	}
	b.steps += count
	return nil
}

// Visit rejects cyclic or oversized value trees before the copier or encoder
// can allocate from them. All payload fields count, even on malformed NULLs.
func (b *budget) visit(ctx context.Context, value model.Value, depth int) error {
	if depth > 64 || b.nodes == b.limits.MaxNodes {
		return ErrLimit
	}
	b.nodes++
	if err := b.work(ctx, 1); err != nil {
		return err
	}
	for _, payload := range [...]struct{ count, size uint64 }{
		{uint64(len(value.StrVal)), 1},
		{uint64(len(value.BytesVal)), 1},
		{uint64(len(value.VectorVal)), 4},
		{uint64(len(value.Dimensions)), uint64(unsafe.Sizeof(model.ArrayDimension{}))},
		{uint64(len(value.ArrayVal)), uint64(unsafe.Sizeof(model.Value{}))},
	} {
		if payload.count > (b.limits.MaxWork-b.steps)/payload.size {
			return ErrLimit
		}
		if err := b.work(ctx, payload.count*payload.size); err != nil {
			return err
		}
	}
	if uint64(len(value.ArrayVal)) > b.limits.MaxNodes-b.nodes {
		return ErrLimit
	}
	for _, child := range value.ArrayVal {
		if err := b.visit(ctx, child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (b *budget) visitLayout(ctx context.Context, layout model.TypeLayout, depth int) error {
	if depth > 64 || b.nodes == b.limits.MaxNodes {
		return ErrLimit
	}
	b.nodes++
	if err := b.work(ctx, 1); err != nil {
		return err
	}
	for _, child := range layout.Children {
		if err := b.visitLayout(ctx, child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (b *budget) encode(ctx context.Context, encoder storage.KeyEncoder, layout []storage.KeyColumn, values []model.Value) ([]byte, error) {
	n := uint64(len(layout))
	// The SQL encoder validates layout identities, including duplicate columns.
	if n != 0 && n > (b.limits.MaxWork-b.steps)/n {
		return nil, ErrLimit
	}
	if err := b.work(ctx, n*n); err != nil {
		return nil, err
	}
	for _, column := range layout {
		if err := b.visitLayout(ctx, model.TypeLayout{Type: column.Type, Children: column.Children}, 0); err != nil {
			return nil, err
		}
	}
	prefix, err := encoder.EncodePrefix(ctx, layout, values, b.limits.MaxBytes-b.bytes)
	if err != nil {
		return nil, fmt.Errorf("routing key comparison: %w", err)
	}
	if err := b.allocate(uint64(cap(prefix)), 1); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return prefix, nil
}

func (b *budget) copySpan(ctx context.Context, span storage.KeySpan) (storage.KeySpan, error) {
	lower, err := b.copyBound(ctx, span.Lower)
	if err != nil {
		return storage.KeySpan{}, err
	}
	upper, err := b.copyBound(ctx, span.Upper)
	if err != nil {
		return storage.KeySpan{}, err
	}
	return storage.KeySpan{Lower: lower, Upper: upper}, nil
}

func (b *budget) copyBound(ctx context.Context, bound storage.KeyBound) (storage.KeyBound, error) {
	if uint64(len(bound.Values)) > b.limits.MaxNodes-b.nodes {
		return storage.KeyBound{}, ErrLimit
	}
	for _, value := range bound.Values {
		if err := b.visit(ctx, value, 0); err != nil {
			return storage.KeyBound{}, err
		}
	}
	if err := b.allocate(uint64(len(bound.Values)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return storage.KeyBound{}, err
	}
	if len(bound.Values) == 0 {
		return storage.KeyBound{Unbounded: bound.Unbounded, Inclusive: bound.Inclusive}, nil
	}
	owned := make([]model.Value, len(bound.Values))
	for i, value := range bound.Values {
		copied, size, err := model.CopyValue(ctx, value, b.limits.MaxBytes-b.bytes)
		if err != nil {
			return storage.KeyBound{}, err
		}
		b.bytes += size
		owned[i] = copied
	}
	return storage.KeyBound{Values: owned, Inclusive: bound.Inclusive, Unbounded: bound.Unbounded}, nil
}
