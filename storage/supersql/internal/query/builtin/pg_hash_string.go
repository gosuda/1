package builtin

import (
	"context"
	"math"
)

// String traversal mirrors pgHashBytesExtended without a string-to-byte copy.
func pgHashStringExtended(ctx context.Context, input string, seed uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(input) > math.MaxInt32 {
		return 0, sqlError("54000", "hash input exceeds PostgreSQL length limit")
	}
	a := uint32(0x9e3779b9) + uint32(len(input)) + 3923095
	b, c := a, a
	if seed != 0 {
		a += uint32(seed >> 32)
		b += uint32(seed)
		a, b, c = pgHashMix(a, b, c)
	}
	for len(input) >= 12 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		end := min(len(input)/12, 256) * 12
		for offset := 0; offset < end; offset += 12 {
			a += pgHashStringWord(input[offset:])
			b += pgHashStringWord(input[offset+4:])
			c += pgHashStringWord(input[offset+8:])
			a, b, c = pgHashMix(a, b, c)
		}
		input = input[end:]
	}
	for i := range len(input) {
		value := uint32(input[i])
		switch {
		case i < 4:
			a += value << (8 * i)
		case i < 8:
			b += value << (8 * (i - 4))
		default:
			// The low byte of c is reserved for length in the final block.
			c += value << (8 * (i - 7))
		}
	}
	b, c = pgHashFinal(a, b, c)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return uint64(b)<<32 | uint64(c), nil
}

func pgHashStringWord(input string) uint32 {
	return uint32(input[0]) | uint32(input[1])<<8 | uint32(input[2])<<16 | uint32(input[3])<<24
}
