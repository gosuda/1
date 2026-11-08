package builtin

import "context"

func walkMultirangeUnion(ctx context.Context, ops *rangeTypeOps, a, b multirangeSequence, budget CallBudget, emit func(rangeDatum) error) error {
	x, xm, err := a.next(ctx)
	if err != nil {
		return err
	}
	y, ym, err := b.next(ctx)
	if err != nil {
		return err
	}
	pending := rangeDatum{empty: true}
	for xm || ym {
		left := !ym
		if xm && ym {
			order, err := ops.compareRanges(ctx, x, y, budget)
			if err != nil {
				return err
			}
			left = order <= 0
		}
		var value rangeDatum
		if left {
			value = x
			x, xm, err = a.next(ctx)
		} else {
			value = y
			y, ym, err = b.next(ctx)
		}
		if err != nil {
			return err
		}
		if pending.empty {
			pending = value
			continue
		}
		join, err := ops.overlaps(ctx, pending, value, budget)
		if err != nil {
			return err
		}
		if !join {
			join, err = ops.adjacent(ctx, pending, value, budget)
		}
		if err != nil {
			return err
		}
		if join {
			pending, err = ops.union(ctx, pending, value, false, budget)
			if err != nil {
				return err
			}
		} else {
			if err := emit(pending); err != nil {
				return err
			}
			pending = value
		}
	}
	if !pending.empty {
		return emit(pending)
	}
	return ctx.Err()
}

func emitRangePieces(pieces []rangeDatum, emit func(rangeDatum) error) error {
	for _, piece := range pieces {
		if err := emit(piece); err != nil {
			return err
		}
	}
	return nil
}

func walkMultirangeDifference(ctx context.Context, ops *rangeTypeOps, a, b multirangeSequence, budget CallBudget, emit func(rangeDatum) error) error {
	x, xm, err := a.next(ctx)
	if err != nil {
		return err
	}
	y, ym, err := b.next(ctx)
	if err != nil {
		return err
	}
	for xm {
		if !ym {
			if err := emit(x); err != nil {
				return err
			}
			x, xm, err = a.next(ctx)
			if err != nil {
				return err
			}
			continue
		}
		order, err := ops.compareBounds(ctx, y.upper, x.lower, budget)
		if err != nil {
			return err
		}
		if order < 0 {
			y, ym, err = b.next(ctx)
			if err != nil {
				return err
			}
			continue
		}
		order, err = ops.compareBounds(ctx, x.upper, y.lower, budget)
		if err != nil {
			return err
		}
		if order < 0 {
			if err := emit(x); err != nil {
				return err
			}
			x, xm, err = a.next(ctx)
			if err != nil {
				return err
			}
			continue
		}
		pieces, count, err := ops.difference(ctx, x, y, budget)
		if err != nil {
			return err
		}
		endOrder, err := ops.compareBounds(ctx, x.upper, y.upper, budget)
		if err != nil {
			return err
		}
		if endOrder <= 0 {
			if err := emitRangePieces(pieces[:count], emit); err != nil {
				return err
			}
			x, xm, err = a.next(ctx)
		} else {
			if count == 0 {
				return invalidRangeValue()
			}
			if err := emitRangePieces(pieces[:count-1], emit); err != nil {
				return err
			}
			x = pieces[count-1]
			y, ym, err = b.next(ctx)
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func walkMultirangeIntersection(ctx context.Context, ops *rangeTypeOps, a, b multirangeSequence, budget CallBudget, emit func(rangeDatum) error) error {
	x, xm, err := a.next(ctx)
	if err != nil {
		return err
	}
	y, ym, err := b.next(ctx)
	if err != nil {
		return err
	}
	for xm && ym {
		value, err := ops.intersection(ctx, x, y, budget)
		if err != nil {
			return err
		}
		if !value.empty {
			if err := emit(value); err != nil {
				return err
			}
		}
		order, err := ops.compareBounds(ctx, x.upper, y.upper, budget)
		if err != nil {
			return err
		}
		if order <= 0 {
			x, xm, err = a.next(ctx)
			if err != nil {
				return err
			}
		}
		if order >= 0 {
			y, ym, err = b.next(ctx)
			if err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
