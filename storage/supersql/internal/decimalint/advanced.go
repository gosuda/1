package decimalint

import (
	"context"
	"errors"
)

func (workspace *Workspace) GCD(ctx context.Context, left, right *Int) (result *Int, err error) {
	if err = workspace.check(ctx, left, right); err != nil {
		return nil, err
	}
	var a, b, remainder *Int
	defer func() {
		err = errors.Join(err, a.Close(), b.Close(), remainder.Close())
		if err != nil {
			err = errors.Join(err, result.Close())
			result = nil
		}
	}()
	a, err = workspace.Clone(ctx, left)
	if err != nil {
		return nil, err
	}
	b, err = workspace.Clone(ctx, right)
	if err != nil {
		return nil, err
	}
	a.negative, b.negative = false, false
	for len(b.limbs) != 0 {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		remainder, err = workspace.Rem(ctx, a, b)
		if err != nil {
			return nil, err
		}
		if err = a.Close(); err != nil {
			return nil, err
		}
		a, b, remainder = b, remainder, nil
	}
	result, a = a, nil
	return result, nil
}

// Sqrt returns floor(sqrt(value)); negative values have no integer square root.
func (workspace *Workspace) Sqrt(ctx context.Context, value *Int) (result *Int, err error) {
	if err = workspace.check(ctx, value); err != nil {
		return nil, err
	}
	if value.negative {
		return nil, ErrNegativeSqrt
	}
	if len(value.limbs) == 0 {
		return workspace.Clone(ctx, value)
	}
	var current, two, quotient, sum, next *Int
	defer func() {
		err = errors.Join(err, current.Close(), two.Close(), quotient.Close(), sum.Close(), next.Close())
		if err != nil {
			err = errors.Join(err, result.Close())
			result = nil
		}
	}()
	// base^ceil(limbCount/2) is strictly above the root. Integer Newton
	// steps descend to the floor; stopping on a non-decrease avoids the
	// floor/ceil two-cycle for values one below a perfect square.
	exponent := len(value.limbs)/2 + len(value.limbs)%2
	current, err = workspace.newInt(ctx, exponent+1)
	if err != nil {
		return nil, err
	}
	current.limbs[exponent] = 1
	two, err = workspace.FromInt64(ctx, 2)
	if err != nil {
		return nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		quotient, err = workspace.Quo(ctx, value, current)
		if err != nil {
			return nil, err
		}
		sum, err = workspace.Add(ctx, current, quotient)
		if err != nil {
			return nil, err
		}
		if err = quotient.Close(); err != nil {
			return nil, err
		}
		quotient = nil
		next, err = workspace.Quo(ctx, sum, two)
		if err != nil {
			return nil, err
		}
		if err = sum.Close(); err != nil {
			return nil, err
		}
		sum = nil
		var order int
		order, err = compareMagnitude(ctx, next, current)
		if err != nil {
			return nil, err
		}
		if order >= 0 {
			result, current = current, nil
			return result, nil
		}
		if err = current.Close(); err != nil {
			return nil, err
		}
		current, next = next, nil
	}
}
