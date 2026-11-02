package crypto

import (
	"encoding/binary"
)

// hChaCha20 computes HChaCha20(key[32], in[16]) per spec section 4.4:
// ChaCha constants, eight little-endian key words, four little-endian input
// words, ten double rounds, serialize state words 0,1,2,3,12,13,14,15
// without feed-forward.
func hChaCha20(key []byte, in []byte) [32]byte {
	var st [16]uint32
	st[0] = 0x61707865
	st[1] = 0x3320646e
	st[2] = 0x79622d32
	st[3] = 0x6b206574
	for i := 0; i < 8; i++ {
		st[4+i] = binary.LittleEndian.Uint32(key[i*4:])
	}
	for i := 0; i < 4; i++ {
		st[12+i] = binary.LittleEndian.Uint32(in[i*4:])
	}
	for i := 0; i < 10; i++ {
		// Column round.
		st[0], st[4], st[8], st[12] = quarterRound(st[0], st[4], st[8], st[12])
		st[1], st[5], st[9], st[13] = quarterRound(st[1], st[5], st[9], st[13])
		st[2], st[6], st[10], st[14] = quarterRound(st[2], st[6], st[10], st[14])
		st[3], st[7], st[11], st[15] = quarterRound(st[3], st[7], st[11], st[15])
		// Diagonal round.
		st[0], st[5], st[10], st[15] = quarterRound(st[0], st[5], st[10], st[15])
		st[1], st[6], st[11], st[12] = quarterRound(st[1], st[6], st[11], st[12])
		st[2], st[7], st[8], st[13] = quarterRound(st[2], st[7], st[8], st[13])
		st[3], st[4], st[9], st[14] = quarterRound(st[3], st[4], st[9], st[14])
	}
	var out [32]byte
	for i, w := range [8]uint32{st[0], st[1], st[2], st[3], st[12], st[13], st[14], st[15]} {
		binary.LittleEndian.PutUint32(out[i*4:], w)
	}
	return out
}

func quarterRound(a, b, c, d uint32) (uint32, uint32, uint32, uint32) {
	a += b
	d ^= a
	d = d<<16 | d>>16
	c += d
	b ^= c
	b = b<<12 | b>>20
	a += b
	d ^= a
	d = d<<8 | d>>24
	c += d
	b ^= c
	b = b<<7 | b>>25
	return a, b, c, d
}
