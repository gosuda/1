package decimalint

import (
	"context"
	"errors"
	"math"
)

// ParseBaseDigits consumes validated numeric digits without a sanitized text copy.
func (workspace *Workspace) ParseBaseDigits(ctx context.Context, count, base int, negative bool, next func() (uint8, error)) (*Int, error) {
	if base == 10 {
		return workspace.ParseDigits(ctx, count, negative, next)
	}
	if next == nil || count < 0 {
		return nil, ErrInvalid
	}
	bitsPerDigit := 0
	switch base {
	case 2:
		bitsPerDigit = 1
	case 8:
		bitsPerDigit = 3
	case 16:
		bitsPerDigit = 4
	default:
		return nil, ErrInvalid
	}
	if count > (math.MaxInt-28)/bitsPerDigit {
		return nil, ErrLimit
	}
	value, err := workspace.newInt(ctx, (count*bitsPerDigit+28)/29)
	if err != nil {
		return nil, err
	}
	used := 0
	for range count {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, value.Close())
		}
		digit, err := next()
		if err != nil {
			return nil, errors.Join(err, value.Close())
		}
		if int(digit) >= base {
			return nil, errors.Join(ErrInvalid, value.Close())
		}
		carry := uint64(digit)
		for i := range used {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, errors.Join(err, value.Close())
				}
			}
			product := uint64(value.limbs[i])*uint64(base) + carry
			value.limbs[i] = uint32(product % limbBase)
			carry = product / limbBase
		}
		if carry != 0 {
			value.limbs[used] = uint32(carry)
			used++
		}
	}
	value.limbs = value.limbs[:used]
	value.negative = negative && used != 0
	return value, nil
}
