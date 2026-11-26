package builtin

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const bitStringMaxLen = math.MaxInt32 - 7

// bitString borrows canonical MSB-first storage; only constructors own payloads.
type bitString struct {
	bitLen int
	data   []byte
}

func decodeBitString(raw []byte) (bitString, error) {
	if len(raw) < 4 {
		return bitString{}, sqlError("22P03", "invalid internal bit string")
	}
	n := uint64(binary.BigEndian.Uint32(raw))
	if n > bitStringMaxLen || uint64(len(raw)-4) != (n+7)/8 {
		return bitString{}, sqlError("22P03", "invalid internal bit string length")
	}
	if n%8 != 0 && raw[len(raw)-1]&byte((1<<(8-n%8))-1) != 0 {
		return bitString{}, sqlError("22P03", "invalid internal bit string padding")
	}
	return bitString{int(n), raw[4:]}, nil
}

func (value bitString) bit(index int) byte {
	return value.data[index/8] >> (7 - uint(index%8)) & 1
}

func bitStringOutput(ctx context.Context, size int64, budget CallBudget, build func() (model.Value, error)) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if err := outputBudget(size, budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	if uint64(size) > budget.MemoryBytes {
		return model.Value{}, sqlError("54000", "builtin invocation memory budget exceeded")
	}
	if size > 0 && budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(uint64(size))
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	result, err := build()
	if err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	return result, nil
}

func newBitString(ctx context.Context, length int64, budget CallBudget, fill func([]byte) error) (model.Value, error) {
	if length < 0 || length > bitStringMaxLen {
		return model.Value{}, sqlError("54000", "bit string length exceeds the maximum allowed (2147483640)")
	}
	return bitStringOutput(ctx, 4+(length+7)/8, budget, func() (model.Value, error) {
		raw := make([]byte, 4+(length+7)/8)
		binary.BigEndian.PutUint32(raw, uint32(length))
		if fill != nil {
			if err := fill(raw[4:]); err != nil {
				return model.Value{}, err
			}
		}
		if length%8 != 0 {
			raw[len(raw)-1] &= byte(255 << (8 - uint(length%8)))
		}
		return model.BytesVal(raw), nil
	})
}

// copyBitString copies arbitrary bit ranges into zero-initialized output.
func copyBitString(ctx context.Context, output []byte, destination int, source bitString, start, length int) error {
	for copied := 0; copied < length; {
		if (copied/8)%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		n := min(8-destination%8, length-copied)
		index, shift := start/8, uint(start%8)
		value := source.data[index] << shift
		if shift != 0 && index+1 < len(source.data) {
			value |= source.data[index+1] >> (8 - shift)
		}
		value &= byte(255 << (8 - uint(n)))
		output[destination/8] |= value >> uint(destination%8)
		destination += n
		start += n
		copied += n
	}
	return ctx.Err()
}

func compareBitStrings(ctx context.Context, left, right bitString) (int, error) {
	for i := range min(len(left.data), len(right.data)) {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if left.data[i] != right.data[i] {
			return int(left.data[i]) - int(right.data[i]), nil
		}
	}
	if left.bitLen < right.bitLen {
		return -1, nil
	}
	if left.bitLen > right.bitLen {
		return 1, nil
	}
	return 0, nil
}

func parseBitString(ctx context.Context, text string, typmod int32, varying bool, budget CallBudget) (model.Value, error) {
	// C-string input stops at NUL, including before an optional b/x prefix.
	for start := 0; start < len(text); start += 4096 {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if end := strings.IndexByte(text[start:min(start+4096, len(text))], 0); end >= 0 {
			text = text[:start+end]
			break
		}
	}
	hex := false
	if len(text) > 0 {
		switch text[0] {
		case 'b', 'B':
			text = text[1:]
		case 'x', 'X':
			text, hex = text[1:], true
		}
	}
	length := int64(len(text))
	if hex {
		length *= 4
	}
	if length > bitStringMaxLen {
		return model.Value{}, sqlError("54000", "bit string length exceeds the maximum allowed (2147483640)")
	}
	if typmod > 0 {
		if !varying && length != int64(typmod) {
			return model.Value{}, sqlError("22026", fmt.Sprintf("bit string length %d does not match type bit(%d)", length, typmod))
		}
		if varying && length > int64(typmod) {
			return model.Value{}, sqlError("22001", fmt.Sprintf("bit string too long for type bit varying(%d)", typmod))
		}
	}
	return newBitString(ctx, length, budget, func(output []byte) error {
		for i := range len(text) {
			if i%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			c := text[i]
			if !hex {
				if c == '1' {
					output[i/8] |= 1 << (7 - uint(i%8))
				} else if c != '0' {
					_, width := utf8.DecodeRuneInString(text[i:])
					return sqlError("22P02", fmt.Sprintf("\"%s\" is not a valid binary digit", text[i:i+width]))
				}
				continue
			}
			var digit byte
			switch {
			case c >= '0' && c <= '9':
				digit = c - '0'
			case c >= 'a' && c <= 'f':
				digit = c - 'a' + 10
			case c >= 'A' && c <= 'F':
				digit = c - 'A' + 10
			default:
				_, width := utf8.DecodeRuneInString(text[i:])
				return sqlError("22P02", fmt.Sprintf("\"%s\" is not a valid hexadecimal digit", text[i:i+width]))
			}
			output[i/2] |= digit << (4 - 4*uint(i%2))
		}
		return nil
	})
}

func formatBitString(ctx context.Context, value bitString, budget CallBudget) (model.Value, error) {
	return bitStringOutput(ctx, int64(value.bitLen), budget, func() (model.Value, error) {
		var output strings.Builder
		output.Grow(value.bitLen)
		for i := range value.bitLen {
			if i%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			output.WriteByte('0' + value.bit(i))
		}
		return model.StringVal(output.String()), nil
	})
}
