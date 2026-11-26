package window

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (c *cursor) configureNumericRanges(ctx context.Context, available uint64) (uint64, error) {
	if !c.rangeNumeric {
		return 0, nil
	}
	var used uint64
	for i, bound := range [...]ir.FrameBound{c.options.Frame.Start, c.options.Frame.End} {
		if bound.Kind != ir.OffsetPreceding && bound.Kind != ir.OffsetFollowing {
			continue
		}
		if builtin.NumericRangeWorkBytes > available-used {
			return used, resource("window workspace cannot admit numeric RANGE", nil)
		}
		value := c.options.StartOffset
		if i == 1 {
			value = c.options.EndOffset
		}
		comparator, err := builtin.NewNumericRange(ctx, *value, c.options.Query.Allocator(runtime.WorkMemory))
		if err != nil {
			return used, err
		}
		c.numericRanges[i] = comparator
		used += builtin.NumericRangeWorkBytes
	}
	return used, nil
}
