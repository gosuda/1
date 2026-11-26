package builtin

import (
	"context"
	"encoding/binary"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type multirangeSequence struct {
	cursor   multirangeCursor
	single   rangeDatum
	isSingle bool
	pending  bool
}

func newMultirangeSequence(ctx context.Context, family RangeFamilyInfo, value model.Value, single bool) (multirangeSequence, error) {
	sequence := multirangeSequence{isSingle: single}
	var err error
	if single {
		sequence.single, err = decodeRange(ctx, family, value)
		sequence.pending = !sequence.single.empty
	} else {
		sequence.cursor, err = newMultirangeCursor(family, value)
	}
	return sequence, err
}

func (sequence *multirangeSequence) next(ctx context.Context) (rangeDatum, bool, error) {
	if !sequence.isSingle {
		return sequence.cursor.next(ctx)
	}
	if err := ctx.Err(); err != nil {
		return rangeDatum{}, false, err
	}
	pending := sequence.pending
	sequence.pending = false
	return sequence.single, pending, nil
}

func multirangeSpan(ctx context.Context, sequence multirangeSequence) (rangeDatum, error) {
	first, more, err := sequence.next(ctx)
	if err != nil {
		return rangeDatum{}, err
	}
	if !more {
		return rangeDatum{empty: true}, nil
	}
	for {
		value, more, err := sequence.next(ctx)
		if err != nil {
			return rangeDatum{}, err
		}
		if !more {
			return first, nil
		}
		first.upper = value.upper
	}
}

func compareMultiranges(ctx context.Context, ops *rangeTypeOps, a, b multirangeSequence, budget CallBudget) (int, error) {
	for {
		x, xm, err := a.next(ctx)
		if err != nil {
			return 0, err
		}
		y, ym, err := b.next(ctx)
		if err != nil {
			return 0, err
		}
		if !xm || !ym {
			if xm {
				return 1, nil
			}
			if ym {
				return -1, nil
			}
			return 0, nil
		}
		order, err := ops.compareRanges(ctx, x, y, budget)
		if err != nil || order != 0 {
			return order, err
		}
	}
}

func multirangeContains(ctx context.Context, ops *rangeTypeOps, a, b multirangeSequence, budget CallBudget) (bool, error) {
	x, xm, err := a.next(ctx)
	if err != nil {
		return false, err
	}
	for {
		y, ym, err := b.next(ctx)
		if err != nil {
			return false, err
		}
		if !ym {
			return true, nil
		}
		for xm {
			order, err := ops.compareBounds(ctx, x.upper, y.lower, budget)
			if err != nil {
				return false, err
			}
			if order >= 0 {
				break
			}
			x, xm, err = a.next(ctx)
			if err != nil {
				return false, err
			}
		}
		if !xm {
			return false, nil
		}
		contains, err := ops.containsRange(ctx, x, y, budget)
		if err != nil || !contains {
			return false, err
		}
	}
}

func multirangeOverlaps(ctx context.Context, ops *rangeTypeOps, a, b multirangeSequence, budget CallBudget) (bool, error) {
	x, xm, err := a.next(ctx)
	if err != nil {
		return false, err
	}
	y, ym, err := b.next(ctx)
	if err != nil {
		return false, err
	}
	for xm && ym {
		overlap, err := ops.overlaps(ctx, x, y, budget)
		if err != nil || overlap {
			return overlap, err
		}
		order, err := ops.compareBounds(ctx, x.upper, y.upper, budget)
		if err != nil {
			return false, err
		}
		if order < 0 {
			x, xm, err = a.next(ctx)
		} else {
			y, ym, err = b.next(ctx)
		}
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

// Canonical algebra uses sizing and encoding passes with constant component
// state, except numeric union where PostgreSQL's sort determines endpoint scale.
func multirangeAlgebra(ctx context.Context, ops *rangeTypeOps, a, b multirangeSequence, id model.FunctionID, budget CallBudget) (model.Value, error) {
	numericUnion := id == 4270 && ops.family.Subtype == 1700
	if numericUnion && a.cursor.remaining != 0 && b.cursor.remaining != 0 {
		return numericMultirangeUnion(ctx, ops, a, b, budget)
	}
	size, count := uint64(4), uint64(0)
	err := walkMultirangeAlgebra(ctx, ops, a, b, id, budget, func(value rangeDatum) error {
		length, err := rangeEncodedSize(ops.family, value)
		if err != nil {
			return err
		}
		if length > math.MaxUint32 || size > math.MaxUint64-4-length || count == math.MaxInt32 {
			return sqlError("54000", "multirange exceeds encoding limit")
		}
		size += 4 + length
		count++
		return nil
	})
	if err != nil {
		return model.Value{}, err
	}
	buffer, err := newByteResult(ctx, size, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	nested, err := budget.After(size)
	if err != nil {
		return model.Value{}, err
	}
	output := binary.BigEndian.AppendUint32(buffer.data[:0], uint32(count))
	err = walkMultirangeAlgebra(ctx, ops, a, b, id, nested, func(value rangeDatum) error {
		length, err := rangeEncodedSize(ops.family, value)
		if err != nil {
			return err
		}
		output = binary.BigEndian.AppendUint32(output, uint32(length))
		output, err = appendRangeBytes(ctx, output, ops.family, value)
		return err
	})
	if err != nil {
		return model.Value{}, err
	}
	return buffer.value(), ctx.Err()
}

func walkMultirangeAlgebra(ctx context.Context, ops *rangeTypeOps, a, b multirangeSequence, id model.FunctionID, budget CallBudget, emit func(rangeDatum) error) error {
	switch id {
	case 4270:
		return walkMultirangeUnion(ctx, ops, a, b, budget, emit)
	case 4271:
		return walkMultirangeDifference(ctx, ops, a, b, budget, emit)
	default:
		return walkMultirangeIntersection(ctx, ops, a, b, budget, emit)
	}
}
