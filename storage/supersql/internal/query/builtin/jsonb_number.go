package builtin

import (
	"context"
	"strings"
)

// JSON numbers need decimal-point movement, not arithmetic. Borrowing the digit
// spans avoids expanding PostgreSQL's 131072-digit range into a big.Int.
type jsonbNumber struct {
	integer, fraction  string
	first, last, point int
	scale              int
	negative           bool
	zero               bool
}

func inspectJSONBNumber(ctx context.Context, text string) (jsonbNumber, error) {
	number := jsonbNumber{negative: text[0] == '-'}
	if number.negative {
		text = text[1:]
	}
	point, exponentStart := -1, len(text)
	first := -1
	digits := 0
	for i := 0; i < len(text); i++ {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return jsonbNumber{}, err
			}
		}
		switch text[i] {
		case '.':
			point = i
		case 'e', 'E':
			exponentStart = i
			i = len(text)
		default:
			if first < 0 && text[i] != '0' {
				first = digits
			}
			if text[i] != '0' {
				number.last = digits
			}
			digits++
		}
	}
	number.integer = text[:exponentStart]
	if point >= 0 {
		number.integer = text[:point]
		number.fraction = text[point+1 : exponentStart]
	}
	exponent := 0
	if exponentStart < len(text) {
		index := exponentStart + 1
		negative := false
		if text[index] == '+' || text[index] == '-' {
			negative = text[index] == '-'
			index++
		}
		for ; index < len(text); index++ {
			if index%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return jsonbNumber{}, err
				}
			}
			exponent = exponent*10 + int(text[index]-'0')
			// numeric.c:set_var_from_str bounds the exponent before zero
			// normalization, so even 0e1073741824 overflows.
			if exponent > 1073741823 {
				return jsonbNumber{}, numericOverflow()
			}
		}
		if negative {
			exponent = -exponent
		}
	}
	number.first = first
	number.point = len(number.integer) + exponent
	number.scale = max(len(number.fraction)-exponent, 0)
	number.zero = first < 0
	if number.scale > numericMaxScale || !number.zero && number.point-first > numericMaxIntegerDigits {
		return jsonbNumber{}, numericOverflow()
	}
	if number.zero {
		number.negative = false
	}
	return number, ctx.Err()
}

func (number jsonbNumber) bytes() int64 {
	integer := 1
	if !number.zero {
		integer = max(number.point-number.first, 1)
	}
	size := int64(integer + number.scale)
	if number.scale != 0 {
		size++
	}
	if number.negative {
		size++
	}
	return size
}

func (number jsonbNumber) storageBytes() int64 {
	weight, groups := 0, 0
	if !number.zero {
		weight = (number.point - number.first - 1) >> 2
		lastWeight := (number.point - number.last - 1) >> 2
		groups = weight - lastWeight + 1
	}
	header := 8
	if number.scale <= 63 && weight >= -64 && weight <= 63 {
		header = 6
	}
	return int64(header + 2*groups)
}

func (number jsonbNumber) append(ctx context.Context, output *strings.Builder) error {
	if number.negative {
		output.WriteByte('-')
	}
	if number.zero || number.point <= number.first {
		output.WriteByte('0')
	} else if err := number.appendDigits(ctx, output, number.first, number.point); err != nil {
		return err
	}
	if number.scale != 0 {
		output.WriteByte('.')
		if number.zero {
			return appendJSONBZeros(ctx, output, number.scale)
		}
		return number.appendDigits(ctx, output, number.point, number.point+number.scale)
	}
	return ctx.Err()
}

func (number jsonbNumber) appendDigits(ctx context.Context, output *strings.Builder, start, end int) error {
	if start < 0 {
		if err := appendJSONBZeros(ctx, output, min(-start, end-start)); err != nil {
			return err
		}
		start = 0
	}
	integerEnd := min(end, len(number.integer))
	if start < integerEnd {
		if err := copyArrayText(ctx, output, number.integer[start:integerEnd], false); err != nil {
			return err
		}
		start = integerEnd
	}
	fractionEnd := min(end, len(number.integer)+len(number.fraction))
	if start < fractionEnd {
		if err := copyArrayText(ctx, output, number.fraction[start-len(number.integer):fractionEnd-len(number.integer)], false); err != nil {
			return err
		}
		start = fractionEnd
	}
	return appendJSONBZeros(ctx, output, end-start)
}

func appendJSONBZeros(ctx context.Context, output *strings.Builder, count int) error {
	for i := range count {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		output.WriteByte('0')
	}
	return ctx.Err()
}
