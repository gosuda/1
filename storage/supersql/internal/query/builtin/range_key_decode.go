package builtin

import (
	"cmp"
	"context"
	"encoding/binary"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type rangeKeyOutput struct {
	data    []byte
	size    uint64
	maximum uint64
}

func (out *rangeKeyOutput) grow(count uint64) (int, error) {
	if count > out.maximum-out.size {
		return 0, columnar.ErrBatchBudget
	}
	start := int(out.size)
	out.size += count
	return start, nil
}

func (d *keyDecoder) rangeKeyValue(ctx context.Context, family RangeFamilyInfo, multi bool, mask byte) (model.Value, error) {
	probe := *d
	_, size, err := probe.transcodeRangeKey(ctx, family, multi, mask, nil)
	if err != nil {
		return model.Value{}, err
	}
	if err := d.charge(size); err != nil {
		return model.Value{}, err
	}
	output := make([]byte, int(size))
	remaining := d.remaining
	_, _, err = d.transcodeRangeKey(ctx, family, multi, mask, output)
	d.remaining = remaining
	if err != nil {
		return model.Value{}, err
	}
	return model.Value{Type: model.TypeBytes, BytesVal: output}, nil
}

func (d *keyDecoder) transcodeRangeKey(ctx context.Context, family RangeFamilyInfo, multi bool, mask byte, output []byte) ([]byte, uint64, error) {
	maximum := d.remaining
	if output != nil {
		maximum = uint64(len(output))
	}
	out := rangeKeyOutput{data: output, maximum: min(maximum, uint64(math.MaxInt))}
	if !multi {
		err := d.transcodeRangeKeyDatum(ctx, family, mask, &out, false)
		return output, out.size, err
	}
	if _, err := out.grow(4); err != nil {
		return nil, 0, err
	}
	var count uint32
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		marker, err := d.byte(mask)
		if err != nil {
			return nil, 0, err
		}
		if marker == 0 {
			break
		}
		if marker != 1 || count == math.MaxInt32 {
			return nil, 0, storage.ErrInvalidEncoding
		}
		lengthOffset, err := out.grow(4)
		if err != nil {
			return nil, 0, err
		}
		start := out.size
		if err := d.transcodeRangeKeyDatum(ctx, family, mask, &out, true); err != nil {
			return nil, 0, err
		}
		if out.size-start > math.MaxUint32 {
			return nil, 0, storage.ErrInvalidEncoding
		}
		if output != nil {
			binary.BigEndian.PutUint32(output[lengthOffset:], uint32(out.size-start))
		}
		count++
	}
	if output != nil {
		binary.BigEndian.PutUint32(output, count)
	}
	return output, out.size, nil
}

func (d *keyDecoder) transcodeRangeKeyDatum(ctx context.Context, family RangeFamilyInfo, mask byte, out *rangeKeyOutput, nonempty bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	marker, err := d.byte(mask)
	if err != nil {
		return err
	}
	if marker > 1 || (nonempty && marker == 0) {
		return storage.ErrInvalidEncoding
	}
	flagOffset, err := out.grow(1)
	if err != nil {
		return err
	}
	if marker == 0 {
		if out.data != nil {
			out.data[flagOffset] = rangeEmptyFlag
		}
		return nil
	}
	var flags byte
	for i := range 2 {
		marker, err := d.byte(mask)
		if err != nil {
			return err
		}
		if marker > 1 {
			return storage.ErrInvalidEncoding
		}
		infinite := (i == 0 && marker == 0) || (i == 1 && marker == 1)
		if infinite {
			if i == 0 {
				flags |= rangeLowerInfiniteFlag
			} else {
				flags |= rangeUpperInfiniteFlag
			}
			continue
		}
		if family.subtypeStorage() == model.TypeNumeric {
			if err := d.transcodeRangeNumeric(ctx, mask, out); err != nil {
				return err
			}
		} else if err := d.transcodeRangeBound(ctx, family, mask, out); err != nil {
			return err
		}
		inclusive, err := d.byte(mask)
		if err != nil {
			return err
		}
		if inclusive > 1 {
			return storage.ErrInvalidEncoding
		}
		if i == 0 && inclusive == 0 {
			flags |= rangeLowerInclusiveFlag
		}
		if i == 1 && inclusive == 1 {
			flags |= rangeUpperInclusiveFlag
		}
	}
	if out.data != nil {
		out.data[flagOffset] = flags
	}
	return nil
}

