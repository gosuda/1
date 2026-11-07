package builtin

import (
	"context"
	"encoding/binary"
	"math"
	"math/bits"
)

// PostgreSQL REL_18_4 hashfn.c, using its little-endian byte order on every host.
// Input slices are borrowed; hashing uses constant space and never copies them.
func pgHashBytes(ctx context.Context, input []byte) (uint32, error) {
	hash, err := pgHashBytesExtended(ctx, input, 0)
	return uint32(hash), err
}

func pgHashBytesExtended(ctx context.Context, input []byte, seed uint64) (uint64, error) {
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
		// Check cancellation once per bounded group of complete hash blocks.
		end := min(len(input)/12, 256) * 12
		for offset := 0; offset < end; offset += 12 {
			a += binary.LittleEndian.Uint32(input[offset:])
			b += binary.LittleEndian.Uint32(input[offset+4:])
			c += binary.LittleEndian.Uint32(input[offset+8:])
			a, b, c = pgHashMix(a, b, c)
		}
		input = input[end:]
	}
	for i, value := range input {
		switch {
		case i < 4:
			a += uint32(value) << (8 * i)
		case i < 8:
			b += uint32(value) << (8 * (i - 4))
		default:
			// The low byte of c is reserved for length in the final block.
			c += uint32(value) << (8 * (i - 7))
		}
	}
	b, c = pgHashFinal(a, b, c)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return uint64(b)<<32 | uint64(c), nil
}

func pgHashUint32(value uint32) uint32 {
	return uint32(pgHashUint32Extended(value, 0))
}

func pgHashUint32Extended(value uint32, seed uint64) uint64 {
	a := uint32(0x9e3779b9 + 4 + 3923095)
	b, c := a, a
	if seed != 0 {
		a += uint32(seed >> 32)
		b += uint32(seed)
		a, b, c = pgHashMix(a, b, c)
	}
	a += value
	b, c = pgHashFinal(a, b, c)
	return uint64(b)<<32 | uint64(c)
}

func pgHashMix(a, b, c uint32) (uint32, uint32, uint32) {
	a -= c
	a ^= bits.RotateLeft32(c, 4)
	c += b
	b -= a
	b ^= bits.RotateLeft32(a, 6)
	a += c
	c -= b
	c ^= bits.RotateLeft32(b, 8)
	b += a
	a -= c
	a ^= bits.RotateLeft32(c, 16)
	c += b
	b -= a
	b ^= bits.RotateLeft32(a, 19)
	a += c
	c -= b
	c ^= bits.RotateLeft32(b, 4)
	b += a
	return a, b, c
}

func pgHashFinal(a, b, c uint32) (uint32, uint32) {
	c ^= b
	c -= bits.RotateLeft32(b, 14)
	a ^= c
	a -= bits.RotateLeft32(c, 11)
	b ^= a
	b -= bits.RotateLeft32(a, 25)
	c ^= b
	c -= bits.RotateLeft32(b, 16)
	a ^= c
	a -= bits.RotateLeft32(c, 4)
	b ^= a
	b -= bits.RotateLeft32(a, 14)
	c ^= b
	c -= bits.RotateLeft32(b, 24)
	return b, c
}
