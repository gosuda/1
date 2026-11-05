package builtin

import (
	"context"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileOIDName(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 1287, 1288, 1400, 1401, 1798, 1799:
	default:
		return nil
	}
	nameIO := compileNameIO(signature)
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		switch signature.ID {
		case 1287:
			value := args[0].IntVal
			if value < 0 || value > math.MaxUint32 {
				return model.Value{}, sqlError("22003", "OID out of range")
			}
			return model.Int64Val(value), nil
		case 1288:
			return model.Int64Val(int64(uint32(args[0].IntVal))), nil
		case 1798:
			return parseOID(ctx, args[0].StrVal)
		}
		available := callBudget.MemoryBytes
		if callBudget.Allocator != nil {
			available = min(available, callBudget.Allocator.AvailableBytes())
		}
		callBudget.OutputBytes = min(callBudget.OutputBytes, int64(min(available, uint64(math.MaxInt64))))
		if signature.ID == 1799 {
			return ownedText(ctx, callBudget.OutputBytes, strconv.FormatUint(uint64(uint32(args[0].IntVal)), 10))
		}
		return nameIO(ctx, args, callBudget)
	}
}

// PostgreSQL 18.4 uint32in_subr uses strtoul(base=0), not the SQL integer
// parser: leading zero means octal, and underscores and 0o/0b are not accepted.
// Its 64-bit unsigned-long result must survive unsigned or signed extension
// from uint32, including the historical negative-input wrapping behavior.
func parseOID(ctx context.Context, input string) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	position := 0
	for position < len(input) && isTSSpace(input[position]) {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		position++
	}
	negative := false
	if position < len(input) && (input[position] == '+' || input[position] == '-') {
		negative = input[position] == '-'
		position++
	}
	base := uint64(10)
	if position < len(input) && input[position] == '0' {
		base = 8
		if position+2 < len(input) && (input[position+1] == 'x' || input[position+1] == 'X') {
			if next := digit(input[position+2]); next >= 0 && next < 16 {
				base = 16
				position += 2
			}
		}
	}
	firstDigit := position
	var value uint64
	for position < len(input) {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		d := digit(input[position])
		if d < 0 || uint64(d) >= base {
			break
		}
		if value > (math.MaxUint64-uint64(d))/base {
			return model.Value{}, sqlError("22003", "OID out of range")
		}
		value = value*base + uint64(d)
		position++
	}
	if position == firstDigit {
		return model.Value{}, invalidInput()
	}
	for position < len(input) && isTSSpace(input[position]) {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		position++
	}
	if position < len(input) && input[position] != 0 {
		return model.Value{}, invalidInput()
	}
	if negative {
		value = -value
	}
	result := uint32(value)
	if value != uint64(result) && value != uint64(int64(int32(result))) {
		return model.Value{}, sqlError("22003", "OID out of range")
	}
	return model.Int64Val(int64(result)), nil
}
