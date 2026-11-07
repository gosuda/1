package window

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type span struct{ start, end int64 }

func shifted(index, offset, limit int64, following bool) int64 {
	if following {
		if offset >= limit-index {
			return limit
		}
		return index + offset
	}
	if offset > index {
		return -1
	}
	return index - offset
}

func (c *cursor) frameBound(ctx context.Context, kind ir.BoundKind, offset, group, peerStart, peerEnd int64, end bool) (int64, error) {
	n := int64(c.data.Rows())
	switch kind {
	case ir.UnboundedPreceding:
		return 0, nil
	case ir.UnboundedFollowing:
		return n, nil
	}
	if c.options.Frame.Mode == ir.RangeFrame {
		if kind == ir.OffsetPreceding || kind == ir.OffsetFollowing {
			if c.current[c.options.OrderBy[0].Column].IsNull {
				if end {
					return peerEnd, nil
				}
				return peerStart, nil
			}
			return c.rangeBound(ctx, kind, offset, end)
		}
		if end {
			return peerEnd, nil
		}
		return peerStart, nil
	}
	index, limit := c.position, n
	if c.options.Frame.Mode == ir.GroupsFrame {
		index, limit = group, int64(c.groups.spool.Rows())
	}
	if kind == ir.OffsetPreceding || kind == ir.OffsetFollowing {
		index = shifted(index, offset, limit, kind == ir.OffsetFollowing)
	}
	if c.options.Frame.Mode == ir.RowsFrame {
		if end {
			index++
		}
		return max(0, min(n, index)), nil
	}
	if index < 0 {
		return 0, nil
	}
	if index >= limit {
		return n, nil
	}
	start, finish, err := c.groups.read(ctx, index)
	if err != nil {
		return 0, err
	}
	if end {
		return int64(finish), nil
	}
	return int64(start), nil
}

func (c *cursor) frame(ctx context.Context, group, peerStart, peerEnd int64) ([3]span, int, error) {
	if c.frameReady {
		return c.frameSpans, c.frameCount, nil
	}
	spans, count, err := c.computeFrame(ctx, group, peerStart, peerEnd)
	if err == nil {
		c.frameSpans, c.frameCount, c.frameReady = spans, count, true
	}
	return spans, count, err
}

func (c *cursor) computeFrame(ctx context.Context, group, peerStart, peerEnd int64) ([3]span, int, error) {
	var spans [3]span
	f := c.options.Frame
	lo, err := c.frameBound(ctx, f.Start.Kind, c.startOffset, group, peerStart, peerEnd, false)
	if err != nil {
		return spans, 0, err
	}
	hi, err := c.frameBound(ctx, f.End.Kind, c.endOffset, group, peerStart, peerEnd, true)
	if err != nil {
		return spans, 0, err
	}
	count := 0
	add := func(start, end int64) {
		if start < end {
			spans[count] = span{start, end}
			count++
		}
	}
	if lo >= hi {
		return spans, 0, nil
	}
	switch f.Exclusion {
	case ir.ExcludeNone:
		add(lo, hi)
	case ir.ExcludeCurrent:
		add(lo, min(hi, c.position))
		add(max(lo, c.position+1), hi)
	case ir.ExcludeGroup, ir.ExcludeTies:
		add(lo, min(hi, peerStart))
		if f.Exclusion == ir.ExcludeTies && lo <= c.position && c.position < hi {
			add(c.position, c.position+1)
		}
		add(max(lo, peerEnd), hi)
	}
	return spans, count, nil
}
