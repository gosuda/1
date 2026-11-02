package model

import (
	"context"
	"errors"
	"strings"
	"unsafe"
)

var (
	ErrValueBudget = errors.New("owned value exceeds byte budget")
	ErrValueDepth  = errors.New("owned value exceeds nesting limit")
)

// CopyValue owns every visible payload, excluding spare slice capacity. The
// returned byte count excludes the Value header supplied by the caller. Failed
// copies return no value; cancellation and the 64-level nesting bound also apply
// to malformed trees with cycles.
func CopyValue(ctx context.Context, value Value, maxBytes uint64) (Value, uint64, error) {
	copier := valueCopier{limit: maxBytes}
	owned, err := copier.copy(ctx, value, 0)
	if err != nil {
		return Value{}, 0, err
	}
	return owned, copier.used, nil
}

type valueCopier struct{ used, limit uint64 }

func (copier *valueCopier) charge(count, size uint64) error {
	if count > (copier.limit-copier.used)/size {
		return ErrValueBudget
	}
	copier.used += count * size
	return nil
}

func (copier *valueCopier) copy(ctx context.Context, value Value, depth int) (Value, error) {
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	if depth > 64 {
		return Value{}, ErrValueDepth
	}
	for _, allocation := range [...]struct{ count, size uint64 }{
		{uint64(len(value.StrVal)), 1},
		{uint64(len(value.BytesVal)), 1},
		{uint64(len(value.VectorVal)), 4},
		{uint64(len(value.Dimensions)), uint64(unsafe.Sizeof(ArrayDimension{}))},
		{uint64(len(value.ArrayVal)), uint64(unsafe.Sizeof(Value{}))},
	} {
		if err := copier.charge(allocation.count, allocation.size); err != nil {
			return Value{}, err
		}
	}
	owned := value
	owned.StrVal = strings.Clone(value.StrVal)
	if value.BytesVal != nil {
		owned.BytesVal = make([]byte, len(value.BytesVal))
		copy(owned.BytesVal, value.BytesVal)
	}
	if value.VectorVal != nil {
		owned.VectorVal = make([]float32, len(value.VectorVal))
		copy(owned.VectorVal, value.VectorVal)
	}
	if value.Dimensions != nil {
		owned.Dimensions = make([]ArrayDimension, len(value.Dimensions))
		copy(owned.Dimensions, value.Dimensions)
	}
	if value.ArrayVal != nil {
		owned.ArrayVal = make([]Value, len(value.ArrayVal))
		for i, child := range value.ArrayVal {
			copied, err := copier.copy(ctx, child, depth+1)
			if err != nil {
				return Value{}, err
			}
			owned.ArrayVal[i] = copied
		}
	}
	return owned, nil
}
