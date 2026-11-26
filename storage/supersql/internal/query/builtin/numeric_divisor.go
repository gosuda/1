package builtin

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/decimalint"
)

func numericGCDLCM(ctx context.Context, work *decimalint.Workspace, left, right decimal, op primitiveOp) (result decimal, err error) {
	if err = ctx.Err(); err != nil {
		return decimal{}, err
	}
	if left.special != numericFinite || right.special != numericFinite {
		return decimal{special: numericNaN}, nil
	}
	scale := max(left.scale, right.scale)
	leftZero, rightZero := left.coefficient.Sign() == 0, right.coefficient.Sign() == 0
	if leftZero || rightZero {
		if op == opLCM || leftZero && rightZero {
			coefficient, err := work.FromInt64(ctx, 0)
			return decimal{coefficient: coefficient, scale: scale}, err
		}
		if leftZero {
			left = right
		}
		return numericDivisorMagnitude(ctx, work, left, scale)
	}
	equal, err := numericMagnitudeEqual(ctx, left.coefficient, right.coefficient)
	if err != nil {
		return decimal{}, err
	}
	if equal {
		// Equal coefficients differ only by a power of ten, so one divides the other.
		useRightForGCD := op == opGCD && left.scale < right.scale
		useRightForLCM := op == opLCM && left.scale > right.scale
		if useRightForGCD || useRightForLCM {
			left = right
		}
		return numericDivisorMagnitude(ctx, work, left, scale)
	}
	var a, b, gcd, quotient, product *decimalint.Int
	defer func() { err = errors.Join(err, a.Close(), b.Close(), gcd.Close(), quotient.Close(), product.Close()) }()
	a, err = numericAligned(ctx, work, left, scale)
	if err != nil {
		return decimal{}, err
	}
	b, err = numericAligned(ctx, work, right, scale)
	if err != nil {
		return decimal{}, err
	}
	equal, err = numericMagnitudeEqual(ctx, a, b)
	if err != nil {
		return decimal{}, err
	}
	if equal {
		coefficient, err := work.Abs(ctx, a)
		return decimal{coefficient: coefficient, scale: scale}, err
	}
	gcd, err = work.GCD(ctx, a, b)
	if err != nil {
		return decimal{}, err
	}
	if op == opLCM {
		quotient, err = work.Quo(ctx, a, gcd)
		if err != nil {
			return decimal{}, err
		}
		if err = numericWork(ctx, numericDigits(quotient)+numericDigits(b)); err != nil {
			return decimal{}, err
		}
		product, err = work.Mul(ctx, quotient, b)
		if err != nil {
			return decimal{}, err
		}
		coefficient, err := work.Abs(ctx, product)
		return decimal{coefficient: coefficient, scale: scale}, err
	}
	result = decimal{coefficient: gcd, scale: scale}
	gcd = nil
	return result, nil
}

func numericMagnitudeEqual(ctx context.Context, left, right *decimalint.Int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if left.Digits() != right.Digits() {
		return false, nil
	}
	for i := range left.Digits() {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if left.DecimalDigit(i) != right.DecimalDigit(i) {
			return false, nil
		}
	}
	return true, nil
}

func numericDivisorMagnitude(ctx context.Context, work *decimalint.Workspace, value decimal, scale int) (result decimal, err error) {
	if scale == value.scale {
		if err = numericWork(ctx, numericDigits(value.coefficient)); err != nil {
			return decimal{}, err
		}
		coefficient, err := work.Abs(ctx, value.coefficient)
		return decimal{coefficient: coefficient, scale: scale}, err
	}
	coefficient, err := numericAligned(ctx, work, value, scale)
	if err != nil {
		return decimal{}, err
	}
	defer func() { err = errors.Join(err, coefficient.Close()) }()
	magnitude, err := work.Abs(ctx, coefficient)
	return decimal{coefficient: magnitude, scale: scale}, err
}
