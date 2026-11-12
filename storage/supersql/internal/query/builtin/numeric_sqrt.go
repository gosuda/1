package builtin

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/decimalint"
)

// numericSqrt rounds at the caller's scale; scalar and aggregate scale selection differ.
func numericSqrt(ctx context.Context, work *decimalint.Workspace, value decimal, scale int) (result decimal, err error) {
	if err = ctx.Err(); err != nil {
		return decimal{}, err
	}
	switch value.special {
	case numericNaN, numericPositiveInfinity:
		return decimal{special: value.special}, nil
	case numericNegativeInfinity:
		return decimal{}, sqlError("2201F", "cannot take square root of a negative number")
	}
	if value.coefficient.Sign() < 0 {
		return decimal{}, sqlError("2201F", "cannot take square root of a negative number")
	}
	if err = numericWork(ctx, scale); err != nil {
		return decimal{}, err
	}
	if err = numericWork(ctx, value.scale); err != nil {
		return decimal{}, err
	}
	if value.coefficient.Sign() == 0 {
		coefficient, err := work.FromInt64(ctx, 0)
		return decimal{coefficient: coefficient, scale: scale}, err
	}
	digits := numericDigits(value.coefficient)
	if err = numericWork(ctx, digits); err != nil {
		return decimal{}, err
	}
	shift := 2*scale - value.scale
	if shift < -digits {
		// The scaled radicand is below 0.1, hence below the rounding midpoint 0.25.
		coefficient, err := work.FromInt64(ctx, 0)
		return decimal{coefficient: coefficient, scale: scale}, err
	}
	if err = numericWork(ctx, digits+max(shift, 0)); err != nil {
		return decimal{}, err
	}
	radicand := value.coefficient
	var power, scaled, fraction, root, square, remainder, four, quarter, rest *decimalint.Int
	defer func() {
		err = errors.Join(err, power.Close(), scaled.Close(), fraction.Close(), root.Close(), square.Close(), remainder.Close(), four.Close(), quarter.Close(), rest.Close())
	}()
	if shift > 0 {
		power, err = numericPower(ctx, work, shift)
		if err != nil {
			return decimal{}, err
		}
		scaled, err = work.Mul(ctx, radicand, power)
		if err != nil {
			return decimal{}, err
		}
		radicand = scaled
	} else if shift < 0 {
		power, err = numericPower(ctx, work, -shift)
		if err != nil {
			return decimal{}, err
		}
		scaled, fraction, err = work.QuoRem(ctx, radicand, power)
		if err != nil {
			return decimal{}, err
		}
		radicand = scaled
	}
	root, err = work.Sqrt(ctx, radicand)
	if err != nil {
		return decimal{}, err
	}
	// floor(sqrt(N/D)) = floor(sqrt(floor(N/D))). Its exact rounding
	// midpoint is q²+q+1/4, so only the equality case needs the fraction.
	square, err = work.Mul(ctx, root, root)
	if err != nil {
		return decimal{}, err
	}
	remainder, err = work.Sub(ctx, radicand, square)
	if err != nil {
		return decimal{}, err
	}
	comparison, err := remainder.Cmp(ctx, root)
	if err != nil {
		return decimal{}, err
	}
	if comparison == 0 && shift < 0 {
		four, err = work.FromInt64(ctx, 4)
		if err != nil {
			return decimal{}, err
		}
		quarter, rest, err = work.QuoRem(ctx, power, four)
		if err != nil {
			return decimal{}, err
		}
		if rest.Sign() != 0 {
			quarter, err = numericIncrement(ctx, work, quarter, 1)
			if err != nil {
				return decimal{}, err
			}
		}
		comparison, err = fraction.Cmp(ctx, quarter)
		if err != nil {
			return decimal{}, err
		}
		if comparison >= 0 {
			comparison = 1
		} else {
			comparison = 0
		}
	}
	if comparison > 0 {
		root, err = numericIncrement(ctx, work, root, 1)
		if err != nil {
			return decimal{}, err
		}
	}
	result = decimal{coefficient: root, scale: scale}
	root = nil
	return result, nil
}
