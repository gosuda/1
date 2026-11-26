package decimalint

import (
	"context"
	"errors"
	"math/bits"
)

func (workspace *Workspace) Add(ctx context.Context, left, right *Int) (*Int, error) {
	if err := workspace.check(ctx, left, right); err != nil {
		return nil, err
	}
	return workspace.addSigned(ctx, left, right, right.negative)
}

func (workspace *Workspace) Sub(ctx context.Context, left, right *Int) (*Int, error) {
	if err := workspace.check(ctx, left, right); err != nil {
		return nil, err
	}
	return workspace.addSigned(ctx, left, right, !right.negative)
}

func (workspace *Workspace) addSigned(ctx context.Context, left, right *Int, rightNegative bool) (*Int, error) {
	if left.negative == rightNegative {
		count := max(len(left.limbs), len(right.limbs))
		if count == int(^uint(0)>>1) {
			return nil, ErrLimit
		}
		result, err := workspace.newInt(ctx, count+1)
		if err != nil {
			return nil, err
		}
		var carry uint64
		for i := range count {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, errors.Join(err, result.Close())
				}
			}
			sum := carry
			if i < len(left.limbs) {
				sum += uint64(left.limbs[i])
			}
			if i < len(right.limbs) {
				sum += uint64(right.limbs[i])
			}
			result.limbs[i] = uint32(sum % limbBase)
			carry = sum / limbBase
		}
		result.limbs[count] = uint32(carry)
		result.negative = left.negative
		result.normalize()
		return result, nil
	}

	order, err := compareMagnitude(ctx, left, right)
	if err != nil {
		return nil, err
	}
	if order == 0 {
		return workspace.newInt(ctx, 0)
	}
	larger, smaller, negative := left, right, left.negative
	if order < 0 {
		larger, smaller, negative = right, left, rightNegative
	}
	result, err := workspace.newInt(ctx, len(larger.limbs))
	if err != nil {
		return nil, err
	}
	var borrow uint64
	for i, limb := range larger.limbs {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, errors.Join(err, result.Close())
			}
		}
		subtrahend := borrow
		if i < len(smaller.limbs) {
			subtrahend += uint64(smaller.limbs[i])
		}
		if uint64(limb) < subtrahend {
			result.limbs[i] = uint32(uint64(limb) + limbBase - subtrahend)
			borrow = 1
		} else {
			result.limbs[i] = uint32(uint64(limb) - subtrahend)
			borrow = 0
		}
	}
	result.negative = negative
	result.normalize()
	return result, nil
}

func (workspace *Workspace) Mul(ctx context.Context, left, right *Int) (*Int, error) {
	if err := workspace.check(ctx, left, right); err != nil {
		return nil, err
	}
	if len(left.limbs) == 0 || len(right.limbs) == 0 {
		return workspace.newInt(ctx, 0)
	}
	if len(left.limbs) > int(^uint(0)>>1)-len(right.limbs) {
		return nil, ErrLimit
	}
	result, err := workspace.newInt(ctx, len(left.limbs)+len(right.limbs))
	if err != nil {
		return nil, err
	}
	for i, leftLimb := range left.limbs {
		var carry uint64
		for j, rightLimb := range right.limbs {
			if j&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, errors.Join(err, result.Close())
				}
			}
			// Each sum is at most limbBase squared minus one, below uint64's limit.
			product := uint64(leftLimb)*uint64(rightLimb) + uint64(result.limbs[i+j]) + carry
			result.limbs[i+j] = uint32(product % limbBase)
			carry = product / limbBase
		}
		result.limbs[i+len(right.limbs)] = uint32(carry)
	}
	result.negative = left.negative != right.negative
	result.normalize()
	return result, nil
}

func (workspace *Workspace) Neg(ctx context.Context, value *Int) (*Int, error) {
	result, err := workspace.Clone(ctx, value)
	if err != nil {
		return nil, err
	}
	result.negative = len(result.limbs) != 0 && !result.negative
	return result, nil
}

func (workspace *Workspace) Abs(ctx context.Context, value *Int) (*Int, error) {
	result, err := workspace.Clone(ctx, value)
	if err != nil {
		return nil, err
	}
	result.negative = false
	return result, nil
}

// Pow defines zero to the zeroth power as one.
func (workspace *Workspace) Pow(ctx context.Context, value *Int, exponent uint64) (*Int, error) {
	if err := workspace.check(ctx, value); err != nil {
		return nil, err
	}
	if exponent == 0 {
		return workspace.FromInt64(ctx, 1)
	}
	result, err := workspace.Clone(ctx, value)
	if err != nil {
		return nil, err
	}
	if len(value.limbs) == 0 || (len(value.limbs) == 1 && value.limbs[0] == 1) {
		result.negative = value.negative && exponent&1 != 0
		return result, nil
	}
	// Scan from the high bit to retain only one accumulator and its replacement.
	for bit := bits.Len64(exponent) - 2; bit >= 0; bit-- {
		next, err := workspace.Mul(ctx, result, result)
		if err != nil {
			return nil, errors.Join(err, result.Close())
		}
		if err := result.Close(); err != nil {
			return nil, errors.Join(err, next.Close())
		}
		result = next
		if exponent&(uint64(1)<<bit) == 0 {
			continue
		}
		next, err = workspace.Mul(ctx, result, value)
		if err != nil {
			return nil, errors.Join(err, result.Close())
		}
		if err := result.Close(); err != nil {
			return nil, errors.Join(err, next.Close())
		}
		result = next
	}
	return result, nil
}
