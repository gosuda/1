package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileCharValue(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 33, 61, 70, 72, 73, 74, 77, 78, 358, 446, 454, 860, 944, 946, 1245, 1246, 2435:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		var value byte
		switch signature.ID {
		case 78:
			integer := args[0].IntVal
			if integer < -128 || integer > 127 {
				return model.Value{}, sqlError("22003", "\"char\" out of range")
			}
			value = byte(integer)
		case 944, 1245:
			text := args[0].StrVal
			if signature.ID == 1245 {
				// Only the first five bytes can affect charin's octal decision.
				text = text[:min(len(text), 5)]
				if end := strings.IndexByte(text, 0); end >= 0 {
					text = text[:end]
				}
			}
			value = parseCharValue(text)
		default:
			var err error
			value, err = charValueByte(args[0].StrVal)
			if err != nil {
				return model.Value{}, err
			}
		}
		switch signature.ID {
		case 61, 70, 72, 73, 74, 358, 1246:
			right, err := charValueByte(args[1].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			switch signature.ID {
			case 61:
				return model.BoolVal(value == right), nil
			case 70:
				return model.BoolVal(value != right), nil
			case 72:
				return model.BoolVal(value <= right), nil
			case 73:
				return model.BoolVal(value > right), nil
			case 74:
				return model.BoolVal(value >= right), nil
			case 1246:
				return model.BoolVal(value < right), nil
			default:
				return model.Int64Val(int64(value) - int64(right)), nil
			}
		case 77:
			return model.Int64Val(int64(int8(value))), nil
		case 454:
			return model.Int64Val(int64(int32(pgHashUint32(uint32(int32(int8(value))))))), nil
		case 446:
			return model.Int64Val(int64(pgHashUint32Extended(uint32(int32(int8(value))), uint64(args[1].IntVal)))), nil
		}
		var output [4]byte
		size := 0
		switch signature.ID {
		case 33, 946:
			if value >= 128 {
				output = [4]byte{'\\', '0' + value>>6, '0' + (value>>3)&7, '0' + value&7}
				size = 4
			} else if value != 0 {
				output[0], size = value, 1
			}
		case 860, 2435:
			// bpchar(char), like binary send, preserves even the raw NUL byte.
			output[0], size = value, 1
		default:
			if value != 0 {
				output[0], size = value, 1
			}
		}
		if err := outputBudget(int64(size), budget.OutputBytes); err != nil {
			return model.Value{}, err
		}
		if uint64(size) > budget.MemoryBytes {
			return model.Value{}, sqlError("54000", "builtin invocation memory budget exceeded")
		}
		if size != 0 && budget.Allocator != nil {
			claim, err := budget.Allocator.Reserve(uint64(size))
			if err != nil {
				return model.Value{}, err
			}
			defer claim.Release()
		}
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if signature.ID == 2435 {
			return model.BytesVal([]byte{value}), nil
		}
		return model.StringVal(string(output[:size])), nil
	}
}

// OID 18 uses TypeString, not SQL bpchar storage semantics. Empty StrVal is
// canonical NUL, distinct from IsNull; a single raw NUL byte decodes identically.
// Every nonzero value is one raw byte, including invalid standalone UTF-8.
func charValueByte(text string) (byte, error) {
	if len(text) > 1 {
		return 0, sqlError("22P02", "invalid internal \"char\" value")
	}
	if len(text) == 0 {
		return 0, nil
	}
	return text[0], nil
}

func parseCharValue(text string) byte {
	if len(text) == 0 {
		return 0
	}
	if len(text) != 4 || text[0] != '\\' {
		return text[0]
	}
	for _, digit := range text[1:] {
		if digit < '0' || digit > '7' {
			return text[0]
		}
	}
	// PostgreSQL accepts all three octal digits, truncating to eight bits.
	return (text[1]-'0')<<6 | (text[2]-'0')<<3 | (text[3] - '0')
}
