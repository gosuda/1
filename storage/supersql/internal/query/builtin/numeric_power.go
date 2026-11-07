package builtin

import (
	"context"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/decimalint"
)

func numericExponentiate(ctx context.Context, work *decimalint.Workspace, base, exponent decimal) (result decimal, err error) {
	checkpoint := work.Checkpoint()
	defer func() { err = errors.Join(err, work.ReleaseAfter(checkpoint, result.coefficient)) }()
	c := numericCalculation{ctx: ctx, work: work}
	one := c.integer(1)
	if c.err != nil {
		return decimal{}, c.err
	}
	if base.special == numericNaN {
		if exponent.special == numericFinite && exponent.coefficient.Sign() == 0 {
			return one, nil
		}
		return base, nil
	}
	if exponent.special == numericNaN {
		if c.compare(base, one) == 0 && c.err == nil {
			return one, nil
		}
		return exponent, c.err
	}
	baseSign, exponentSign := numericSign(base), numericSign(exponent)
	if baseSign == 0 && exponentSign < 0 {
		return decimal{}, sqlError("2201F", "zero raised to a negative power is undefined")
	}
	integral, odd := true, false
	if exponent.special == numericFinite {
		minimum, err := numericMinimumScale(ctx, exponent)
		if err != nil {
			return decimal{}, err
		}
		integral = minimum == 0
		position := numericDigits(exponent.coefficient) - exponent.scale - 1
		if integral && position >= 0 {
			odd = exponent.coefficient.DecimalDigit(position)&1 != 0
		}
	}
	if baseSign < 0 && !integral {
		return decimal{}, sqlError("2201F", "a negative number raised to a non-integer power yields a complex result")
	}
	if base.special != numericFinite || exponent.special != numericFinite {
		if c.compare(base, one) == 0 || exponentSign == 0 {
			return one, c.err
		}
		if baseSign == 0 {
			result = c.integer(0)
			return result, c.err
		}
		if exponent.special != numericFinite {
			magnitudeOrder := 1
			if base.special == numericFinite {
				magnitude := base
				if baseSign < 0 {
					magnitude, c.err = numericNegate(ctx, work, base)
				}
				magnitudeOrder = c.compare(magnitude, one)
			}
			if magnitudeOrder == 0 {
				return one, c.err
			}
			if (magnitudeOrder > 0) == (exponentSign > 0) {
				return decimal{special: numericPositiveInfinity}, c.err
			}
			result = c.integer(0)
			return result, c.err
		}
		if exponentSign < 0 {
			result = c.integer(0)
			return result, c.err
		}
		if baseSign < 0 && odd {
			return decimal{special: numericNegativeInfinity}, c.err
		}
		return decimal{special: numericPositiveInfinity}, c.err
	}
	if integral {
		integer, err := numericRescale(ctx, work, exponent, 0, false)
		if err != nil {
			return decimal{}, err
		}
		value, fits := integer.coefficient.Int64()
		if err = integer.coefficient.Close(); err != nil {
			return decimal{}, err
		}
		if fits && value >= math.MinInt32 && value <= math.MaxInt32 {
			return numericPowerInteger(ctx, work, base, int(value), exponent.scale)
		}
	}
	if baseSign == 0 {
		result = c.integer(0)
		result.scale = 16
		return result, c.err
	}
	if baseSign < 0 {
		base, err = numericNegate(ctx, work, base)
		if err != nil {
			return decimal{}, err
		}
	}
	logWeight, err := numericLnWeight(ctx, work, base)
	if err != nil {
		return decimal{}, err
	}
	localScale := max(8-logWeight, 0)
	logarithm, err := numericLnAtScale(ctx, work, base, localScale)
	if err != nil {
		return decimal{}, err
	}
	product := c.multiply(logarithm, exponent, localScale)
	if c.err != nil {
		return decimal{}, c.err
	}
	estimate, err := numericScaleEstimate(ctx, product)
	if err != nil {
		return decimal{}, err
	}
	if math.Abs(estimate) > 6020 {
		if estimate > 0 {
			return decimal{}, numericOverflow()
		}
		result = c.integer(0)
		result.scale = 1000
		return result, c.err
	}
	estimate *= 0.434294481903252
	scale := numericDisplayScale(int(estimate), base.scale, exponent.scale)
	significant := max(scale+int(estimate), 0)
	localScale = max(significant-logWeight+8, 0)
	c.err = errors.Join(logarithm.coefficient.Close(), product.coefficient.Close())
	if c.err != nil {
		return decimal{}, c.err
	}
	logarithm, err = numericLnAtScale(ctx, work, base, localScale)
	if err != nil {
		return decimal{}, err
	}
	product = c.multiply(logarithm, exponent, localScale)
	if c.err != nil {
		return decimal{}, c.err
	}
	result, err = numericExpAtScale(ctx, work, product, scale)
	if err != nil {
		return decimal{}, err
	}
	if baseSign < 0 && odd {
		negative, err := numericNegate(ctx, work, result)
		if err != nil {
			return decimal{}, err
		}
		c.replace(&result, negative)
	}
	return result, c.err
}

