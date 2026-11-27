package builtin

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/decimalint"
)

func numericSign(value decimal) int {
	switch value.special {
	case numericNegativeInfinity:
		return -1
	case numericPositiveInfinity:
		return 1
	case numericNaN:
		return 0
	default:
		return value.coefficient.Sign()
	}
}

func numericNegate(ctx context.Context, work *decimalint.Workspace, value decimal) (decimal, error) {
	switch value.special {
	case numericNegativeInfinity:
		value.special = numericPositiveInfinity
	case numericPositiveInfinity:
		value.special = numericNegativeInfinity
	case numericFinite:
		coefficient, err := work.Neg(ctx, value.coefficient)
		if err != nil {
			return decimal{}, err
		}
		value.coefficient = coefficient
	}
	return value, nil
}

// numericAligned always returns an owned coefficient, even without a scale change.
func numericAligned(ctx context.Context, work *decimalint.Workspace, value decimal, scale int) (result *decimalint.Int, err error) {
	if err = numericWork(ctx, numericDigits(value.coefficient)+scale-value.scale); err != nil {
		return nil, err
	}
	if scale == value.scale {
		return work.Clone(ctx, value.coefficient)
	}
	power, err := numericPower(ctx, work, scale-value.scale)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, power.Close()) }()
	return work.Mul(ctx, value.coefficient, power)
}

func numericCompare(ctx context.Context, left, right decimal) (int, error) {
	if left.special != numericFinite || right.special != numericFinite {
		rank := func(value decimal) int {
			switch value.special {
			case numericNegativeInfinity:
				return 0
			case numericFinite:
				return 1
			case numericPositiveInfinity:
				return 2
			default:
				return 3
			}
		}
		a, b := rank(left), rank(right)
		if a < b {
			return -1, nil
		}
		if a > b {
			return 1, nil
		}
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	leftSign, rightSign := left.coefficient.Sign(), right.coefficient.Sign()
	if leftSign < rightSign {
		return -1, nil
	}
	if leftSign > rightSign {
		return 1, nil
	}
	if leftSign == 0 {
		return 0, nil
	}
	leftDigits, rightDigits := numericDigits(left.coefficient), numericDigits(right.coefficient)
	leftWeight, rightWeight := leftDigits-left.scale, rightDigits-right.scale
	if leftWeight < rightWeight {
		return -leftSign, nil
	}
	if leftWeight > rightWeight {
		return leftSign, nil
	}
	for i := range max(leftDigits, rightDigits) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		x, y := left.coefficient.DecimalDigit(i), right.coefficient.DecimalDigit(i)
		if x < y {
			return -leftSign, nil
		}
		if x > y {
			return leftSign, nil
		}
	}
	return 0, nil
}

func numericArithmetic(ctx context.Context, work *decimalint.Workspace, left, right decimal, op primitiveOp) (result decimal, err error) {
	if op == opGCD || op == opLCM {
		return numericGCDLCM(ctx, work, left, right, op)
	}
	if op == opMi {
		right, err = numericNegate(ctx, work, right)
		if err != nil {
			return decimal{}, err
		}
		if right.special == numericFinite {
			defer func() { err = errors.Join(err, right.coefficient.Close()) }()
		}
		op = opPl
	}
	if left.special == numericNaN || right.special == numericNaN {
		return decimal{special: numericNaN}, nil
	}
	divides := op == opDiv || op == opMod || op == opDivTrunc
	zeroDivisor := right.special == numericFinite && right.coefficient.Sign() == 0
	if divides && zeroDivisor {
		return decimal{}, sqlError("22012", "division by zero")
	}
	if left.special != numericFinite || right.special != numericFinite {
		switch op {
		case opPl:
			if left.special == numericFinite {
				return right, nil
			}
			if right.special == numericFinite || left.special == right.special {
				return left, nil
			}
			return decimal{special: numericNaN}, nil
		case opMul:
			if numericSign(left) == 0 || numericSign(right) == 0 {
				return decimal{special: numericNaN}, nil
			}
		case opDiv, opDivTrunc:
			if right.special != numericFinite {
				if left.special != numericFinite {
					return decimal{special: numericNaN}, nil
				}
				coefficient, err := work.FromInt64(ctx, 0)
				return decimal{coefficient: coefficient}, err
			}
		case opMod:
			if left.special != numericFinite {
				return decimal{special: numericNaN}, nil
			}
			return left, nil
		default:
			return decimal{}, sqlError("XX000", "invalid numeric arithmetic operation")
		}
		kind := numericPositiveInfinity
		if numericSign(left)*numericSign(right) < 0 {
			kind = numericNegativeInfinity
		}
		return decimal{special: kind}, nil
	}
	if op == opMul {
		if err := numericWork(ctx, numericDigits(left.coefficient)+numericDigits(right.coefficient)); err != nil {
			return decimal{}, err
		}
		coefficient, mulErr := work.Mul(ctx, left.coefficient, right.coefficient)
		if mulErr != nil {
			return decimal{}, mulErr
		}
		result := decimal{coefficient: coefficient, scale: left.scale + right.scale}
		// numeric_mul rounds only when the product's display scale exceeds storage.
		if result.scale > numericMaxScale {
			defer func() { err = errors.Join(err, result.coefficient.Close()) }()
			return numericRescale(ctx, work, result, numericMaxScale, true)
		}
		return result, ctx.Err()
	}
	if op == opDiv || op == opDivTrunc {
		return numericDivide(ctx, work, left, right, op == opDivTrunc)
	}
	scale := max(left.scale, right.scale)
	a, err := numericAligned(ctx, work, left, scale)
	if err != nil {
		return decimal{}, err
	}
	defer func() { err = errors.Join(err, a.Close()) }()
	b, err := numericAligned(ctx, work, right, scale)
	if err != nil {
		return decimal{}, err
	}
	defer func() { err = errors.Join(err, b.Close()) }()
	var coefficient *decimalint.Int
	switch op {
	case opPl:
		if err := numericWork(ctx, max(numericDigits(a), numericDigits(b))+1); err != nil {
			return decimal{}, err
		}
		coefficient, err = work.Add(ctx, a, b)
	case opMod:
		coefficient, err = work.Rem(ctx, a, b)
	default:
		return decimal{}, sqlError("XX000", "invalid numeric arithmetic operation")
	}
	return decimal{coefficient: coefficient, scale: scale}, err
}

