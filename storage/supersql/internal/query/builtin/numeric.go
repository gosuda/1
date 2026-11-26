package builtin

import (
	"context"
	"errors"

	"strings"

	"github.com/gosuda/supersql-alpha/internal/decimalint"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Decimal work bounds are independent of the caller's output quota.
const numericWorkDigits = 32768
const numericInputBytes = 2 * numericWorkDigits
const numericMaxScale = 16383
const numericMaxIntegerDigits = 131072

type numericSpecial int8

const (
	numericFinite numericSpecial = iota
	numericNegativeInfinity
	numericPositiveInfinity
	numericNaN
)

// A decimal borrows its coefficient from the caller-owned workspace.
type decimal struct {
	coefficient *decimalint.Int
	scale       int
	special     numericSpecial
}

func numericResourceError() error { return sqlError("54000", "numeric work limit exceeded") }
func numericOverflow() error      { return sqlError("22003", "value overflows numeric format") }

func numericWork(ctx context.Context, digits int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if digits < 0 || digits > numericWorkDigits {
		return numericResourceError()
	}
	return nil
}

func numericPower(ctx context.Context, work *decimalint.Workspace, exponent int) (*decimalint.Int, error) {
	if err := numericWork(ctx, exponent+1); err != nil {
		return nil, err
	}
	index := 0
	return work.ParseDigits(ctx, exponent+1, false, func() (uint8, error) {
		index++
		if index == 1 {
			return 1, nil
		}
		return 0, nil
	})
}

func numericDigits(value *decimalint.Int) int { return value.Digits() }

func parseNumeric(ctx context.Context, work *decimalint.Workspace, input string) (decimal, error) {
	if err := ctx.Err(); err != nil {
		return decimal{}, err
	}
	if len(input) > numericInputBytes {
		return decimal{}, numericResourceError()
	}
	input = trimInput(input)
	if strings.EqualFold(input, "nan") {
		return decimal{special: numericNaN}, nil
	}
	negative := false
	if len(input) > 0 && (input[0] == '+' || input[0] == '-') {
		negative = input[0] == '-'
		input = input[1:]
	}
	if strings.EqualFold(input, "inf") || strings.EqualFold(input, "infinity") {
		kind := numericPositiveInfinity
		if negative {
			kind = numericNegativeInfinity
		}
		return decimal{special: kind}, nil
	}
	if input == "" {
		return decimal{}, invalidInput()
	}
	base := 10
	if len(input) >= 2 && input[0] == '0' {
		switch input[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
	}
	if base != 10 {
		return parseNumericBase(ctx, work, input[2:], base, negative)
	}
	count, significant, first := 0, 0, -1
	scale := 0
	point := false
	seen := false
	i := 0
	for ; i < len(input); i++ {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return decimal{}, err
			}
		}
		c := input[i]
		if c >= '0' && c <= '9' {
			if count == numericWorkDigits {
				return decimal{}, numericResourceError()
			}
			count++
			if c != '0' || first >= 0 {
				if first < 0 {
					first = i
				}
				significant++
			}
			seen = true
			if point {
				scale++
			}
			continue
		}
		if c == '.' && !point {
			point = true
			if i+1 < len(input) && input[i+1] == '_' {
				return decimal{}, invalidInput()
			}
			continue
		}
		if c == '_' && i > 0 && i+1 < len(input) {
			previous, next := digit(input[i-1]), digit(input[i+1])
			if previous >= 0 && previous < 10 && next >= 0 && next < 10 {
				continue
			}
		}
		break
	}
	if !seen {
		return decimal{}, invalidInput()
	}
	exponent := 0
	if i < len(input) && (input[i] == 'e' || input[i] == 'E') {
		i++
		exponentNegative := false
		if i < len(input) && (input[i] == '+' || input[i] == '-') {
			exponentNegative = input[i] == '-'
			i++
		}
		start := i
		for ; i < len(input); i++ {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return decimal{}, err
				}
			}
			c := input[i]
			if c == '_' && i > start && i+1 < len(input) && input[i+1] >= '0' && input[i+1] <= '9' {
				continue
			}
			if c < '0' || c > '9' {
				return decimal{}, invalidInput()
			}
			exponent = exponent*10 + int(c-'0')
			if exponent > 1073741823 {
				return decimal{}, numericOverflow()
			}
		}
		if i == start {
			return decimal{}, invalidInput()
		}
		if exponentNegative {
			exponent = -exponent
		}
	}
	if i != len(input) {
		return decimal{}, invalidInput()
	}
	scale -= exponent
	if significant != 0 && significant-scale > numericMaxIntegerDigits {
		return decimal{}, numericOverflow()
	}
	if err := numericWork(ctx, max(significant, scale)); err != nil {
		return decimal{}, err
	}
	if significant == 0 {
		coefficient, err := work.FromInt64(ctx, 0)
		return decimal{coefficient: coefficient, scale: max(scale, 0)}, err
	}
	if err := numericWork(ctx, significant+max(-scale, 0)); err != nil {
		return decimal{}, err
	}
	cursor, remaining := first, significant
	coefficient, err := work.ParseDigits(ctx, significant+max(-scale, 0), negative, func() (uint8, error) {
		if remaining == 0 {
			return 0, nil
		}
		for input[cursor] == '_' || input[cursor] == '.' {
			cursor++
		}
		d := input[cursor] - '0'
		cursor++
		remaining--
		return d, nil
	})
	return decimal{coefficient: coefficient, scale: max(scale, 0)}, err
}

