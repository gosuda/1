package builtin

import (
	"context"
	"encoding/binary"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func structuredByteKey(id model.TypeID) bool {
	switch id {
	case 650, 774, 829, 869, 1560, 1562, 3614, 3615:
		return true
	default:
		return false
	}
}

func structuredByteKeySize(ctx context.Context, id model.TypeID, raw []byte, budget uint64) (uint64, error) {
	var size uint64
	switch id {
	case 3614, 3615:
		return fullTextKeySize(ctx, id, raw, budget)
	case 774, 829:
		if _, err := decodeMACAddress(raw, id); err != nil {
			return 0, err
		}
		size = 1 + uint64(macAddressSize(id))
	case 650, 869:
		address, err := decodeNetwork(raw, id == 650)
		if err != nil {
			return 0, err
		}
		size = 2 + 3*uint64(address.size())
	case 1560, 1562:
		value, err := decodeBitString(raw)
		if err != nil {
			return 0, err
		}
		size = uint64(len(value.data)) + 7
		if size > budget {
			return 0, columnar.ErrBatchBudget
		}
		for i, b := range value.data {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
			}
			if b == 0 {
				size++
			}
		}
	default:
		return 0, storage.ErrInvalidScanPlan
	}
	if size > budget {
		return 0, columnar.ErrBatchBudget
	}
	return size, ctx.Err()
}

func appendStructuredByteKey(ctx context.Context, key []byte, id model.TypeID, raw []byte, mask byte) ([]byte, error) {
	switch id {
	case 3614, 3615:
		return appendFullTextKey(ctx, key, id, raw, mask)
	case 774, 829:
		address, err := decodeMACAddress(raw, id)
		if err != nil {
			return nil, err
		}
		for _, b := range address[:macAddressSize(id)] {
			key = append(key, b^mask)
		}
		return key, ctx.Err()
	case 650, 869:
		address, err := decodeNetwork(raw, id == 650)
		if err != nil {
			return nil, err
		}
		return appendNetworkKey(key, address, mask), ctx.Err()
	case 1560, 1562:
		value, err := decodeBitString(raw)
		if err != nil {
			return nil, err
		}
		for i, b := range value.data {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			key = append(key, b^mask)
			if b == 0 {
				key = append(key, 0xff^mask)
			}
		}
		key = append(key, mask, mask)
		length := uint32(value.bitLen)
		if mask != 0 {
			length = ^length
		}
		return binary.BigEndian.AppendUint32(key, length), ctx.Err()
	default:
		return nil, storage.ErrInvalidScanPlan
	}
}

// Prefix symbols 01/10 sort bits after their 00 terminator. This preserves
// PostgreSQL's prefix-first network order without expanding each bit to a byte.
func appendNetworkKey(key []byte, address networkAddress, mask byte) []byte {
	var prefix [32]byte
	for i := 0; i < int(address.prefix); i++ {
		symbol := byte(1) + (address.address[i/8] >> (7 - uint(i%8)) & 1)
		prefix[i/4] |= symbol << (6 - uint(i%4)*2)
	}
	key = append(key, address.family^mask)
	for _, b := range prefix[:address.size()*2] {
		key = append(key, b^mask)
	}
	for _, b := range address.address[:address.size()] {
		key = append(key, b^mask)
	}
	return key
}

func (d *keyDecoder) structuredBytes(ctx context.Context, id model.TypeID, mask byte) (model.Value, error) {
	switch id {
	case 3614, 3615:
		return d.fullTextBytes(ctx, id, mask)
	case 774, 829:
		width := macAddressSize(id)
		if width > len(d.data)-d.offset {
			return model.Value{}, storage.ErrInvalidEncoding
		}
		if err := d.charge(uint64(width)); err != nil {
			return model.Value{}, err
		}
		result := make([]byte, width)
		for i := range result {
			result[i] = d.data[d.offset+i] ^ mask
		}
		d.offset += width
		return model.BytesVal(result), nil
	case 650, 869:
		return d.networkBytes(id == 650, mask)
	case 1560, 1562:
		raw, err := d.escaped(ctx, mask, false, 4)
		if err != nil {
			return model.Value{}, err
		}
		length, err := d.number(4, mask)
		if err != nil {
			return model.Value{}, err
		}
		binary.BigEndian.PutUint32(raw, uint32(length))
		if _, err := decodeBitString(raw); err != nil {
			return model.Value{}, storage.ErrInvalidEncoding
		}
		return model.BytesVal(raw), nil
	default:
		return model.Value{}, storage.ErrInvalidEncoding
	}
}

func (d *keyDecoder) networkBytes(cidr bool, mask byte) (model.Value, error) {
	family, err := d.byte(mask)
	if err != nil {
		return model.Value{}, err
	}
	if family != 2 && family != 3 {
		return model.Value{}, storage.ErrInvalidEncoding
	}
	address := networkAddress{family: family}
	width := address.size()
	if width*3 > len(d.data)-d.offset {
		return model.Value{}, storage.ErrInvalidEncoding
	}
	var expectedPrefix [16]byte
	terminated := false
	for i := 0; i < width*8; i++ {
		packed := d.data[d.offset+i/4] ^ mask
		symbol := packed >> (6 - uint(i%4)*2) & 3
		if symbol == 0 {
			terminated = true
			continue
		}
		if terminated || symbol == 3 {
			return model.Value{}, storage.ErrInvalidEncoding
		}
		expectedPrefix[i/8] |= (symbol - 1) << (7 - uint(i%8))
		address.prefix++
	}
	d.offset += width * 2
	for i := range width {
		address.address[i] = d.data[d.offset+i] ^ mask
	}
	d.offset += width
	if address.network().address != expectedPrefix {
		return model.Value{}, storage.ErrInvalidEncoding
	}
	if cidr && !address.hostIsZero() {
		return model.Value{}, storage.ErrInvalidEncoding
	}
	if err := d.charge(uint64(width + 2)); err != nil {
		return model.Value{}, err
	}
	raw := make([]byte, width+2)
	raw[0], raw[1] = family, address.prefix
	copy(raw[2:], address.address[:width])
	return model.BytesVal(raw), nil
}
