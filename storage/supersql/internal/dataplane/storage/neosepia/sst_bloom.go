package neosepia

import (
	"bytes"

	"github.com/zeebo/xxh3"
)

const (
	bloomBitsPerKey    = 10
	bloomDefaultProbes = 7
	bloomMinBytes      = 8
)

// bloomBuilder accumulates 128-bit xxh3 hashes of distinct keys in sorted
// order during SST construction and materializes a compact bitset on Finish.
type bloomBuilder struct {
	hashes   [][2]uint64
	lastKey  []byte
	hasLast  bool
	maxBytes uint64
	overflow bool
}

func newBloomBuilder(maxIndexBytes uint64) bloomBuilder {
	// Cap bloom filter at 1/4 of MaxIndexBytes so metadata stays bounded.
	capBytes := maxIndexBytes / 4
	return bloomBuilder{maxBytes: capBytes}
}

func (b *bloomBuilder) addKey(key []byte) {
	if b.overflow || b.maxBytes < bloomMinBytes {
		return
	}
	if b.hasLast && bytes.Equal(b.lastKey, key) {
		return
	}
	b.lastKey = append(b.lastKey[:0], key...)
	b.hasLast = true
	// Check if resulting bloom bitset would exceed maxBytes.
	nextCount := uint64(len(b.hashes) + 1)
	neededBytes := bloomByteSize(nextCount)
	if neededBytes > b.maxBytes {
		b.overflow = true
		b.hashes = nil
		b.lastKey = nil
		return
	}
	h := xxh3.Hash128(key)
	b.hashes = append(b.hashes, [2]uint64{h.Lo, h.Hi})
}

func bloomByteSize(distinctKeys uint64) uint64 {
	if distinctKeys == 0 {
		return 0
	}
	bits := distinctKeys * bloomBitsPerKey
	bytesLen := ((bits + 63) / 64) * 8
	if bytesLen < bloomMinBytes {
		bytesLen = bloomMinBytes
	}
	return bytesLen
}

// estimatedBytes returns the exact byte length Finish will emit if capped by
// remaining index space.
func (b *bloomBuilder) estimatedBytes() uint64 {
	if b.overflow || len(b.hashes) == 0 {
		return 0
	}
	return bloomByteSize(uint64(len(b.hashes)))
}

func (b *bloomBuilder) finish(maxRemaining uint64) ([]byte, uint32) {
	if b.overflow || len(b.hashes) == 0 {
		return nil, 0
	}
	byteLen := bloomByteSize(uint64(len(b.hashes)))
	if byteLen > maxRemaining || byteLen == 0 {
		return nil, 0
	}
	bits := make([]byte, byteLen)
	m := byteLen * 8
	for _, h := range b.hashes {
		h1 := h[0]
		h2 := h[1] | 1
		for i := uint64(0); i < bloomDefaultProbes; i++ {
			pos := (h1 + i*h2) % m
			bits[pos/8] |= 1 << (pos % 8)
		}
	}
	return bits, bloomDefaultProbes
}

// bloomMayContain reports whether key may be present in the SST bloom filter.
// An empty filter conservatively returns true.
func bloomMayContain(bits []byte, probes uint32, key []byte) bool {
	if len(bits) == 0 || probes == 0 {
		return true
	}
	m := uint64(len(bits)) * 8
	h := xxh3.Hash128(key)
	h1 := h.Lo
	h2 := h.Hi | 1
	for i := uint64(0); i < uint64(probes); i++ {
		pos := (h1 + i*h2) % m
		if bits[pos/8]&(1<<(pos%8)) == 0 {
			return false
		}
	}
	return true
}