func parseNumericBase(ctx context.Context, work *decimalint.Workspace, input string, base int, negative bool) (decimal, error) {
	if err := numericWork(ctx, len(input)*2); err != nil {
		return decimal{}, err
	}
	count := 0
	for i := range len(input) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return decimal{}, err
			}
		}
		if input[i] == '_' && i+1 < len(input) && digit(input[i+1]) >= 0 && digit(input[i+1]) < base {
			continue
		}
		d := digit(input[i])
		if d < 0 || d >= base {
			return decimal{}, invalidInput()
		}
		count++
	}
	if count == 0 {
		return decimal{}, invalidInput()
	}
	cursor := 0
	coefficient, err := work.ParseBaseDigits(ctx, count, base, negative, func() (uint8, error) {
		for input[cursor] == '_' {
			cursor++
		}
		d := uint8(digit(input[cursor]))
		cursor++
		return d, nil
	})
	return decimal{coefficient: coefficient}, err
}

func numericTextSize(value decimal) (int64, error) {
	switch value.special {
	case numericNaN:
		return 3, nil
	case numericPositiveInfinity:
		return 8, nil
	case numericNegativeInfinity:
		return 9, nil
	}
	if value.scale > numericMaxScale {
		return 0, numericOverflow()
	}
	digits := numericDigits(value.coefficient)
	if value.coefficient.Sign() != 0 && digits-value.scale > numericMaxIntegerDigits {
		return 0, numericOverflow()
	}
	size := max(digits, value.scale+1)
	if value.scale > 0 {
		size++
	}
	if value.coefficient.Sign() < 0 {
		size++
	}
	return int64(size), nil
}

func numericText(ctx context.Context, value decimal, budget int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if value.special != numericFinite {
		text := "NaN"
		if value.special == numericPositiveInfinity {
			text = "Infinity"
		}
		if value.special == numericNegativeInfinity {
			text = "-Infinity"
		}
		if err := outputBudget(int64(len(text)), budget); err != nil {
			return "", err
		}
		return strings.Clone(text), nil
	}
	size, err := numericTextSize(value)
	if err != nil {
		return "", err
	}
	digits := numericDigits(value.coefficient)
	if err := outputBudget(int64(size), budget); err != nil {
		return "", err
	}
	var result strings.Builder
	result.Grow(int(size))
	if value.coefficient.Sign() < 0 {
		result.WriteByte('-')
	}
	integerDigits := digits - value.scale
	if integerDigits <= 0 {
		result.WriteString("0.")
	}
	for i := min(integerDigits, 0); i < digits; i++ {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		if i == integerDigits && i > 0 {
			result.WriteByte('.')
		}
		d := uint8(0)
		if i >= 0 {
			d = value.coefficient.DecimalDigit(i)
		}
		result.WriteByte('0' + d)
	}
	return result.String(), nil
}

func numericValue(ctx context.Context, value decimal, budget int64) (model.Value, error) {
	text, err := numericText(ctx, value, budget)
	if err != nil {
		return model.Value{}, err
	}
	return model.Value{Type: model.TypeNumeric, StrVal: text}, nil
}

