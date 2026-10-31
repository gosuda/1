package model

import (
	"context"
	"unsafe"
)

// ValueBytes measures the payload CopyValue would own, excluding the root Value
// header and spare slice capacity. It allocates no copies and shares CopyValue's
// byte and 64-level nesting limits.
func ValueBytes(ctx context.Context, value Value, maxBytes uint64) (uint64, error) {
	return valuePayloadBytes(ctx, value, maxBytes, 0)
}

func valuePayloadBytes(ctx context.Context, value Value, limit uint64, depth int) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if depth > 64 {
		return 0, ErrValueDepth
	}
	used := uint64(0)
	for _, allocation := range [...]struct{ count, width uint64 }{
		{uint64(len(value.StrVal)), 1}, {uint64(len(value.BytesVal)), 1},
		{uint64(len(value.VectorVal)), 4},
		{uint64(len(value.Dimensions)), uint64(unsafe.Sizeof(ArrayDimension{}))},
		{uint64(len(value.ArrayVal)), uint64(unsafe.Sizeof(Value{}))},
	} {
		if allocation.count > (limit-used)/allocation.width {
			return 0, ErrValueBudget
		}
		used += allocation.count * allocation.width
	}
	for _, child := range value.ArrayVal {
		bytes, err := valuePayloadBytes(ctx, child, limit-used, depth+1)
		if err != nil {
			return 0, err
		}
		used += bytes
	}
	return used, nil
}
