package builtin

import (
	"context"
	"encoding/binary"
	"math/bits"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileByteAccess(signature Signature) scalarKernel {
	switch signature.ID {
	case 721, 722, 723, 724:
	case 6163:
		return countByteBits
	default:
		return nil
	}
	bitAccess := signature.ID == 723 || signature.ID == 724
	setter := signature.ID == 722 || signature.ID == 724
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		input, index := args[0].BytesVal, args[1].IntVal
		byteIndex := index
		if bitAccess {
			// Divide the index rather than multiplying the input length by eight.
			byteIndex /= 8
		}
		if index < 0 || byteIndex >= int64(len(input)) {
			return model.Value{}, sqlError("2202E", "bytea index out of range")
		}
		value := input[byteIndex]
		if !setter {
			if bitAccess {
				value = value >> uint(index%8) & 1
			}
			return model.Int64Val(int64(value)), nil
		}
		newValue := args[2].IntVal
		if bitAccess {
			if newValue != 0 && newValue != 1 {
				return model.Value{}, sqlError("22023", "new bit must be 0 or 1")
			}
			mask := byte(1 << uint(index%8))
			if newValue == 0 {
				value &^= mask
			} else {
				value |= mask
			}
		} else {
			value = byte(newValue)
		}
		output, err := ownedBytes(ctx, budget, input)
		if err != nil {
			return model.Value{}, err
		}
		output.BytesVal[byteIndex] = value
		return output, nil
	}
}

func countByteBits(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	input := args[0].BytesVal
	var count int64
	for len(input) > 0 {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		size := min(cancellationChunk, len(input))
		chunk := input[:size]
		input = input[size:]
		for len(chunk) >= 8 {
			count += int64(bits.OnesCount64(binary.LittleEndian.Uint64(chunk)))
			chunk = chunk[8:]
		}
		for _, value := range chunk {
			count += int64(bits.OnesCount8(value))
		}
	}
	return model.Int64Val(count), nil
}