func numericPowerInteger(ctx context.Context, work *decimalint.Workspace, base decimal, exponent, exponentScale int) (result decimal, err error) {
	checkpoint := work.Checkpoint()
	defer func() { err = errors.Join(err, work.ReleaseAfter(checkpoint, result.coefficient)) }()
	c := numericCalculation{ctx: ctx, work: work}
	var estimate float64
	if base.coefficient.Sign() != 0 {
		weight, first := numericLeading(base)
		leading := float64(first)
		power := weight * 4
		digits := numericDigits(base.coefficient)
		width := digits - base.scale - weight*4
		last, err := numericSignificantDigits(ctx, base)
		if err != nil {
			return decimal{}, err
		}
		for i := 1; i < 4 && width+(i-1)*4 < last; i++ {
			for j := range 4 {
				leading = leading*10 + float64(base.coefficient.DecimalDigit(width+(i-1)*4+j))
			}
			power -= 4
		}
		estimate = float64(exponent) * (math.Log10(leading) + float64(power))
	}
	if estimate > numericMaxIntegerDigits {
		return decimal{}, numericOverflow()
	}
	if estimate+1 < -1000 {
		result = c.integer(0)
		result.scale = 1000
		return result, c.err
	}
	scale := numericDisplayScale(int(estimate), base.scale, exponentScale)
	one := c.integer(1)
	switch exponent {
	case 0:
		result = c.round(one, scale)
		return result, c.err
	case 1:
		result = c.round(base, scale)
		return result, c.err
	case -1:
		result = c.divide(one, base, scale)
		return result, c.err
	case 2:
		result = c.multiply(base, base, scale)
		return result, c.err
	}
	if base.coefficient.Sign() == 0 {
		result = c.integer(0)
		result.scale = scale
		return result, c.err
	}
	significant := 1 + scale + int(estimate) + int(math.Log(math.Abs(float64(exponent)))) + 8
	negative := exponent < 0
	mask := uint64(exponent)
	if negative {
		mask = uint64(-int64(exponent))
	}
	power := c.clone(base)
	result = c.clone(one)
	if mask&1 != 0 {
		c.replace(&result, c.clone(base))
	}
	for mask >>= 1; mask > 0 && c.err == nil; mask >>= 1 {
		weight, _ := numericLeading(power)
		localScale := max(min(significant-weight*8, power.scale*2), 0)
		c.replace(&power, c.multiply(power, power, localScale))
		if c.err != nil {
			break
		}
		if mask&1 != 0 {
			powerWeight, _ := numericLeading(power)
			resultWeight, _ := numericLeading(result)
			localScale = max(min(significant-(powerWeight+resultWeight)*4, power.scale+result.scale), 0)
			c.replace(&result, c.multiply(power, result, localScale))
		}
		if c.err != nil {
			break
		}
		if numericDigits(power.coefficient)-power.scale > numericMaxIntegerDigits || numericDigits(result.coefficient)-result.scale > numericMaxIntegerDigits {
			if !negative {
				return decimal{}, numericOverflow()
			}
			c.replace(&result, c.integer(0))
			negative = false
			break
		}
	}
	if negative {
		c.replace(&result, c.divide(one, result, scale))
	} else {
		c.replace(&result, c.round(result, scale))
	}
	return result, c.err
}
