package neosepia

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
)

type columnEncoding uint8

const (
	encodingPlain columnEncoding = iota
	encodingDictionary
	encodingRLE
	encodingBitPacked
	encodingDelta
	encodingLOB
)

var errColumnEncoding = errors.New("corrupt PAX column encoding")

// The RLE and delta payload layouts retain the existing NeoSepia kernels' wire
// format. Selection is costed without allocating both candidate encodings.
func encodeIntegers(dst []byte, values []int64) ([]byte, columnEncoding) {
	if len(values) == 0 {
		return dst, encodingPlain
	}
	lo, hi, runs := values[0], values[0], 1
	for i, v := range values[1:] {
		lo, hi = min(lo, v), max(hi, v)
		if v != values[i] {
			runs++
		}
	}
	width := max(1, bits.Len64(uint64(hi)-uint64(lo)))
	deltaBytes := 13 + (len(values)*width+7)/8
	rleBytes := 4 + runs*12
	if rleBytes < deltaBytes && rleBytes < len(values)*8 {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(runs))
		start := 0
		for i := 1; i <= len(values); i++ {
			if i != len(values) && values[i] == values[start] {
				continue
			}
			dst = binary.LittleEndian.AppendUint32(dst, uint32(i-start))
			dst = binary.LittleEndian.AppendUint64(dst, uint64(values[start]))
			start = i
		}
		return dst, encodingRLE
	}
	if deltaBytes < len(values)*8 {
		dst = binary.LittleEndian.AppendUint64(dst, uint64(lo))
		dst = append(dst, byte(width))
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(values)))
		start := len(dst)
		dst = append(dst, make([]byte, deltaBytes-13)...)
		position := 0
		for _, v := range values {
			delta := uint64(v) - uint64(lo)
			for remaining := width; remaining > 0; {
				shift := position % 8
				n := min(remaining, 8-shift)
				dst[start+position/8] |= byte(delta&((1<<n)-1)) << shift
				delta >>= n
				position += n
				remaining -= n
			}
		}
		return dst, encodingDelta
	}
	for _, v := range values {
		dst = binary.LittleEndian.AppendUint64(dst, uint64(v))
	}
	return dst, encodingPlain
}

// The caller supplies exactly the bounded output lanes; malformed counts never
// allocate and never leave a short RLE stream looking like zero-valued rows.
func decodeIntegers(data []byte, encoding columnEncoding, dst []int64) error {
	switch encoding {
	case encodingPlain:
		if len(data)/8 != len(dst) || len(data)%8 != 0 {
			return errColumnEncoding
		}
		for i := range dst {
			dst[i] = int64(binary.LittleEndian.Uint64(data[i*8:]))
		}
		return nil
	case encodingRLE:
		return decodeRuns(data, dst)
	case encodingDelta:
		return decodeDeltas(data, dst)
	default:
		return errColumnEncoding
	}
}

func decodeRuns(data []byte, dst []int64) error {
	if len(data) < 4 {
		return errColumnEncoding
	}
	runs := uint64(binary.LittleEndian.Uint32(data))
	data = data[4:]
	if runs != uint64(len(data))/12 || len(data)%12 != 0 {
		return errColumnEncoding
	}
	position := 0
	for len(data) != 0 {
		count := uint64(binary.LittleEndian.Uint32(data))
		if count == 0 || count > uint64(len(dst)-position) {
			return errColumnEncoding
		}
		value := int64(binary.LittleEndian.Uint64(data[4:]))
		end := position + int(count)
		for i := position; i < end; i++ {
			dst[i] = value
		}
		position = end
		data = data[12:]
	}
	if position != len(dst) {
		return errColumnEncoding
	}
	return nil
}

func decodeDeltas(data []byte, dst []int64) error {
	if len(data) < 13 {
		return errColumnEncoding
	}
	base := int64(binary.LittleEndian.Uint64(data))
	width := int(data[8])
	if width < 1 || width > 64 {
		return errColumnEncoding
	}
	if uint64(binary.LittleEndian.Uint32(data[9:])) != uint64(len(dst)) {
		return errColumnEncoding
	}
	data = data[13:]
	if uint64(len(data)) != (uint64(len(dst))*uint64(width)+7)/8 {
		return errColumnEncoding
	}
	position := 0
	for i := range dst {
		var delta uint64
		for remaining := width; remaining > 0; {
			offset := position % 8
			n := min(remaining, 8-offset)
			part := uint64(data[position/8]>>offset) & ((1 << n) - 1)
			delta |= part << (width - remaining)
			position += n
			remaining -= n
		}
		if delta > uint64(math.MaxInt64)-uint64(base) {
			return errColumnEncoding
		}
		dst[i] = int64(uint64(base) + delta)
	}
	return nil
}
