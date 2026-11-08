package builtin

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Money uses the embedded C/POSIX monetary profile: two fractional digits,
// dollar currency symbol, comma grouping, and a leading minus sign.
func compileMoney(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 377, 846, 847, 848, 862, 863, 864, 865, 866, 867,
		886, 887, 888, 889, 890, 891, 892, 893, 894, 895, 896, 897,
		898, 899, 919, 935, 2493, 3344, 3345, 3399, 3811, 3812, 3822, 3823, 3824:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		a := args[0].IntVal
		switch signature.ID {
		case 886:
			value, err := parseMoney(ctx, args[0].StrVal)
			return model.Int64Val(value), err
		case 887:
			var buffer [32]byte
			return moneyTextResult(ctx, formatMoney(buffer[:0], a), budget)
		case 935:
			// Six groups of at most 999 plus cents fit in 512 ASCII bytes.
			var buffer [512]byte
			return moneyTextResult(ctx, moneyWords(buffer[:0], a), budget)
		case 2493:
			var buffer [8]byte
			binary.BigEndian.PutUint64(buffer[:], uint64(a))
			if err := moneyOutputBudget(8, budget); err != nil {
				return model.Value{}, err
			}
			return ownedBytes(ctx, budget.OutputBytes, buffer[:])
		case 3823, 3824:
			return moneyNumeric(ctx, args[0], signature.ID == 3823, budget)
		case 3811, 3812:
			value, err := moneyMultiply(a, 100)
			if err != nil {
				return model.Value{}, integerOverflow(20)
			}
			return model.Int64Val(value), nil
		}
		b := args[1].IntVal
		switch signature.ID {
		case 377:
			return model.Int64Val(int64(cmp.Compare(a, b))), nil
		case 888:
			return model.BoolVal(a == b), nil
		case 889:
			return model.BoolVal(a != b), nil
		case 890:
			return model.BoolVal(a < b), nil
		case 891:
			return model.BoolVal(a <= b), nil
		case 892:
			return model.BoolVal(a > b), nil
		case 893:
			return model.BoolVal(a >= b), nil
		case 898:
			return model.Int64Val(max(a, b)), nil
		case 899:
			return model.Int64Val(min(a, b)), nil
		case 894:
			value := a + b
			positiveOverflow := b > 0 && value < a
			negativeOverflow := b < 0 && value > a
			if positiveOverflow || negativeOverflow {
				return model.Value{}, moneyOverflow()
			}
			return model.Int64Val(value), nil
		case 895:
			value := a - b
			positiveOverflow := b < 0 && value < a
			negativeOverflow := b > 0 && value > a
			if positiveOverflow || negativeOverflow {
				return model.Value{}, moneyOverflow()
			}
			return model.Int64Val(value), nil
		case 862, 863, 864, 866, 3344, 3399:
			value, err := moneyMultiply(a, b)
			return model.Int64Val(value), err
		case 865, 867, 3345:
			if b == 0 {
				return model.Value{}, sqlError("22012", "division by zero")
			}
			if a == math.MinInt64 && b == -1 {
				return model.Value{}, moneyOverflow()
			}
			return model.Int64Val(a / b), nil
		case 3822:
			if b == 0 {
				return model.Value{}, sqlError("22012", "division by zero")
			}
			return model.Float64Val(float64(a) / float64(b)), nil
		default:
			factor := args[1].FloatVal
			if signature.ID == 848 || signature.ID == 919 {
				a, factor = b, args[0].FloatVal
			}
			if signature.ID == 846 || signature.ID == 847 || signature.ID == 848 {
				factor = float64(float32(factor))
			}
			op := opMul
			if signature.ID == 847 || signature.ID == 897 {
				op = opDiv
			}
			value, err := floatArithmetic(float64(a), factor, 701, op)
			if err != nil {
				return model.Value{}, err
			}
			rounded := math.RoundToEven(value.FloatVal)
			if math.IsNaN(rounded) || rounded < -0x1p63 || rounded >= 0x1p63 {
				return model.Value{}, moneyOverflow()
			}
			return model.Int64Val(int64(rounded)), nil
		}
	}
}

func moneyOverflow() error { return sqlError("22003", "money out of range") }

func moneyMultiply(a, b int64) (int64, error) {
	if a == math.MinInt64 && b == -1 {
		return 0, moneyOverflow()
	}
	value := a * b
	if b != 0 && value/b != a {
		return 0, moneyOverflow()
	}
	return value, nil
}

func moneyOutputBudget(size int64, budget CallBudget) error {
	if err := outputBudget(size, budget.OutputBytes); err != nil {
		return err
	}
	if uint64(size) > budget.MemoryBytes || (budget.Allocator != nil && uint64(size) > budget.Allocator.AvailableBytes()) {
		return sqlError("54000", "builtin invocation memory budget exceeded")
	}
	return nil
}

func moneyTextResult(ctx context.Context, text []byte, budget CallBudget) (model.Value, error) {
	if err := moneyOutputBudget(int64(len(text)), budget); err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(string(text)), nil
}

func moneyMagnitude(value int64) uint64 {
	if value < 0 {
		return uint64(-(value + 1)) + 1
	}
	return uint64(value)
}

func formatMoney(output []byte, value int64) []byte {
	if value < 0 {
		output = append(output, '-')
	}
	output = append(output, '$')
	magnitude := moneyMagnitude(value)
	var digits [20]byte
	integer := strconv.AppendUint(digits[:0], magnitude/100, 10)
	for i, digit := range integer {
		if i > 0 && (len(integer)-i)%3 == 0 {
			output = append(output, ',')
		}
		output = append(output, digit)
	}
	return append(output, '.', byte(magnitude%100/10)+'0', byte(magnitude%10)+'0')
}

