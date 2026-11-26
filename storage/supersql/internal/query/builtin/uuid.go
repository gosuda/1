package builtin

import (
	"cmp"
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileUUID(signature Signature) scalarKernel {
	var op primitiveOp
	switch signature.ID {
	case 2952, 2953:
		storage := model.TypeString
		if signature.ID == 2952 {
			storage = model.TypeUUID
		}
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			data, err := parseUUIDText(ctx, args[0].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			if err := outputBudget(36, budget); err != nil {
				return model.Value{}, err
			}
			return model.Value{Type: storage, StrVal: formatUUID(data)}, nil
		}
	case 2962:
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			data, err := parseUUIDText(ctx, args[0].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			return ownedBytes(ctx, budget, data[:])
		}
	case 6343:
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			data, err := parseUUIDText(ctx, args[0].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			if data[8]&0xc0 != 0x80 {
				return model.NullVal(model.TypeInt64), nil
			}
			return model.Int64Val(int64(data[6] >> 4)), nil
		}
	case 2954:
		op = opLt
	case 2955:
		op = opLe
	case 2956:
		op = opEq
	case 2957:
		op = opGe
	case 2958:
		op = opGt
	case 2959:
		op = opNe
	case 2960:
		op = opCmp
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		left, err := parseUUIDText(ctx, args[0].StrVal)
		if err != nil {
			return model.Value{}, err
		}
		right, err := parseUUIDText(ctx, args[1].StrVal)
		if err != nil {
			return model.Value{}, err
		}
		for i := range left {
			if left[i] != right[i] {
				// PostgreSQL returns memcmp directly; only its sign is portable.
				return comparisonResult(cmp.Compare(left[i], right[i]), op), nil
			}
		}
		return comparisonResult(0, op), nil
	}
}

func parseUUIDText(ctx context.Context, text string) ([16]byte, error) {
	var data [16]byte
	if err := ctx.Err(); err != nil {
		return data, err
	}
	// At most seven optional hyphens and a pair of braces surround 32 hex digits.
	if len(text) < 32 || len(text) > 41 {
		return data, invalidInput()
	}
	position := 0
	braces := text[0] == '{'
	if braces {
		position++
	}
	for i := range data {
		if position+1 >= len(text) {
			return [16]byte{}, invalidInput()
		}
		high, low := digit(text[position]), digit(text[position+1])
		if high < 0 || low < 0 {
			return [16]byte{}, invalidInput()
		}
		data[i] = byte(high<<4 | low)
		position += 2
		if i%2 == 1 && i < len(data)-1 && position < len(text) && text[position] == '-' {
			position++
		}
	}
	if braces {
		if position == len(text) || text[position] != '}' {
			return [16]byte{}, invalidInput()
		}
		position++
	}
	if position != len(text) {
		return [16]byte{}, invalidInput()
	}
	if err := ctx.Err(); err != nil {
		return [16]byte{}, err
	}
	return data, nil
}

func formatUUID(data [16]byte) string {
	const hex = "0123456789abcdef"
	var text [36]byte
	position := 0
	for i, value := range data {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			text[position] = '-'
			position++
		}
		text[position] = hex[value>>4]
		text[position+1] = hex[value&0x0f]
		position += 2
	}
	return string(text[:])
}
