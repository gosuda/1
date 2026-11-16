package builtin

import (
	"context"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/decimalint"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileWidthBucketRange(signature Signature) scalarKernel {
	switch signature.ID {
	case 320:
		return widthBucketFloat
	case 2170:
		return widthBucketNumeric
	default:
		return nil
	}
}

func widthBucketFloat(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
	operand, bound1, bound2, count := args[0].FloatVal, args[1].FloatVal, args[2].FloatVal, args[3].IntVal
	if count <= 0 {
		return model.Value{}, sqlError("2201G", "count must be greater than zero")
	}
	if math.IsNaN(operand) || math.IsNaN(bound1) || math.IsNaN(bound2) {
		return model.Value{}, sqlError("2201G", "operand, lower bound, and upper bound cannot be NaN")
	}
	if math.IsInf(bound1, 0) || math.IsInf(bound2, 0) {
		return model.Value{}, sqlError("2201G", "lower and upper bounds must be finite")
	}
	var quotient float64
	switch {
	case bound1 < bound2:
		if operand < bound1 {
			return model.Int64Val(0), nil
		}
		if operand >= bound2 {
			return widthBucketOverflowBucket(count)
		}
		span := bound2 - bound1
		if math.IsInf(span, 0) {
			// Halving first preserves finite arithmetic when the span exceeds DBL_MAX.
			quotient = (operand/2 - bound1/2) / (bound2/2 - bound1/2)
		} else {
			quotient = (operand - bound1) / span
		}
	case bound1 > bound2:
		if operand > bound1 {
			return model.Int64Val(0), nil
		}
		if operand <= bound2 {
			return widthBucketOverflowBucket(count)
		}
		span := bound1 - bound2
		if math.IsInf(span, 0) {
			quotient = (bound1/2 - operand/2) / (bound1/2 - bound2/2)
		} else {
			quotient = (bound1 - operand) / span
		}
	default:
		return model.Value{}, sqlError("2201G", "lower bound cannot equal upper bound")
	}
	// PostgreSQL divides before multiplying, then clamps an interior value rounded to the endpoint.
	bucket := min(int64(float64(count)*quotient), count-1)
	return model.Int64Val(bucket + 1), nil
}

func widthBucketOverflowBucket(count int64) (model.Value, error) {
	if count == math.MaxInt32 {
		return model.Value{}, integerOverflow(23)
	}
	return model.Int64Val(count + 1), nil
}

func widthBucketNumeric(ctx context.Context, args []model.Value, budget CallBudget) (result model.Value, err error) {
	work, err := budget.NewWorkspace(ctx)
	if err != nil {
		return model.Value{}, numericAllocationError(err)
	}
	defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
	var numbers [3]decimal
	for i := range len(numbers) {
		value, err := parseNumeric(ctx, work, args[i].StrVal)
		if err != nil {
			return model.Value{}, err
		}
		if value.special == numericFinite && value.scale > numericMaxScale {
			return model.Value{}, numericOverflow()
		}
		numbers[i] = value
	}
	operand, bound1, bound2, count := numbers[0], numbers[1], numbers[2], args[3].IntVal
	if count <= 0 {
		return model.Value{}, sqlError("2201G", "count must be greater than zero")
	}
	if operand.special == numericNaN || bound1.special == numericNaN || bound2.special == numericNaN {
		return model.Value{}, sqlError("2201G", "operand, lower bound, and upper bound cannot be NaN")
	}
	if bound1.special != numericFinite || bound2.special != numericFinite {
		return model.Value{}, sqlError("2201G", "lower and upper bounds must be finite")
	}
	order, err := numericCompare(ctx, bound1, bound2)
	if err != nil {
		return model.Value{}, err
	}
	if order == 0 {
		return model.Value{}, sqlError("2201G", "lower bound cannot equal upper bound")
	}
	if operand.special != numericFinite {
		beforeAscending := operand.special == numericNegativeInfinity && order < 0
		beforeDescending := operand.special == numericPositiveInfinity && order > 0
		if beforeAscending || beforeDescending {
			return model.Int64Val(0), nil
		}
		return widthBucketOverflowBucket(count)
	}
	scale := max(operand.scale, bound1.scale, bound2.scale)
	var aligned [3]*decimalint.Int
	for i := range len(aligned) {
		aligned[i], err = numericAligned(ctx, work, numbers[i], scale)
		if err != nil {
			return model.Value{}, err
		}
	}
	value, first, second := aligned[0], aligned[1], aligned[2]
	firstOrder, err := value.Cmp(ctx, first)
	if err != nil {
		return model.Value{}, err
	}
	if firstOrder*order > 0 {
		return model.Int64Val(0), nil
	}
	if firstOrder == 0 {
		return model.Int64Val(1), nil
	}
	secondOrder, err := value.Cmp(ctx, second)
	if err != nil {
		return model.Value{}, err
	}
	if secondOrder*order <= 0 {
		return widthBucketOverflowBucket(count)
	}
	if err := numericWork(ctx, max(numericDigits(value), numericDigits(first), numericDigits(second))+1); err != nil {
		return model.Value{}, err
	}
	numerator, err := work.Sub(ctx, value, first)
	if err != nil {
		return model.Value{}, err
	}
	denominator, err := work.Sub(ctx, second, first)
	if err != nil {
		return model.Value{}, err
	}
	factor, err := work.FromInt64(ctx, count)
	if err != nil {
		return model.Value{}, err
	}
	if err := numericWork(ctx, numericDigits(numerator)+numericDigits(factor)); err != nil {
		return model.Value{}, err
	}
	numerator, err = work.Mul(ctx, numerator, factor)
	if err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	// The common decimal scale cancels; both differences have the same sign.
	numerator, err = work.Quo(ctx, numerator, denominator)
	if err != nil {
		return model.Value{}, err
	}
	integer, ok := numerator.Int64()
	if !ok {
		return model.Value{}, integerOverflow(23)
	}
	return model.Int64Val(integer + 1), ctx.Err()
}