// PostgreSQL select_div_scale estimates the quotient using base-10000 leading
// digits, not a decimal logarithm. Equal leading digits deliberately lower it.
func numericLeading(value decimal) (weight, first int) {
	if value.coefficient.Sign() == 0 {
		return 0, 0
	}
	digits := numericDigits(value.coefficient)
	decimalWeight := digits - value.scale - 1
	weight = decimalWeight / 4
	if decimalWeight < 0 && decimalWeight%4 != 0 {
		weight--
	}
	width := decimalWeight - weight*4 + 1
	for i := range width {
		first *= 10
		first += int(value.coefficient.DecimalDigit(i))
	}
	return weight, first
}

func numericDivide(ctx context.Context, work *decimalint.Workspace, left, right decimal, truncate bool) (result decimal, err error) {
	scale := 0
	if !truncate {
		leftWeight, leftFirst := numericLeading(left)
		rightWeight, rightFirst := numericLeading(right)
		weight := leftWeight - rightWeight
		if leftFirst <= rightFirst {
			weight--
		}
		scale = min(max(16-weight*4, left.scale, right.scale, 0), 1000)
	}
	shift := right.scale + scale - left.scale
	numerator, denominator := left.coefficient, right.coefficient
	var power, scaled, quotient, remainder *decimalint.Int
	defer func() { err = errors.Join(err, power.Close(), scaled.Close(), quotient.Close(), remainder.Close()) }()
	if shift >= 0 {
		if err = numericWork(ctx, numericDigits(numerator)+shift); err != nil {
			return decimal{}, err
		}
		power, err = numericPower(ctx, work, shift)
		if err != nil {
			return decimal{}, err
		}
		scaled, err = work.Mul(ctx, numerator, power)
		numerator = scaled
	} else {
		if err = numericWork(ctx, numericDigits(denominator)-shift); err != nil {
			return decimal{}, err
		}
		power, err = numericPower(ctx, work, -shift)
		if err != nil {
			return decimal{}, err
		}
		scaled, err = work.Mul(ctx, denominator, power)
		denominator = scaled
	}
	if err != nil {
		return decimal{}, err
	}
	quotient, remainder, err = work.QuoRem(ctx, numerator, denominator)
	if err != nil {
		return decimal{}, err
	}
	if !truncate {
		var up bool
		up, err = numericRoundHalf(ctx, work, remainder, denominator)
		if err != nil {
			return decimal{}, err
		}
		if up {
			quotient, err = numericIncrement(ctx, work, quotient, left.coefficient.Sign()*right.coefficient.Sign())
			if err != nil {
				return decimal{}, err
			}
		}
	}
	result = decimal{coefficient: quotient, scale: scale}
	quotient = nil
	return result, nil
}