func parseMoney(ctx context.Context, input string) (int64, error) {
	if _, err := textCharacters(ctx, input); err != nil {
		return 0, err
	}
	position := 0
	skipSpace := func() error {
		for position < len(input) && isTSSpace(input[position]) {
			if position%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			position++
		}
		return nil
	}
	if err := skipSpace(); err != nil {
		return 0, err
	}
	if position < len(input) && input[position] == '$' {
		position++
	}
	if err := skipSpace(); err != nil {
		return 0, err
	}
	negative := false
	if position < len(input) {
		switch input[position] {
		case '-', '(':
			negative = true
			position++
		case '+':
			position++
		}
	}
	if err := skipSpace(); err != nil {
		return 0, err
	}
	if position < len(input) && input[position] == '$' {
		position++
	}
	if err := skipSpace(); err != nil {
		return 0, err
	}
	// cash_in deliberately accepts empty digit sequences and loose grouping,
	// signs and parentheses. Accumulating negative permits INT64_MIN.
	value, decimals := int64(0), 0
	seenDot := false
	for position < len(input) {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		c := input[position]
		switch {
		case c >= '0' && c <= '9' && (!seenDot || decimals < 2):
			digit := int64(c - '0')
			if value < (math.MinInt64+digit)/10 {
				return 0, moneyOverflow()
			}
			value = value*10 - digit
			if seenDot {
				decimals++
			}
		case c == '.' && !seenDot:
			seenDot = true
		case c == ',':
		default:
			goto rounded
		}
		position++
	}
rounded:
	if position < len(input) && input[position] >= '5' && input[position] <= '9' {
		if value == math.MinInt64 {
			return 0, moneyOverflow()
		}
		value--
	}
	for ; decimals < 2; decimals++ {
		if value < math.MinInt64/10 {
			return 0, moneyOverflow()
		}
		value *= 10
	}
	for position < len(input) && input[position] >= '0' && input[position] <= '9' {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		position++
	}
	for ; position < len(input); position++ {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		c := input[position]
		switch {
		case c == '-':
			negative = true
		case c == '+' || c == '$' || c == ')' || isTSSpace(c):
		default:
			return 0, sqlError("22P02", "invalid input syntax for type money")
		}
	}
	if negative {
		return value, nil
	}
	if value == math.MinInt64 {
		return 0, moneyOverflow()
	}
	return -value, nil
}

func moneyNumeric(ctx context.Context, input model.Value, toNumeric bool, budget CallBudget) (result model.Value, err error) {
	work, err := budget.NewWorkspace(ctx)
	if err != nil {
		return model.Value{}, numericAllocationError(err)
	}
	defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
	if toNumeric {
		coefficient, err := work.FromInt64(ctx, input.IntVal)
		if err != nil {
			return model.Value{}, err
		}
		return numericValue(ctx, decimal{coefficient: coefficient, scale: 2}, budget.OutputLimit(work))
	}
	value, err := parseNumeric(ctx, work, input.StrVal)
	if err != nil {
		return model.Value{}, err
	}
	if value.special != numericFinite {
		if value.special == numericNaN {
			return model.Value{}, sqlError("0A000", "cannot convert NaN to bigint")
		}
		return model.Value{}, sqlError("0A000", "cannot convert infinity to bigint")
	}
	if value.scale > numericMaxScale {
		return model.Value{}, numericOverflow()
	}
	value, err = numericRescale(ctx, work, value, 2, true)
	if err != nil {
		return model.Value{}, err
	}
	integer, ok := value.coefficient.Int64()
	if !ok {
		return model.Value{}, integerOverflow(20)
	}
	return model.Int64Val(integer), nil
}

var moneySmallWords = [...]string{
	"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
	"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen",
}
var moneyTensWords = [...]string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}

func moneyNumberWords(output []byte, value uint64) []byte {
	if value < 20 {
		return append(output, moneySmallWords[value]...)
	}
	if value >= 100 {
		output = append(output, moneySmallWords[value/100]...)
		output = append(output, " hundred"...)
		value %= 100
		if value == 0 {
			return output
		}
		if value < 20 {
			output = append(output, " and "...)
		} else {
			output = append(output, ' ')
		}
	}
	if value < 20 {
		return append(output, moneySmallWords[value]...)
	}
	output = append(output, moneyTensWords[value/10]...)
	if value%10 != 0 {
		output = append(output, ' ')
		output = append(output, moneySmallWords[value%10]...)
	}
	return output
}

func moneyWords(output []byte, value int64) []byte {
	if value < 0 {
		output = append(output, "minus "...)
	}
	magnitude := moneyMagnitude(value)
	dollars := magnitude / 100
	groups := [...]string{" quadrillion ", " trillion ", " billion ", " million ", " thousand "}
	divisor := uint64(1000000000000000)
	for _, group := range groups {
		if part := dollars / divisor % 1000; part != 0 {
			output = moneyNumberWords(output, part)
			output = append(output, group...)
		}
		divisor /= 1000
	}
	if dollars%1000 != 0 || dollars == 0 {
		output = moneyNumberWords(output, dollars%1000)
	}
	if dollars == 1 {
		output = append(output, " dollar and "...)
	} else {
		output = append(output, " dollars and "...)
	}
	output = moneyNumberWords(output, magnitude%100)
	if magnitude%100 == 1 {
		output = append(output, " cent"...)
	} else {
		output = append(output, " cents"...)
	}
	output[0] -= 'a' - 'A'
	return output
}
