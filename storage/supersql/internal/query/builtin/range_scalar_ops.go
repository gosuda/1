package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (ops *rangeTypeOps) canonical(ctx context.Context, value rangeDatum, budget CallBudget) (rangeDatum, error) {
	value.lower.side, value.upper.side = rangeLower, rangeUpper
	if err := ctx.Err(); err != nil {
		return rangeDatum{}, err
	}
	if value.empty {
		return rangeDatum{empty: true, upper: rangeBound{side: rangeUpper}}, nil
	}
	order, err := ops.compareValues(ctx, value.lower, value.upper, budget)
	if err != nil {
		return rangeDatum{}, err
	}
	if order > 0 {
		return rangeDatum{}, sqlError("22000", "range lower bound must be less than or equal to range upper bound")
	}
	if order == 0 && !(value.lower.inclusive && value.upper.inclusive) {
		return rangeDatum{empty: true, upper: rangeBound{side: rangeUpper}}, nil
	}
	for _, bound := range []*rangeBound{&value.lower, &value.upper} {
		if bound.infinite {
			bound.inclusive = false
		}
	}
	if ops.family.canonicalCall != nil {
		encoded, err := encodeRange(ctx, ops.family, value, budget)
		if err != nil {
			return rangeDatum{}, err
		}
		arguments := [1]model.Value{encoded}
		result, err := ops.family.canonicalCall(ctx, arguments[:], budget)
		if err != nil {
			return rangeDatum{}, err
		}
		if result.IsNull || result.Type != model.TypeBytes {
			return rangeDatum{}, sqlError("XX000", "range canonical function returned invalid result")
		}
		canonical, err := decodeRange(ctx, ops.family, result)
		if err != nil {
			return rangeDatum{}, err
		}
		if canonical.empty {
			return canonical, nil
		}
		order, err := ops.compareValues(ctx, canonical.lower, canonical.upper, budget)
		if err != nil {
			return rangeDatum{}, err
		}
		if order > 0 || order == 0 && !(canonical.lower.inclusive && canonical.upper.inclusive) {
			return rangeDatum{}, sqlError("XX000", "range canonical function returned an invalid range")
		}
		return canonical, nil
	}
	if ops.family.Canonical == 0 {
		return value, nil
	}
	for _, bound := range []*rangeBound{&value.lower, &value.upper} {
		if bound.infinite || bound.inclusive == (bound.side == rangeLower) {
			continue
		}
		switch ops.family.representation() {
		case 23:
			if bound.integer == math.MaxInt32 {
				return rangeDatum{}, sqlError("22003", "integer out of range")
			}
		case 20:
			if bound.integer == math.MaxInt64 {
				return rangeDatum{}, sqlError("22003", "bigint out of range")
			}
		case 1082:
			if !finiteDate(bound.integer) {
				continue
			}
			if !validDate(bound.integer + 1) {
				return rangeDatum{}, sqlError("22008", "date out of range")
			}
		default:
			continue
		}
		bound.integer++
		bound.inclusive = !bound.inclusive
	}
	order, err = ops.compareValues(ctx, value.lower, value.upper, budget)
	if err != nil {
		return rangeDatum{}, err
	}
	if order > 0 {
		return rangeDatum{}, sqlError("22000", "range lower bound must be less than or equal to range upper bound")
	}
	if order == 0 && !(value.lower.inclusive && value.upper.inclusive) {
		return rangeDatum{empty: true, upper: rangeBound{side: rangeUpper}}, nil
	}
	return value, nil
}

func (ops *rangeTypeOps) overlaps(ctx context.Context, a, b rangeDatum, budget CallBudget) (bool, error) {
	if a.empty || b.empty {
		return false, ctx.Err()
	}
	order, err := ops.compareBounds(ctx, a.lower, b.upper, budget)
	if err != nil || order > 0 {
		return false, err
	}
	order, err = ops.compareBounds(ctx, b.lower, a.upper, budget)
	return order <= 0, err
}

func (ops *rangeTypeOps) containsRange(ctx context.Context, a, b rangeDatum, budget CallBudget) (bool, error) {
	if b.empty {
		return true, ctx.Err()
	}
	if a.empty {
		return false, ctx.Err()
	}
	order, err := ops.compareBounds(ctx, a.lower, b.lower, budget)
	if err != nil || order > 0 {
		return false, err
	}
	order, err = ops.compareBounds(ctx, a.upper, b.upper, budget)
	return order >= 0, err
}

func (ops *rangeTypeOps) containsElement(ctx context.Context, a rangeDatum, element rangeBound, budget CallBudget) (bool, error) {
	if a.empty {
		return false, ctx.Err()
	}
	order, err := ops.compareValues(ctx, a.lower, element, budget)
	outside := order > 0 || (order == 0 && !a.lower.inclusive)
	if err != nil || outside {
		return false, err
	}
	order, err = ops.compareValues(ctx, a.upper, element, budget)
	return order > 0 || order == 0 && a.upper.inclusive, err
}

func (ops *rangeTypeOps) boundsAdjacent(ctx context.Context, upper, lower rangeBound, budget CallBudget) (bool, error) {
	order, err := ops.compareValues(ctx, upper, lower, budget)
	if err != nil || order > 0 {
		return false, err
	}
	if order == 0 {
		return upper.inclusive != lower.inclusive, nil
	}
	if ops.family.Canonical == 0 {
		return false, nil
	}
	upper.inclusive, lower.inclusive = !upper.inclusive, !lower.inclusive
	gap, err := ops.canonical(ctx, rangeDatum{lower: upper, upper: lower}, budget)
	return gap.empty, err
}