func numericRescale(ctx context.Context, work *decimalint.Workspace, value decimal, scale int, round bool) (result decimal, err error) {
	if value.special != numericFinite {
		return value, nil
	}
	var coefficient, power, remainder *decimalint.Int
	defer func() { err = errors.Join(err, coefficient.Close(), power.Close(), remainder.Close()) }()
	if scale >= value.scale {
		if value.coefficient.Sign() == 0 {
			coefficient, err = work.FromInt64(ctx, 0)
		} else {
			if err = numericWork(ctx, numericDigits(value.coefficient)+scale-value.scale); err != nil {
				return decimal{}, err
			}
			power, err = numericPower(ctx, work, scale-value.scale)
			if err != nil {
				return decimal{}, err
			}
			coefficient, err = work.Mul(ctx, value.coefficient, power)
		}
	} else {
		drop := value.scale - scale
		if drop > numericDigits(value.coefficient) {
			coefficient, err = work.FromInt64(ctx, 0)
			if err != nil {
				return decimal{}, err
			}
			result = decimal{coefficient: coefficient, scale: max(scale, 0)}
			coefficient = nil
			return result, nil
		}
		power, err = numericPower(ctx, work, drop)
		if err != nil {
			return decimal{}, err
		}
		coefficient, remainder, err = work.QuoRem(ctx, value.coefficient, power)
		if err == nil && round {
			var up bool
			up, err = numericRoundHalf(ctx, work, remainder, power)
			if err == nil && up {
				coefficient, err = numericIncrement(ctx, work, coefficient, value.coefficient.Sign())
			}
		}
	}
	if err != nil {
		return decimal{}, err
	}
	if scale < 0 {
		if err = numericWork(ctx, numericDigits(coefficient)-scale); err != nil {
			return decimal{}, err
		}
		if err = power.Close(); err != nil {
			return decimal{}, err
		}
		power, err = numericPower(ctx, work, -scale)
		if err != nil {
			return decimal{}, err
		}
		next, mulErr := work.Mul(ctx, coefficient, power)
		err = errors.Join(mulErr, coefficient.Close())
		coefficient = next
		if err != nil {
			return decimal{}, err
		}
		scale = 0
	}
	result = decimal{coefficient: coefficient, scale: scale}
	coefficient = nil
	return result, ctx.Err()
}

// numericIncrement consumes its owned coefficient, including on failure.
func numericIncrement(ctx context.Context, work *decimalint.Workspace, coefficient *decimalint.Int, sign int) (result *decimalint.Int, err error) {
	var increment *decimalint.Int
	defer func() { err = errors.Join(err, coefficient.Close(), increment.Close()) }()
	increment, err = work.FromInt64(ctx, int64(sign))
	if err != nil {
		return nil, err
	}
	return work.Add(ctx, coefficient, increment)
}

func numericRoundHalf(ctx context.Context, work *decimalint.Workspace, remainder, denominator *decimalint.Int) (up bool, err error) {
	var magnitude, twice, divisor *decimalint.Int
	defer func() { err = errors.Join(err, magnitude.Close(), twice.Close(), divisor.Close()) }()
	magnitude, err = work.Abs(ctx, remainder)
	if err != nil {
		return false, err
	}
	twice, err = work.Add(ctx, magnitude, magnitude)
	if err != nil {
		return false, err
	}
	divisor, err = work.Abs(ctx, denominator)
	if err != nil {
		return false, err
	}
	order, err := twice.Cmp(ctx, divisor)
	return order >= 0, err
}

func numericIntegral(ctx context.Context, work *decimalint.Workspace, value decimal, op primitiveOp) (result decimal, err error) {
	if value.special != numericFinite {
		return value, ctx.Err()
	}
	if value.scale <= 0 {
		return numericRescale(ctx, work, value, 0, false)
	}
	var coefficient, power, remainder *decimalint.Int
	defer func() { err = errors.Join(err, coefficient.Close(), power.Close(), remainder.Close()) }()
	sign := value.coefficient.Sign()
	fractional := sign != 0
	if sign != 0 && value.scale < numericDigits(value.coefficient) {
		power, err = numericPower(ctx, work, value.scale)
		if err != nil {
			return decimal{}, err
		}
		coefficient, remainder, err = work.QuoRem(ctx, value.coefficient, power)
		if err != nil {
			return decimal{}, err
		}
		fractional = remainder.Sign() != 0
	} else {
		coefficient, err = work.FromInt64(ctx, 0)
		if err != nil {
			return decimal{}, err
		}
	}
	roundUp := op == opCeil && sign > 0
	roundDown := op == opFloor && sign < 0
	if fractional && (roundUp || roundDown) {
		coefficient, err = numericIncrement(ctx, work, coefficient, sign)
		if err != nil {
			return decimal{}, err
		}
	}
	result = decimal{coefficient: coefficient}
	coefficient = nil
	return result, ctx.Err()
}

func numericTypmod(ctx context.Context, work *decimalint.Workspace, value decimal, typmod int64) (decimal, error) {
	if typmod < 4 || value.special == numericNaN {
		return value, nil
	}
	if value.special != numericFinite {
		return decimal{}, sqlError("22003", "numeric field overflow")
	}
	encoded := int32(typmod) - 4
	precision := int((uint32(encoded) >> 16) & 65535)
	scale := int((encoded&2047)^1024) - 1024
	result, err := numericRescale(ctx, work, value, scale, true)
	if err != nil {
		return decimal{}, err
	}
	if result.coefficient.Sign() != 0 && numericDigits(result.coefficient)-result.scale > precision-scale {
		return decimal{}, errors.Join(sqlError("22003", "numeric field overflow"), result.coefficient.Close())
	}
	return result, nil
}
