package builtin

import "context"

// PostgreSQL sort_template.h ordering matters when equal numeric endpoints have
// different scales: the subsequent union selects the second range's bounds.
func sortMultirange(ctx context.Context, ops *rangeTypeOps, values []rangeDatum, budget CallBudget) error {
	for len(values) > 1 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(values) < 7 {
			return insertSortMultirange(ctx, ops, values, budget)
		}
		sorted, err := multirangePresorted(ctx, ops, values, budget)
		if err != nil || sorted {
			return err
		}
		pivot, err := multirangePivot(ctx, ops, values, budget)
		if err != nil {
			return err
		}
		values[0], values[pivot] = values[pivot], values[0]
		left, right, err := partitionMultirange(ctx, ops, values, budget)
		if err != nil {
			return err
		}
		if left <= right {
			if err := sortMultirange(ctx, ops, values[:left], budget); err != nil {
				return err
			}
			values = values[len(values)-right:]
		} else {
			if err := sortMultirange(ctx, ops, values[len(values)-right:], budget); err != nil {
				return err
			}
			values = values[:left]
		}
	}
	return ctx.Err()
}

func insertSortMultirange(ctx context.Context, ops *rangeTypeOps, values []rangeDatum, budget CallBudget) error {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0; j-- {
			order, err := ops.compareRanges(ctx, values[j-1], values[j], budget)
			if err != nil {
				return err
			}
			if order <= 0 {
				break
			}
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
	return ctx.Err()
}

func multirangePresorted(ctx context.Context, ops *rangeTypeOps, values []rangeDatum, budget CallBudget) (bool, error) {
	for i := 1; i < len(values); i++ {
		order, err := ops.compareRanges(ctx, values[i-1], values[i], budget)
		if err != nil || order > 0 {
			return false, err
		}
	}
	return true, nil
}

func medianMultirange(ctx context.Context, ops *rangeTypeOps, values []rangeDatum, a, b, c int, budget CallBudget) (int, error) {
	ab, err := ops.compareRanges(ctx, values[a], values[b], budget)
	if err != nil {
		return 0, err
	}
	bc, err := ops.compareRanges(ctx, values[b], values[c], budget)
	if err != nil {
		return 0, err
	}
	ascending := ab < 0 && bc < 0
	descending := ab >= 0 && bc > 0
	if ascending || descending {
		return b, nil
	}
	ac, err := ops.compareRanges(ctx, values[a], values[c], budget)
	if err != nil {
		return 0, err
	}
	if ab < 0 {
		if ac < 0 {
			return c, nil
		}
		return a, nil
	}
	if ac < 0 {
		return a, nil
	}
	return c, nil
}

func multirangePivot(ctx context.Context, ops *rangeTypeOps, values []rangeDatum, budget CallBudget) (int, error) {
	middle := len(values) / 2
	if len(values) == 7 {
		return middle, nil
	}
	left, right := 0, len(values)-1
	if len(values) > 40 {
		stride := len(values) / 8
		var err error
		left, err = medianMultirange(ctx, ops, values, left, left+stride, left+2*stride, budget)
		if err != nil {
			return 0, err
		}
		middle, err = medianMultirange(ctx, ops, values, middle-stride, middle, middle+stride, budget)
		if err != nil {
			return 0, err
		}
		right, err = medianMultirange(ctx, ops, values, right-2*stride, right-stride, right, budget)
		if err != nil {
			return 0, err
		}
	}
	return medianMultirange(ctx, ops, values, left, middle, right, budget)
}

func partitionMultirange(ctx context.Context, ops *rangeTypeOps, values []rangeDatum, budget CallBudget) (int, int, error) {
	a, b, c, d := 1, 1, len(values)-1, len(values)-1
	for {
		for b <= c {
			order, err := ops.compareRanges(ctx, values[b], values[0], budget)
			if err != nil {
				return 0, 0, err
			}
			if order > 0 {
				break
			}
			if order == 0 {
				values[a], values[b] = values[b], values[a]
				a++
			}
			b++
		}
		for b <= c {
			order, err := ops.compareRanges(ctx, values[c], values[0], budget)
			if err != nil {
				return 0, 0, err
			}
			if order < 0 {
				break
			}
			if order == 0 {
				values[c], values[d] = values[d], values[c]
				d--
			}
			c--
		}
		if b > c {
			break
		}
		values[b], values[c] = values[c], values[b]
		b++
		c--
	}
	count := min(a, b-a)
	if err := swapMultirangeBlocks(ctx, values, 0, b-count, count); err != nil {
		return 0, 0, err
	}
	count = min(d-c, len(values)-d-1)
	if err := swapMultirangeBlocks(ctx, values, b, len(values)-count, count); err != nil {
		return 0, 0, err
	}
	return b - a, d - c, nil
}

func swapMultirangeBlocks(ctx context.Context, values []rangeDatum, a, b, count int) error {
	for i := range count {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		values[a+i], values[b+i] = values[b+i], values[a+i]
	}
	return nil
}
