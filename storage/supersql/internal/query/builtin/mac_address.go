package builtin

import (
	"bytes"
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func macAddressSize(typeID model.TypeID) int {
	switch typeID {
	case 829:
		return 6
	case 774:
		return 8
	default:
		return 0
	}
}

func decodeMACAddress(input []byte, typeID model.TypeID) ([8]byte, error) {
	var address [8]byte
	size := macAddressSize(typeID)
	if size == 0 || len(input) != size {
		return address, sqlError("22P03", "invalid binary MAC address")
	}
	copy(address[:], input)
	return address, nil
}

func compileMACAddress(signature Signature, _ ResolveOptions) scalarKernel {
	var op primitiveOp
	switch signature.ID {
	case 830, 4113:
		op = opEq
	case 831, 4114:
		op = opLt
	case 832, 4115:
		op = opLe
	case 833, 4116:
		op = opGt
	case 834, 4117:
		op = opGe
	case 835, 4118:
		op = opNe
	case 836, 4119:
		op = opCmp
	case 328, 399, 436, 437, 753, 778, 781, 2495, 3144, 3145, 3146,
		3447, 4110, 4111, 4112, 4120, 4121, 4122, 4123, 4124, 4125:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if signature.ID == 436 || signature.ID == 4110 {
			address, err := parseMACAddress(ctx, args[0].StrVal, signature.Result)
			if err != nil {
				return model.Value{}, err
			}
			return macAddressOutput(ctx, budget, address[:macAddressSize(signature.Result)], false)
		}
		size := macAddressSize(signature.Arguments[0])
		left, err := decodeMACAddress(args[0].BytesVal, signature.Arguments[0])
		if err != nil {
			return model.Value{}, err
		}
		if isComparison(op) {
			right, err := decodeMACAddress(args[1].BytesVal, signature.Arguments[1])
			if err != nil {
				return model.Value{}, err
			}
			return comparisonResult(bytes.Compare(left[:size], right[:size]), op), nil
		}
		switch signature.ID {
		case 328, 399:
			hash, err := pgHashBytes(ctx, left[:size])
			return model.Int64Val(int64(int32(hash))), err
		case 778, 781:
			hash, err := pgHashBytesExtended(ctx, left[:size], uint64(args[1].IntVal))
			return model.Int64Val(int64(hash)), err
		case 437, 4111:
			const hex = "0123456789abcdef"
			var text [23]byte
			for i, value := range left[:size] {
				if i != 0 {
					text[3*i-1] = ':'
				}
				text[3*i], text[3*i+1] = hex[value>>4], hex[value&15]
			}
			return macAddressOutput(ctx, budget, text[:3*size-1], true)
		case 753, 4112:
			clear(left[3:])
		case 3144, 4120:
			for i := range size {
				left[i] = ^left[i]
			}
		case 3145, 3146, 4121, 4122:
			right, err := decodeMACAddress(args[1].BytesVal, signature.Arguments[1])
			if err != nil {
				return model.Value{}, err
			}
			for i := range size {
				if signature.ID == 3145 || signature.ID == 4121 {
					left[i] &= right[i]
				} else {
					left[i] |= right[i]
				}
			}
		case 4123:
			left = expandMACAddress(left)
			size = 8
		case 4124:
			if left[3] != 0xff || left[4] != 0xfe {
				return model.Value{}, sqlError("22003", "macaddr8 data out of range to convert to macaddr")
			}
			copy(left[3:6], left[5:8])
			size = 6
		case 4125:
			left[0] |= 2
		}
		return macAddressOutput(ctx, budget, left[:size], false)
	}
}

func expandMACAddress(address [8]byte) [8]byte {
	return [8]byte{address[0], address[1], address[2], 0xff, 0xfe, address[3], address[4], address[5]}
}

func macAddressOutput(ctx context.Context, budget CallBudget, data []byte, text bool) (model.Value, error) {
	if err := outputBudget(int64(len(data)), budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	if uint64(len(data)) > budget.MemoryBytes {
		return model.Value{}, sqlError("54000", "builtin invocation memory budget exceeded")
	}
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(uint64(len(data)))
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if text {
		return model.StringVal(string(data)), nil
	}
	return model.BytesVal(bytes.Clone(data)), nil
}
