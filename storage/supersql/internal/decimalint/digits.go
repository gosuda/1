package decimalint

import (
	"context"
	"errors"
)

// DecimalDigit addresses the unsigned decimal spelling from its most significant
// digit. Positions beyond its end read as zero for scale-aligned comparisons.
func (value *Int) DecimalDigit(index int) uint8 {
	value.requireLive()
	if index < 0 {
		panic(ErrInvalid)
	}
	digits := value.Digits()
	if index >= digits || len(value.limbs) == 0 {
		return 0
	}
	fromRight := digits - index - 1
	limb := value.limbs[fromRight/limbDigits]
	for range fromRight % limbDigits {
		limb /= 10
	}
	return uint8(limb % 10)
}

// ParseDigits consumes exactly count decimal digits, most significant first.
// next is borrowed for this call and may skip SQL lexical separators itself.
func (workspace *Workspace) ParseDigits(ctx context.Context, count int, negative bool, next func() (uint8, error)) (*Int, error) {
	if next == nil || count < 0 {
		return nil, ErrInvalid
	}
	capacity := count / limbDigits
	if count%limbDigits != 0 {
		capacity++
	}
	value, err := workspace.newInt(ctx, capacity)
	if err != nil {
		return nil, err
	}
	for index := range count {
		if index&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, errors.Join(err, value.Close())
			}
		}
		digit, err := next()
		if err != nil {
			return nil, errors.Join(err, value.Close())
		}
		if digit > 9 {
			return nil, errors.Join(ErrInvalid, value.Close())
		}
		limb := (count - index - 1) / limbDigits
		value.limbs[limb] = value.limbs[limb]*10 + uint32(digit)
	}
	value.negative = negative
	value.normalize()
	return value, nil
}