func (d *keyDecoder) transcodeRangeBound(ctx context.Context, family RangeFamilyInfo, mask byte, out *rangeKeyOutput) error {
	var value model.Value
	if family.enum != nil {
		bits, err := d.number(8, mask)
		if err != nil {
			return err
		}
		order := int64(bits ^ (uint64(1) << 63))
		if order < 0 || order >= int64(len(family.enum.metadata.ElementOIDs)) {
			return storage.ErrInvalidEncoding
		}
		value = model.Int64Val(int64(family.enum.metadata.ElementOIDs[order]))
	} else {
		kind := family.subtypeStorage()
		value.Type = kind
		switch kind {
		case model.TypeBoolean:
			bit, err := d.byte(mask)
			if err != nil || bit > 1 {
				if err != nil {
					return err
				}
				return storage.ErrInvalidEncoding
			}
			value.IntVal = int64(bit)
		case model.TypeInt64, model.TypeTimestamp:
			bits, err := d.number(8, mask)
			if err != nil {
				return err
			}
			value.IntVal = int64(bits ^ (uint64(1) << 63))
		case model.TypeFloat64:
			bits, err := d.number(8, mask)
			if err != nil {
				return err
			}
			if bits == math.MaxUint64 {
				value.FloatVal = math.NaN()
			} else {
				if bits>>63 != 0 {
					bits ^= uint64(1) << 63
				} else {
					bits = ^bits
				}
				value.FloatVal = math.Float64frombits(bits)
			}
		case model.TypeString:
			text, err := d.escaped(ctx, mask, true, 0)
			if err != nil {
				return err
			}
			value.StrVal = string(text)
		case model.TypeBytes:
			bytes, err := d.escaped(ctx, mask, false, 0)
			if err != nil {
				return err
			}
			value.BytesVal = bytes
		case model.TypeJSONB:
			decoded, err := d.jsonbValue(ctx, mask)
			if err != nil {
				return err
			}
			value = decoded
		case model.TypeUUID:
			var data [16]byte
			if len(data) > len(d.data)-d.offset {
				return storage.ErrInvalidEncoding
			}
			if err := d.charge(36); err != nil {
				return err
			}
			for i := range data {
				data[i] = d.data[d.offset+i] ^ mask
			}
			d.offset += len(data)
			value.StrVal = formatUUID(data)
		case model.TypeInterval:
			interval, err := d.interval(mask)
			if err != nil {
				return err
			}
			value.IntervalVal = interval
		default:
			return &MissingKeyImplementationError{Type: model.Type{ID: family.representation(), Storage: kind}}
		}
	}
	bound, err := rangeBoundFromValue(family, value, rangeLower, false)
	if err != nil {
		return err
	}
	switch family.subtypeStorage() {
	case model.TypeString, model.TypeUUID:
		if uint64(len(bound.numeric)) > math.MaxUint32 {
			return storage.ErrInvalidEncoding
		}
		start, err := out.grow(4 + uint64(len(bound.numeric)))
		if err != nil || out.data == nil {
			return err
		}
		binary.BigEndian.PutUint32(out.data[start:], uint32(len(bound.numeric)))
		copy(out.data[start+4:], bound.numeric)
	case model.TypeBytes, model.TypeJSONB:
		if uint64(len(bound.bytes)) > math.MaxUint32 {
			return storage.ErrInvalidEncoding
		}
		start, err := out.grow(4 + uint64(len(bound.bytes)))
		if err != nil || out.data == nil {
			return err
		}
		binary.BigEndian.PutUint32(out.data[start:], uint32(len(bound.bytes)))
		copy(out.data[start+4:], bound.bytes)
	case model.TypeFloat64:
		start, err := out.grow(8)
		if err != nil || out.data == nil {
			return err
		}
		binary.BigEndian.PutUint64(out.data[start:], math.Float64bits(bound.floating))
	case model.TypeInterval:
		start, err := out.grow(16)
		if err != nil || out.data == nil {
			return err
		}
		binary.BigEndian.PutUint32(out.data[start:], uint32(bound.interval.Months))
		binary.BigEndian.PutUint32(out.data[start+4:], uint32(bound.interval.Days))
		binary.BigEndian.PutUint64(out.data[start+8:], uint64(bound.interval.Microseconds))
	default:
		start, err := out.grow(8)
		if err != nil || out.data == nil {
			return err
		}
		binary.BigEndian.PutUint64(out.data[start:], uint64(bound.integer))
	}
	return ctx.Err()
}

func (d *keyDecoder) transcodeRangeNumeric(ctx context.Context, mask byte, out *rangeKeyOutput) error {
	category, err := d.byte(mask)
	if err != nil {
		return err
	}
	var special string
	switch category {
	case numericKeyNegativeInfinity:
		special = "-Infinity"
	case numericKeyZero:
		special = "0"
	case numericKeyPositiveInfinity:
		special = "Infinity"
	case numericKeyNaN:
		special = "NaN"
	case numericKeyNegative:
		mask ^= 0xff
	case numericKeyPositive:
	default:
		return storage.ErrInvalidEncoding
	}
	if special != "" {
		start, err := out.grow(4 + uint64(len(special)))
		if err != nil {
			return err
		}
		if out.data != nil {
			binary.BigEndian.PutUint32(out.data[start:], uint32(len(special)))
			copy(out.data[start+4:], special)
		}
		return nil
	}
	exponent, err := d.number(4, mask)
	if err != nil {
		return err
	}
	position := int64(int32(uint32(exponent) ^ (uint32(1) << 31)))
	start := d.offset
	var first, last byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		digit, err := d.byte(mask)
		if err != nil {
			return err
		}
		if digit == 0 {
			break
		}
		if digit < '0' || digit > '9' {
			return storage.ErrInvalidEncoding
		}
		first = cmp.Or(first, digit)
		last = digit
	}
	digits := d.offset - start - 1
	if digits == 0 || first == '0' || last == '0' {
		return storage.ErrInvalidEncoding
	}
	var exponentBuffer [24]byte
	exponentText := strconv.AppendInt(exponentBuffer[:0], position-int64(digits), 10)
	size := uint64(digits) + 1 + uint64(len(exponentText))
	negative := category == numericKeyNegative
	if negative {
		size++
	}
	if size > math.MaxUint32 {
		return storage.ErrInvalidEncoding
	}
	offset, err := out.grow(4 + size)
	if err != nil {
		return err
	}
	if out.data == nil {
		return nil
	}
	binary.BigEndian.PutUint32(out.data[offset:], uint32(size))
	offset += 4
	if negative {
		out.data[offset] = '-'
		offset++
	}
	for i := range digits {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		out.data[offset+i] = d.data[start+i] ^ mask
	}
	offset += digits
	out.data[offset] = 'e'
	copy(out.data[offset+1:], exponentText)
	return nil
}
