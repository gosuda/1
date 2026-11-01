package decimalint

import (
	"context"
	"errors"
	"math"
)

func digitValue(value byte) int {
	switch {
	case value >= '0' && value <= '9':
		return int(value - '0')
	case value >= 'a' && value <= 'f':
		return int(value-'a') + 10
	case value >= 'A' && value <= 'F':
		return int(value-'A') + 10
	default:
		return -1
	}
}

// Parse accepts an optional sign and digits in base 2, 8, 10 or 16. SQL lexical
// decorations are handled by the SQL codec. Inputs remain borrowed only here.
func (workspace *Workspace) Parse(ctx context.Context, text string, base int) (*Int, error) {
	if err := workspace.check(ctx); err != nil {
		return nil, err
	}
	if base != 2 && base != 8 && base != 10 && base != 16 {
		return nil, ErrInvalid
	}
	negative := false
	if len(text) > 0 && (text[0] == '-' || text[0] == '+') {
		negative = text[0] == '-'
		text = text[1:]
	}
	if len(text) == 0 {
		return nil, ErrInvalid
	}
	first := len(text)
	for i := range len(text) {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		digit := digitValue(text[i])
		if digit < 0 || digit >= base {
			return nil, ErrInvalid
		}
		if digit != 0 && first == len(text) {
			first = i
		}
	}
	text = text[first:]
	if base == 10 {
		return workspace.parseDecimal(ctx, text, negative)
	}
	bitsPerDigit := 1
	if base == 8 {
		bitsPerDigit = 3
	}
	if base == 16 {
		bitsPerDigit = 4
	}
	if len(text) > (math.MaxInt-28)/bitsPerDigit {
		return nil, ErrLimit
	}
	capacity := (len(text)*bitsPerDigit + 28) / 29
	value, err := workspace.newInt(ctx, capacity)
	if err != nil {
		return nil, err
	}
	used := 0
	for position := range len(text) {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, value.Close())
		}
		carry := uint64(digitValue(text[position]))
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

func (workspace *Workspace) parseDecimal(ctx context.Context, text string, negative bool) (*Int, error) {
	count := len(text) / limbDigits
	if len(text)%limbDigits != 0 {
		count++
	}
	value, err := workspace.newInt(ctx, count)
	if err != nil {
		return nil, err
	}
	end := len(text)
	for i := range count {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, errors.Join(err, value.Close())
			}
		}
		start := max(0, end-limbDigits)
		var limb uint32
		for j := start; j < end; j++ {
			limb = limb*10 + uint32(text[j]-'0')
		}
		value.limbs[i] = limb
		end = start
	}
	value.negative = negative && count != 0
	return value, nil
}

// AppendDecimal never allocates or grows dst. The caller owns output admission;
// on failure, the original prefix is unchanged and no extended slice is returned.
func (value *Int) AppendDecimal(ctx context.Context, dst []byte) ([]byte, error) {
	if value == nil {
		return dst, ErrInvalid
	}
	if err := value.owner.check(ctx, value); err != nil {
		return dst, err
	}
	required := value.Digits()
	if value.negative {
		required++
	}
	if required > cap(dst)-len(dst) {
		return dst, ErrLimit
	}
	original := dst
	start := len(dst)
	dst = dst[:start+required]
	if value.negative {
		dst[start] = '-'
		start++
	}
	if len(value.limbs) == 0 {
		dst[start] = '0'
		return dst, nil
	}
	for i := len(value.limbs) - 1; i >= 0; i-- {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return original, err
			}
		}
		width := limbDigits
		if i == len(value.limbs)-1 {
			width = value.Digits() - (len(value.limbs)-1)*limbDigits
		}
		remaining := value.limbs[i]
		for offset := width - 1; offset >= 0; offset-- {
			dst[start+offset] = '0' + byte(remaining%10)
			remaining /= 10
		}
		start += width
	}
	return dst, nil
}

func (value *Int) Uint64() (uint64, bool) {
	value.requireLive()
	if value.negative {
		return 0, false
	}
	return value.magnitudeUint64(math.MaxUint64)
}
func (value *Int) Int64() (int64, bool) {
	value.requireLive()
	limit := uint64(math.MaxInt64)
	if value.negative {
		limit++
	}
	magnitude, ok := value.magnitudeUint64(limit)
	if !ok {
		return 0, false
	}
	if value.negative {
		if magnitude == uint64(math.MaxInt64)+1 {
			return math.MinInt64, true
		}
		return -int64(magnitude), true
	}
	return int64(magnitude), true
}
func (value *Int) magnitudeUint64(limit uint64) (uint64, bool) {
	var result uint64
	for i := len(value.limbs) - 1; i >= 0; i-- {
		limb := uint64(value.limbs[i])
		if result > (limit-limb)/limbBase {
			return 0, false
		}
		result = result*limbBase + limb
	}
	return result, true
}
