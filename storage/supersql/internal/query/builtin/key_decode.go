package builtin

import (
	"bytes"
	"context"
	"math"
	"strconv"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// DecodePrefix returns canonical key representatives, not original datum spelling
// or bpchar padding. Encoding the result with the same layout reproduces key.
// maxBytes bounds returned headers/payload and decoding/round-trip buffers. The
// existing bounded numeric codec retains its independent numeric-work limit.
func (e *KeyEncoder) DecodePrefix(ctx context.Context, layout []storage.KeyColumn, key []byte, maxBytes uint64) ([]model.Value, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := e.EncodePrefix(ctx, layout, nil, maxBytes); err != nil {
		return nil, err
	}
	capacity := min(len(layout), len(key))
	headers := uint64(capacity) * uint64(unsafe.Sizeof(model.Value{}))
	if headers > maxBytes || uint64(len(key)) > maxBytes-headers {
		return nil, columnar.ErrBatchBudget
	}
	decoder := keyDecoder{data: key, remaining: maxBytes - headers - uint64(len(key)), encoder: e}
	values := make([]model.Value, 0, capacity)
	for _, column := range layout {
		if decoder.offset == len(key) {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value, err := decoder.value(ctx, column)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if decoder.offset != len(key) {
		return nil, storage.ErrInvalidEncoding
	}
	encoded, err := e.EncodePrefix(ctx, layout, values, uint64(len(key)))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(encoded, key) {
		return nil, storage.ErrInvalidEncoding
	}
	return values, nil
}

type keyDecoder struct {
	data      []byte
	offset    int
	remaining uint64
	encoder   *KeyEncoder
}

func (d *keyDecoder) charge(bytes uint64) error {
	if bytes > d.remaining {
		return columnar.ErrBatchBudget
	}
	d.remaining -= bytes
	return nil
}
func (d *keyDecoder) byte(mask byte) (byte, error) {
	if d.offset == len(d.data) {
		return 0, storage.ErrInvalidEncoding
	}
	value := d.data[d.offset] ^ mask
	d.offset++
	return value, nil
}
func (d *keyDecoder) number(width int, mask byte) (uint64, error) {
	if width > len(d.data)-d.offset {
		return 0, storage.ErrInvalidEncoding
	}
	var value uint64
	for range width {
		value = value<<8 | uint64(d.data[d.offset]^mask)
		d.offset++
	}
	return value, nil
}
func (d *keyDecoder) value(ctx context.Context, column storage.KeyColumn) (model.Value, error) {
	return d.field(ctx, column, 0, false)
}

func (d *keyDecoder) field(ctx context.Context, column storage.KeyColumn, depth int, nested bool) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if depth > 64 {
		return model.Value{}, storage.ErrInvalidEncoding
	}
	mask := byte(0)
	if column.Descending {
		mask = 0xff
	}
	markerMask := byte(0)
	if nested {
		markerMask = mask
	}
	marker, err := d.byte(markerMask)
	if err != nil {
		return model.Value{}, err
	}
	null := byte(2)
	if column.NullsFirst && !nested {
		null = 0
	}
	if marker == null {
		return model.NullVal(column.Type.Storage), nil
	}
	if marker != 1 {
		return model.Value{}, storage.ErrInvalidEncoding
	}
	layout, declared := d.encoder.keyLayout(model.TypeLayout{Type: column.Type, Children: column.Children})
	column.Type, column.Children = layout.Type, layout.Children
	if nestedKeyType(column.Type) {
		return d.nestedValue(ctx, column, depth, mask)
	}
	if family, known := d.encoder.rangeFamily(column.Type.ID); known {
		return d.rangeKeyValue(ctx, family, column.Type.ID == family.Multirange, mask)
	}
	if declared != nil && declared.Kind() == model.TypeKindEnum {
		encoded, err := d.number(8, mask)
		if err != nil {
			return model.Value{}, err
		}
		order := int64(encoded ^ (uint64(1) << 63))
		if order < 0 || order >= int64(len(declared.metadata.ElementOIDs)) {
			return model.Value{}, storage.ErrInvalidEncoding
		}
		return model.Int64Val(int64(declared.metadata.ElementOIDs[order])), nil
	}
	if column.Type.ID == 3802 {
		return d.jsonbValue(ctx, mask)
	}
	if structuredByteKey(column.Type.ID) {
		return d.structuredBytes(ctx, column.Type.ID, mask)
	}
	if column.Type.ID == 18 {
		character, err := d.byte(mask)
		if err != nil {
			return model.Value{}, err
		}
		if character == 0 {
			return model.StringVal(""), nil
		}
		if err := d.charge(1); err != nil {
			return model.Value{}, err
		}
		return model.StringVal(string([]byte{character})), nil
	}
	if column.Type.ID == 1266 {
		return d.timetz(mask)
	}
	value := model.Value{Type: column.Type.Storage}
	switch column.Type.Storage {
	case model.TypeInterval:
		interval, err := d.interval(mask)
		if err != nil {
			return model.Value{}, err
		}
		value.IntervalVal = interval
	case model.TypeBoolean:
		bit, err := d.byte(mask)
		if err != nil {
			return model.Value{}, err
		}
		if bit > 1 {
			return model.Value{}, storage.ErrInvalidEncoding
		}
		value.IntVal = int64(bit)
	case model.TypeInt64, model.TypeFloat64, model.TypeTimestamp:
		encoded, err := d.number(8, mask)
		if err != nil {
			return model.Value{}, err
		}
		if column.Type.Storage != model.TypeFloat64 {
			value.IntVal = int64(encoded ^ (uint64(1) << 63))
			break
		}
		if encoded == math.MaxUint64 {
			value.FloatVal = math.NaN()
			break
		}
		if encoded>>63 != 0 {
			encoded ^= uint64(1) << 63
		} else {
			encoded = ^encoded
		}
		value.FloatVal = math.Float64frombits(encoded)
	case model.TypeString, model.TypeBytes:
		raw, err := d.escaped(ctx, mask, column.Type.Storage == model.TypeString, 0)
		if err != nil {
			return model.Value{}, err
		}
		if column.Type.Storage == model.TypeString {
			value.StrVal = string(raw)
		} else {
			value.BytesVal = raw
		}
	case model.TypeNumeric:
		text, err := d.numeric(ctx, mask)
		if err != nil {
			return model.Value{}, err
		}
		value.StrVal = text
	case model.TypeUUID:
		var data [16]byte
		if len(data) > len(d.data)-d.offset {
			return model.Value{}, storage.ErrInvalidEncoding
		}
		if err := d.charge(36); err != nil {
			return model.Value{}, err
		}
		for i := range data {
			data[i] = d.data[d.offset+i] ^ mask
		}
		d.offset += len(data)
		value.StrVal = formatUUID(data)
	default:
		return model.Value{}, &MissingKeyImplementationError{Type: column.Type}
	}
	return value, nil
}
func (d *keyDecoder) escaped(ctx context.Context, mask byte, text bool, prefix int) ([]byte, error) {
	start, scan, count := d.offset, d.offset, 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if scan >= len(d.data) {
			return nil, storage.ErrInvalidEncoding
		}
		value := d.data[scan] ^ mask
		scan++
		if value != 0 {
			count++
			continue
		}
		if scan >= len(d.data) {
			return nil, storage.ErrInvalidEncoding
		}
		escape := d.data[scan] ^ mask
		scan++
		if escape == 0 {
			break
		}
		if escape != 0xff {
			return nil, storage.ErrInvalidEncoding
		}
		count++
	}
	if prefix < 0 || count > math.MaxInt-prefix {
		return nil, columnar.ErrBatchBudget
	}
	copies := uint64(1)
	if text {
		copies = 2
	}
	if err := d.charge(uint64(count+prefix) * copies); err != nil {
		return nil, err
	}
	result := make([]byte, count+prefix)
	cursor := start
	for i := range count {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value := d.data[cursor] ^ mask
		cursor++
		if value == 0 {
			cursor++
		}
		result[prefix+i] = value
	}
	d.offset = scan
	return result, nil
}
func (d *keyDecoder) numeric(ctx context.Context, mask byte) (string, error) {
	category, err := d.byte(mask)
	if err != nil {
		return "", err
	}
	switch category {
	case numericKeyNegativeInfinity:
		return "-Infinity", nil
	case numericKeyZero:
		return "0", nil
	case numericKeyPositiveInfinity:
		return "Infinity", nil
	case numericKeyNaN:
		return "NaN", nil
	case numericKeyNegative:
		mask ^= 0xff
	case numericKeyPositive:
	default:
		return "", storage.ErrInvalidEncoding
	}
	exponent, err := d.number(4, mask)
	if err != nil {
		return "", err
	}
	position := int64(int32(uint32(exponent) ^ (uint32(1) << 31)))
	start := d.offset
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		digit, err := d.byte(mask)
		if err != nil {
			return "", err
		}
		if digit == 0 {
			break
		}
		if digit < '0' || digit > '9' {
			return "", storage.ErrInvalidEncoding
		}
	}
	digits := d.offset - start - 1
	if digits == 0 {
		return "", storage.ErrInvalidEncoding
	}
	size := uint64(digits) + 24
	if err := d.charge(size * 2); err != nil {
		return "", err
	}
	text := make([]byte, 0, int(size))
	if category == numericKeyNegative {
		text = append(text, '-')
	}
	for i := range digits {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		text = append(text, d.data[start+i]^mask)
	}
	text = append(text, 'e')
	text = strconv.AppendInt(text, position-int64(digits), 10)
	return string(text), nil
}

// DecodeKey requires a complete key, unlike DecodePrefix's leading tuple.
func (e *KeyEncoder) DecodeKey(ctx context.Context, layout []storage.KeyColumn, key []byte, maxBytes uint64) ([]model.Value, error) {
	values, err := e.DecodePrefix(ctx, layout, key, maxBytes)
	if err != nil {
		return nil, err
	}
	if len(values) != len(layout) {
		return nil, storage.ErrInvalidEncoding
	}
	return values, nil
}