func (ops *rangeTypeOps) adjacent(ctx context.Context, a, b rangeDatum, budget CallBudget) (bool, error) {
	if a.empty || b.empty {
		return false, ctx.Err()
	}
	adjacent, err := ops.boundsAdjacent(ctx, a.upper, b.lower, budget)
	if err != nil || adjacent {
		return adjacent, err
	}
	return ops.boundsAdjacent(ctx, b.upper, a.lower, budget)
}

func (ops *rangeTypeOps) union(ctx context.Context, a, b rangeDatum, strict bool, budget CallBudget) (rangeDatum, error) {
	if a.empty {
		return b, ctx.Err()
	}
	if b.empty {
		return a, ctx.Err()
	}
	if strict {
		overlaps, err := ops.overlaps(ctx, a, b, budget)
		if err != nil {
			return rangeDatum{}, err
		}
		if !overlaps {
			adjacent, err := ops.adjacent(ctx, a, b, budget)
			if err != nil {
				return rangeDatum{}, err
			}
			if !adjacent {
				return rangeDatum{}, sqlError("22000", "result of range union would not be contiguous")
			}
		}
	}
	order, err := ops.compareBounds(ctx, a.lower, b.lower, budget)
	if err != nil {
		return rangeDatum{}, err
	}
	if order >= 0 {
		a.lower = b.lower
	}
	order, err = ops.compareBounds(ctx, a.upper, b.upper, budget)
	if err != nil {
		return rangeDatum{}, err
	}
	if order <= 0 {
		a.upper = b.upper
	}
	return ops.canonical(ctx, a, budget)
}

func (ops *rangeTypeOps) intersection(ctx context.Context, a, b rangeDatum, budget CallBudget) (rangeDatum, error) {
	overlaps, err := ops.overlaps(ctx, a, b, budget)
	if err != nil || !overlaps {
		return rangeDatum{empty: true}, err
	}
	order, err := ops.compareBounds(ctx, a.lower, b.lower, budget)
	if err != nil {
		return rangeDatum{}, err
	}
	if order < 0 {
		a.lower = b.lower
	}
	order, err = ops.compareBounds(ctx, a.upper, b.upper, budget)
	if err != nil {
		return rangeDatum{}, err
	}
	if order > 0 {
		a.upper = b.upper
	}
	return ops.canonical(ctx, a, budget)
}

func (ops *rangeTypeOps) difference(ctx context.Context, a, b rangeDatum, budget CallBudget) ([2]rangeDatum, int, error) {
	var pieces [2]rangeDatum
	if a.empty {
		return pieces, 0, ctx.Err()
	}
	overlaps, err := ops.overlaps(ctx, a, b, budget)
	if err != nil {
		return pieces, 0, err
	}
	if !overlaps {
		pieces[0] = a
		return pieces, 1, nil
	}
	lower, err := ops.compareBounds(ctx, a.lower, b.lower, budget)
	if err != nil {
		return pieces, 0, err
	}
	upper, err := ops.compareBounds(ctx, a.upper, b.upper, budget)
	if err != nil {
		return pieces, 0, err
	}
	count := 0
	if lower < 0 {
		cut := b.lower
		cut.inclusive = !cut.inclusive
		piece, err := ops.canonical(ctx, rangeDatum{lower: a.lower, upper: cut}, budget)
		if err != nil {
			return pieces, 0, err
		}
		if !piece.empty {
			pieces[count] = piece
			count++
		}
	}
	if upper > 0 {
		cut := b.upper
		cut.inclusive = !cut.inclusive
		piece, err := ops.canonical(ctx, rangeDatum{lower: cut, upper: a.upper}, budget)
		if err != nil {
			return pieces, 0, err
		}
		if !piece.empty {
			pieces[count] = piece
			count++
		}
	}
	return pieces, count, nil
}

func (ops *rangeTypeOps) scalarPredicate(ctx context.Context, id model.FunctionID, a, b rangeDatum, budget CallBudget) (model.Value, error) {
	var answer bool
	var err error
	switch id {
	case 3857:
		answer, err = ops.overlaps(ctx, a, b, budget)
	case 3859:
		answer, err = ops.containsRange(ctx, a, b, budget)
	case 3861:
		answer, err = ops.containsRange(ctx, b, a, budget)
	case 3862:
		answer, err = ops.adjacent(ctx, a, b, budget)
	case 3863, 3864, 3865, 3866:
		if a.empty || b.empty {
			return model.BoolVal(false), ctx.Err()
		}
		left, right := a.upper, b.lower
		switch id {
		case 3864:
			left, right = b.upper, a.lower
		case 3865:
			left, right = a.upper, b.upper
		case 3866:
			left, right = b.lower, a.lower
		}
		order, compareErr := ops.compareBounds(ctx, left, right, budget)
		err = compareErr
		answer = order < 0 || order == 0 && (id == 3865 || id == 3866)
	default:
		order, compareErr := ops.compareRanges(ctx, a, b, budget)
		err = compareErr
		switch id {
		case 3855:
			answer = order == 0
		case 3856:
			answer = order != 0
		case 3870:
			return model.Int64Val(int64(order)), err
		case 3871:
			answer = order < 0
		case 3872:
			answer = order <= 0
		case 3873:
			answer = order >= 0
		case 3874:
			answer = order > 0
		default:
			return model.Value{}, sqlError("XX000", "invalid range predicate")
		}
	}
	return model.BoolVal(answer), err
}
