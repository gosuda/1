package builtin

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileInput(signature Signature) scalarKernel {
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		input := args[0].StrVal
		if _, err := textCharacters(ctx, input); err != nil {
			return model.Value{}, err
		}
		switch signature.Result {
		case 20, 21, 23:
			return parseInteger(ctx, input, signature.Result)
		case 700, 701:
			return parseFloat(input, signature.Result)
		case 16:
			return parseBoolean(input)
		case 17:
			return parseBytea(ctx, input, budget)
		case 25:
			return ownedText(ctx, budget, input)
		default:
			return model.Value{}, sqlError("XX000", "invalid input kernel type")
		}
	}
}

func trimInput(input string) string { return strings.Trim(input, " \t\n\r\v\f") }
func invalidInput() error           { return sqlError("22P02", "invalid input syntax for bound type") }
func digit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return -1
	}
}

func parseInteger(ctx context.Context, input string, id model.TypeID) (model.Value, error) {
	input = trimInput(input)
	if input == "" {
		return model.Value{}, invalidInput()
	}
	negative := false
	if input[0] == '-' || input[0] == '+' {
		negative = input[0] == '-'
		input = input[1:]
	}
	base := 10
	prefixed := false
	if len(input) >= 2 && input[0] == '0' {
		switch input[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 10 {
			input = input[2:]
			prefixed = true
		}
	}
	_, high := integerLimits(id)
	limit := uint64(high)
	if negative {
		limit++
	}
	var value uint64
	seen := false
	for i := range len(input) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		if input[i] == '_' {
			prefixSeparator := prefixed && i == 0
			if !seen && !prefixSeparator {
				return model.Value{}, invalidInput()
			}
			if i+1 == len(input) {
				return model.Value{}, invalidInput()
			}
			next := digit(input[i+1])
			if next < 0 || next >= base {
				return model.Value{}, invalidInput()
			}
			continue
		}
		d := digit(input[i])
		if d < 0 || d >= base {
			return model.Value{}, invalidInput()
		}
		seen = true
		if value > (limit-uint64(d))/uint64(base) {
			return model.Value{}, integerOverflow(id)
		}
		value = value*uint64(base) + uint64(d)
	}
	if !seen {
		return model.Value{}, invalidInput()
	}
	if negative {
		return model.Int64Val(-int64(value)), nil
	}
	return model.Int64Val(int64(value)), nil
}

func parseBoolean(input string) (model.Value, error) {
	input = trimInput(input)
	if input == "1" {
		return model.BoolVal(true), nil
	}
	if input == "0" {
		return model.BoolVal(false), nil
	}
	if input == "" {
		return model.Value{}, invalidInput()
	}
	for _, candidate := range []struct {
		word  string
		value bool
	}{{"true", true}, {"false", false}, {"yes", true}, {"no", false}, {"on", true}, {"off", false}} {
		ambiguousPrefix := len(input) == 1 && (candidate.word == "on" || candidate.word == "off")
		if len(input) > len(candidate.word) || ambiguousPrefix {
			continue
		}
		if strings.EqualFold(input, candidate.word[:len(input)]) {
			return model.BoolVal(candidate.value), nil
		}
	}
	return model.Value{}, invalidInput()
}

func parseFloat(input string, id model.TypeID) (model.Value, error) {
	input = trimInput(input)
	switch {
	case strings.EqualFold(input, "nan"), strings.EqualFold(input, "+nan"), strings.EqualFold(input, "-nan"):
		return model.Float64Val(math.NaN()), nil
	case strings.EqualFold(input, "infinity"), strings.EqualFold(input, "+infinity"), strings.EqualFold(input, "inf"), strings.EqualFold(input, "+inf"):
		return model.Float64Val(math.Inf(1)), nil
	case strings.EqualFold(input, "-infinity"), strings.EqualFold(input, "-inf"):
		return model.Float64Val(math.Inf(-1)), nil
	}
	// Go's digit separators are not part of strtof/strtod's input language.
	if strings.ContainsRune(input, '_') {
		return model.Value{}, invalidInput()
	}
	unsigned := strings.TrimLeft(input, "+-")
	hexInput := strings.HasPrefix(unsigned, "0x") || strings.HasPrefix(unsigned, "0X")
	if hexInput && !strings.ContainsAny(input, "pP") {
		input += "p0"
	}
	bits := 64
	if id == 700 {
		bits = 32
	}
	value, err := strconv.ParseFloat(input, bits)
	if err != nil {
		if number, ok := err.(*strconv.NumError); ok && number.Err == strconv.ErrRange {
			return model.Value{}, sqlError("22003", "floating-point input out of range")
		}
		return model.Value{}, invalidInput()
	}
	if value == 0 {
		mantissa := input
		exponentMarker := "eE"
		if hexInput {
			exponentMarker = "pP"
		}
		if i := strings.IndexAny(mantissa, exponentMarker); i >= 0 {
			mantissa = mantissa[:i]
		}
		if hexInput {
			mantissa = strings.TrimLeft(mantissa, "+-")[2:]
		}
		for i := range len(mantissa) {
			if digit(mantissa[i]) > 0 {
				return model.Value{}, sqlError("22003", "floating-point input out of range")
			}
		}
	}
	return model.Float64Val(value), nil
}

func byteaUnit(input string, position int, hexInput bool) (byte, int, error) {
	if hexInput {
		for position < len(input) && (input[position] == ' ' || input[position] == '\n' || input[position] == '\r' || input[position] == '\t') {
			position++
		}
		if position == len(input) {
			return 0, position, nil
		}
		if position+1 >= len(input) {
			return 0, 0, invalidInput()
		}
		a, b := digit(input[position]), digit(input[position+1])
		if a < 0 || b < 0 {
			return 0, 0, invalidInput()
		}
		return byte(a*16 + b), position + 2, nil
	}
	if input[position] != '\\' {
		return input[position], position + 1, nil
	}
	if position+1 < len(input) && input[position+1] == '\\' {
		return '\\', position + 2, nil
	}
	if position+3 >= len(input) {
		return 0, 0, invalidInput()
	}
	first, second, third := input[position+1], input[position+2], input[position+3]
	validFirst := first >= '0' && first <= '3'
	validSecond := second >= '0' && second <= '7'
	validThird := third >= '0' && third <= '7'
	if validFirst && validSecond && validThird {
		return (first-'0')*64 + (second-'0')*8 + third - '0', position + 4, nil
	}
	return 0, 0, invalidInput()
}

func parseBytea(ctx context.Context, input string, budget int64) (model.Value, error) {
	hexInput := strings.HasPrefix(input, `\x`)
	if hexInput {
		input = input[2:]
		input = strings.TrimRight(input, " \n\r\t")
	}
	size := 0
	for i := 0; i < len(input); {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		_, next, err := byteaUnit(input, i, hexInput)
		if err != nil {
			return model.Value{}, err
		}
		i = next
		size++
		if err := outputBudget(int64(size), budget); err != nil {
			return model.Value{}, err
		}
	}
	output := make([]byte, size)
	for i, position := 0, 0; position < size; position++ {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		value, next, err := byteaUnit(input, i, hexInput)
		if err != nil {
			return model.Value{}, err
		}
		output[position], i = value, next
	}
	return model.BytesVal(output), nil
}
