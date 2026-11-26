package builtin

import (
	"context"
	"encoding/binary"
	"math"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileBinaryValue(signature Signature, _ ResolveOptions) scalarKernel {
	width := 0
	switch signature.ID {
	case 2405, 6367, 6370:
		width = 2
	case 2407, 2419, 2425, 6368, 6371:
		width = 4
	case 2409, 2427, 6369, 6372:
		width = 8
	case 2437:
		width = 1
	case 2473:
		width = 12
	case 2413, 2415, 2423, 2431, 2433:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		input := args[0]
		if signature.ID >= 6370 && signature.ID <= 6372 {
			return binaryValueInteger(input.BytesVal, width)
		}
		var microseconds int64
		var secondsWest int32
		if signature.ID == 2473 {
			var err error
			microseconds, secondsWest, err = DecodeTimetz(input.BytesVal)
			if err != nil {
				return model.Value{}, err
			}
		}
		text := input.StrVal
		size := width
		switch signature.ID {
		case 2413:
			size = len(input.BytesVal)
		case 2423:
			if end := strings.IndexByte(text, 0); end >= 0 {
				text = text[:end]
			}
			size = len(text)
		case 2415, 2431, 2433:
			size = len(text)
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
		output := make([]byte, size)
		switch signature.ID {
		case 2413, 2415, 2423, 2431, 2433:
			for start := 0; start < size; {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
				end := min(start+4096, size)
				if signature.ID == 2413 {
					copy(output[start:end], input.BytesVal[start:end])
				} else {
					copy(output[start:end], text[start:end])
				}
				start = end
			}
		case 2425:
			binary.BigEndian.PutUint32(output, math.Float32bits(float32(input.FloatVal)))
		case 2427:
			binary.BigEndian.PutUint64(output, math.Float64bits(input.FloatVal))
		case 2437:
			output[0] = byte(input.IntVal)
		case 2473:
			binary.BigEndian.PutUint64(output, uint64(microseconds))
			binary.BigEndian.PutUint32(output[8:], uint32(secondsWest))
		default:
			switch width {
			case 2:
				binary.BigEndian.PutUint16(output, uint16(input.IntVal))
			case 4:
				binary.BigEndian.PutUint32(output, uint32(input.IntVal))
			case 8:
				binary.BigEndian.PutUint64(output, uint64(input.IntVal))
			}
		}
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		return model.BytesVal(output), nil
	}
}

func binaryValueInteger(input []byte, width int) (model.Value, error) {
	if len(input) > width {
		switch width {
		case 2:
			return model.Value{}, sqlError("22003", "smallint out of range")
		case 4:
			return model.Value{}, sqlError("22003", "integer out of range")
		default:
			return model.Value{}, sqlError("22003", "bigint out of range")
		}
	}
	// PostgreSQL zero-extends short bytea inputs, then interprets the complete
	// target width as signed. An empty bytea therefore casts to zero.
	var bits uint64
	for _, value := range input {
		bits = bits<<8 | uint64(value)
	}
	switch width {
	case 2:
		return model.Int64Val(int64(int16(bits))), nil
	case 4:
		return model.Int64Val(int64(int32(bits))), nil
	default:
		return model.Int64Val(int64(bits)), nil
	}
}
