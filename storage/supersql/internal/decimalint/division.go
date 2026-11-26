package decimalint

import (
	"context"
	"errors"
)

// QuoRem truncates toward zero; a nonzero remainder has the dividend's sign.
func (workspace *Workspace) QuoRem(ctx context.Context, left, right *Int) (quotient, remainder *Int, err error) {
	if err = workspace.check(ctx, left, right); err != nil {
		return nil, nil, err
	}
	if len(right.limbs) == 0 {
		return nil, nil, ErrDivisionByZero
	}
	var q, r, dividend, divisor *Int
	defer func() {
		err = errors.Join(err, dividend.Close(), divisor.Close())
		if err != nil {
			err = errors.Join(err, q.Close(), r.Close())
			quotient, remainder = nil, nil
		}
	}()
	order, err := compareMagnitude(ctx, left, right)
	if err != nil {
		return nil, nil, err
	}
	if order < 0 {
		q, err = workspace.newInt(ctx, 0)
		if err != nil {
			return nil, nil, err
		}
		r, err = workspace.Clone(ctx, left)
		return q, r, err
	}
	q, err = workspace.newInt(ctx, len(left.limbs)-len(right.limbs)+1)
	if err != nil {
		return nil, nil, err
	}
	r, err = workspace.newInt(ctx, len(right.limbs))
	if err != nil {
		return nil, nil, err
	}
	if len(right.limbs) == 1 {
		var carry uint64
		for i := len(left.limbs) - 1; i >= 0; i-- {
			if i&255 == 0 {
				if err = ctx.Err(); err != nil {
					return nil, nil, err
				}
			}
			current := carry*limbBase + uint64(left.limbs[i])
			q.limbs[i] = uint32(current / uint64(right.limbs[0]))
			carry = current % uint64(right.limbs[0])
		}
		r.limbs[0] = uint32(carry)
	} else {
		if len(left.limbs) == int(^uint(0)>>1) {
			return nil, nil, ErrLimit
		}
		dividend, err = workspace.newInt(ctx, len(left.limbs)+1)
		if err != nil {
			return nil, nil, err
		}
		divisor, err = workspace.newInt(ctx, len(right.limbs))
		if err != nil {
			return nil, nil, err
		}
		if err = divideMagnitude(ctx, left, right, dividend, divisor, q, r); err != nil {
			return nil, nil, err
		}
	}
	q.negative = left.negative != right.negative
	r.negative = left.negative
	q.normalize()
	r.normalize()
	return q, r, nil
}

// divideMagnitude uses Knuth's normalized division. All four writable values
// are unpublished workspace allocations, disjoint from both borrowed operands.
func divideMagnitude(ctx context.Context, left, right, dividend, divisor, quotient, remainder *Int) error {
	n := len(right.limbs)
	factor := limbBase / (uint64(right.limbs[n-1]) + 1)
	var carry uint64
	for i, digit := range right.limbs {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		product := uint64(digit)*factor + carry
		divisor.limbs[i] = uint32(product % limbBase)
		carry = product / limbBase
	}
	carry = 0
	for i, digit := range left.limbs {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		product := uint64(digit)*factor + carry
		dividend.limbs[i] = uint32(product % limbBase)
		carry = product / limbBase
	}
	dividend.limbs[len(left.limbs)] = uint32(carry)
	top := uint64(divisor.limbs[n-1])
	for j := len(quotient.limbs) - 1; j >= 0; j-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		high, low := uint64(dividend.limbs[j+n]), uint64(dividend.limbs[j+n-1])
		var estimate, residual uint64
		if high == top {
			estimate, residual = limbBase-1, low+top
		} else {
			pair := high*limbBase + low
			estimate, residual = pair/top, pair%top
		}
		// Normalization makes top >= base/2, so this estimate is at most
		// two too large. The second divisor limb removes both errors except
		// possibly one; a negative subtraction then needs exactly one add-back.
		// residual >= base already proves the estimate cannot be too large
		// by this two-limb comparison and also guards the multiplication.
		for residual < limbBase && estimate*uint64(divisor.limbs[n-2]) > residual*limbBase+uint64(dividend.limbs[j+n-2]) {
			estimate--
			residual += top
		}
		window := dividend.limbs[j : j+n+1]
		difference, err := subtractDivisor(ctx, window, divisor.limbs, estimate)
		if err != nil {
			return err
		}
		if difference < 0 {
			estimate--
			carry, err := addDivisor(ctx, window, divisor.limbs)
			if err != nil {
				return err
			}
			difference += int64(carry)
		}
		dividend.limbs[j+n] = uint32(difference)
		quotient.limbs[j] = uint32(estimate)
	}
	carry = 0
	for i := n - 1; i >= 0; i-- {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		current := carry*limbBase + uint64(dividend.limbs[i])
		remainder.limbs[i] = uint32(current / factor)
		carry = current % factor
	}
	return ctx.Err()
}

func subtractDivisor(ctx context.Context, target, divisor []uint32, estimate uint64) (int64, error) {
	var borrow uint64
	for i, digit := range divisor {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		product := estimate*uint64(digit) + borrow
		borrow = product / limbBase
		subtrahend := product % limbBase
		current := uint64(target[i])
		if current < subtrahend {
			current += limbBase
			borrow++
		}
		target[i] = uint32(current - subtrahend)
	}
	return int64(target[len(divisor)]) - int64(borrow), nil
}

func addDivisor(ctx context.Context, target, divisor []uint32) (uint64, error) {
	var carry uint64
	for i, digit := range divisor {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		sum := uint64(target[i]) + uint64(digit) + carry
		target[i] = uint32(sum % limbBase)
		carry = sum / limbBase
	}
	return carry, nil
}

func (workspace *Workspace) Quo(ctx context.Context, left, right *Int) (*Int, error) {
	quotient, remainder, err := workspace.QuoRem(ctx, left, right)
	if err != nil {
		return nil, err
	}
	if err = remainder.Close(); err != nil {
		return nil, errors.Join(err, quotient.Close())
	}
	return quotient, nil
}

func (workspace *Workspace) Rem(ctx context.Context, left, right *Int) (*Int, error) {
	quotient, remainder, err := workspace.QuoRem(ctx, left, right)
	if err != nil {
		return nil, err
	}
	if err = quotient.Close(); err != nil {
		return nil, errors.Join(err, remainder.Close())
	}
	return remainder, nil
}
