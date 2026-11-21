package builtin

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/decimalint"
)

const (
	numericKeyNegativeInfinity byte = iota
	numericKeyNegative
	numericKeyZero
	numericKeyPositive
	numericKeyPositiveInfinity
	numericKeyNaN
)

// numericKey owns its canonical coefficient text, not the caller's datum.
// Nonzero finite payloads are category | biased int32 exponent | ASCII digits | 00.
// The exponent is the decimal point position before trailing zeros are removed.
// Negative magnitudes complement exponent, digits and terminator; descending
// complements the entire payload. Neither operation touches the tuple NULL marker.
type numericKey struct {
	category byte
	exponent uint32
	digits   string
}

func prepareNumericKey(ctx context.Context, input string, budget uint64) (result numericKey, size uint64, err error) {
	if err := ctx.Err(); err != nil {
		return numericKey{}, 0, err
	}
	// Every non-NULL numeric needs a tuple marker and a category, even zero.
	if budget < 2 {
		return numericKey{}, 0, columnar.ErrBatchBudget
	}
	work, err := decimalint.NewWorkspace(ctx, nil, 4<<20)
	if err != nil {
		return numericKey{}, 0, numericAllocationError(err)
	}
	defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
	value, err := parseNumeric(ctx, work, input)
	if err != nil {
		return numericKey{}, 0, err
	}
	key := numericKey{}
	switch value.special {
	case numericNegativeInfinity:
		key.category = numericKeyNegativeInfinity
	case numericPositiveInfinity:
		key.category = numericKeyPositiveInfinity
	case numericNaN:
		key.category = numericKeyNaN
	default:
		if value.scale > numericMaxScale {
			return numericKey{}, 0, numericOverflow()
		}
		sign := value.coefficient.Sign()
		if sign == 0 {
			key.category = numericKeyZero
			break
		}
		if budget < 8 {
			return numericKey{}, 0, columnar.ErrBatchBudget
		}
		key.category = numericKeyPositive
		if sign < 0 {
			key.category = numericKeyNegative
		}
		digits := value.coefficient.Digits()
		key.exponent = uint32(int32(digits-value.scale)) ^ (uint32(1) << 31)
		end := digits
		for end > 0 && value.coefficient.DecimalDigit(end-1) == 0 {
			if end%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return numericKey{}, 0, err
				}
			}
			end--
		}
		if uint64(end)+7 > budget {
			return numericKey{}, 0, columnar.ErrBatchBudget
		}
		var text strings.Builder
		text.Grow(end)
		for index := range end {
			if index%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return numericKey{}, 0, err
				}
			}
			text.WriteByte('0' + value.coefficient.DecimalDigit(index))
		}
		key.digits = text.String()
	}
	size = uint64(2)
	if key.digits != "" {
		size += 5 + uint64(len(key.digits))
	}
	if size > budget {
		return numericKey{}, 0, columnar.ErrBatchBudget
	}
	return key, size, ctx.Err()
}

func appendNumericKey(ctx context.Context, dst []byte, key numericKey, descending bool) ([]byte, error) {
	mask := byte(0)
	if descending {
		mask = 0xff
	}
	dst = append(dst, key.category^mask)
	if key.digits == "" {
		return dst, ctx.Err()
	}
	if key.category == numericKeyNegative {
		mask ^= 0xff
	}
	exponent := key.exponent
	if mask != 0 {
		exponent = ^exponent
	}
	dst = binary.BigEndian.AppendUint32(dst, exponent)
	for i := range len(key.digits) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		dst = append(dst, key.digits[i]^mask)
	}
	// ASCII digits cannot equal either terminator, so finite fields are prefix-free.
	return append(dst, mask), ctx.Err()
}
