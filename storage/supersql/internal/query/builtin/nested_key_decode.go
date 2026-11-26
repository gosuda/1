package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (d *keyDecoder) nestedValue(ctx context.Context, column storage.KeyColumn, depth int, mask byte) (model.Value, error) {
	if depth > 64 {
		return model.Value{}, storage.ErrInvalidEncoding
	}
	if column.Type.ID == 30 {
		return d.oidVector(ctx, mask)
	}
	if column.Type.Storage == model.TypeComposite {
		count := uint64(len(column.Children))
		if count > d.remaining/uint64(unsafe.Sizeof(model.Value{})) {
			return model.Value{}, columnar.ErrBatchBudget
		}
		if err := d.charge(count * uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return model.Value{}, err
		}
		result := model.Value{Type: model.TypeComposite, ArrayVal: make([]model.Value, len(column.Children))}
		for i, child := range column.Children {
			value, err := d.field(ctx, storage.KeyColumn{Type: child.Type, Children: child.Children, Descending: mask != 0}, depth+1, true)
			if err != nil {
				return model.Value{}, err
			}
			result.ArrayVal[i] = value
		}
		return result, nil
	}
	child := column.Children[0]
	probe := *d
	count := uint64(0)
	for {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if probe.offset >= len(probe.data) {
			return model.Value{}, storage.ErrInvalidEncoding
		}
		if probe.data[probe.offset]^mask == 0 {
			break
		}
		if count >= d.remaining/uint64(unsafe.Sizeof(model.Value{})) || count >= math.MaxInt {
			return model.Value{}, columnar.ErrBatchBudget
		}
		if err := probe.skipNestedField(ctx, child, depth+1, mask); err != nil {
			return model.Value{}, err
		}
		count++
	}
	endElements := probe.offset
	probe.offset++
	dimensions, dimensionCount, err := probe.arrayDimensions(mask, count)
	if err != nil {
		return model.Value{}, err
	}
	bytes := count*uint64(unsafe.Sizeof(model.Value{})) + uint64(dimensionCount)*uint64(unsafe.Sizeof(model.ArrayDimension{}))
	if err := d.charge(bytes); err != nil {
		return model.Value{}, err
	}
	result := model.Value{Type: model.TypeArray, ArrayVal: make([]model.Value, int(count)), Dimensions: make([]model.ArrayDimension, dimensionCount)}
	copy(result.Dimensions, dimensions[:dimensionCount])
	for i := range result.ArrayVal {
		value, err := d.field(ctx, storage.KeyColumn{Type: child.Type, Children: child.Children, Descending: mask != 0}, depth+1, true)
		if err != nil {
			return model.Value{}, err
		}
		result.ArrayVal[i] = value
	}
	if d.offset != endElements {
		return model.Value{}, storage.ErrInvalidEncoding
	}
	d.offset = probe.offset
	return result, nil
}

func (d *keyDecoder) arrayDimensions(mask byte, elements uint64) ([6]model.ArrayDimension, int, error) {
	var dimensions [6]model.ArrayDimension
	count, err := d.number(4, mask)
	if err != nil {
		return dimensions, 0, err
	}
	if count > uint64(len(dimensions)) {
		return dimensions, 0, storage.ErrInvalidEncoding
	}
	for i := range int(count) {
		length, err := d.number(4, mask)
		if err != nil {
			return dimensions, 0, err
		}
		dimensions[i].Length = int32(length)
	}
	for i := range int(count) {
		lower, err := d.number(4, mask)
		if err != nil {
			return dimensions, 0, err
		}
		dimensions[i].LowerBound = int32(uint32(lower) ^ (uint32(1) << 31))
	}
	if err := model.ValidateArrayShape(dimensions[:count], elements); err != nil {
		return dimensions, 0, storage.ErrInvalidEncoding
	}
	return dimensions, int(count), nil
}

// The preflight scan counts array headers without allocating decoded children.
// Every payload is decoded and validated in the subsequent owned pass.
func (d *keyDecoder) skipNestedField(ctx context.Context, layout model.TypeLayout, depth int, mask byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return storage.ErrInvalidEncoding
	}
	marker, err := d.byte(mask)
	if err != nil {
		return err
	}
	if marker == 2 {
		return nil
	}
	if marker != 1 {
		return storage.ErrInvalidEncoding
	}
	if family, known := LookupRangeFamily(layout.Type.ID); known {
		_, _, err := d.transcodeRangeKey(ctx, family, layout.Type.ID == family.Multirange, mask, nil)
		return err
	}
	if layout.Type.ID == 30 {
		count, err := d.oidVectorCount(mask)
		if err != nil {
			return err
		}
		return d.skipBytes(int(count) * 9)
	}
	switch layout.Type.Storage {
	case model.TypeArray:
		count := uint64(0)
		for {
			if d.offset >= len(d.data) {
				return storage.ErrInvalidEncoding
			}
			if d.data[d.offset]^mask == 0 {
				d.offset++
				break
			}
			if err := d.skipNestedField(ctx, layout.Children[0], depth+1, mask); err != nil {
				return err
			}
			count++
		}
		_, _, err := d.arrayDimensions(mask, count)
		return err
	case model.TypeComposite:
		for _, child := range layout.Children {
			if err := d.skipNestedField(ctx, child, depth+1, mask); err != nil {
				return err
			}
		}
		return nil
	case model.TypeBoolean:
		return d.skipBytes(1)
	case model.TypeInt64, model.TypeFloat64:
		return d.skipBytes(8)
	case model.TypeTimestamp:
		return d.skipBytes(8)
	case model.TypeInterval, model.TypeUUID:
		return d.skipBytes(16)
	case model.TypeString:
		if layout.Type.ID == 18 {
			return d.skipBytes(1)
		}
		return d.skipEscaped(ctx, mask)
	case model.TypeBytes:
		return d.skipByteValue(ctx, layout.Type.ID, mask)
	case model.TypeJSONB:
		return d.skipJSONBKey(ctx, mask)
	case model.TypeNumeric:
		return d.skipNumeric(ctx, mask)
	default:
		return storage.ErrInvalidEncoding
	}
}

func (d *keyDecoder) skipBytes(count int) error {
	if count < 0 || count > len(d.data)-d.offset {
		return storage.ErrInvalidEncoding
	}
	d.offset += count
	return nil
}

func (d *keyDecoder) skipEscaped(ctx context.Context, mask byte) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, err := d.byte(mask)
		if err != nil {
			return err
		}
		if value != 0 {
			continue
		}
		escape, err := d.byte(mask)
		if err != nil {
			return err
		}
		if escape == 0 {
			return nil
		}
		if escape != 0xff {
			return storage.ErrInvalidEncoding
		}
	}
}

func (d *keyDecoder) skipByteValue(ctx context.Context, id model.TypeID, mask byte) error {
	switch id {
	case 1266:
		return d.skipBytes(12)
	case 774, 829:
		return d.skipBytes(macAddressSize(id))
	case 650, 869:
		family, err := d.byte(mask)
		if err != nil {
			return err
		}
		switch family {
		case 2:
			return d.skipBytes(12)
		case 3:
			return d.skipBytes(48)
		default:
			return storage.ErrInvalidEncoding
		}
	case 1560, 1562:
		if err := d.skipEscaped(ctx, mask); err != nil {
			return err
		}
		return d.skipBytes(4)
	default:
		return d.skipEscaped(ctx, mask)
	}
}

func (d *keyDecoder) skipNumeric(ctx context.Context, mask byte) error {
	category, err := d.byte(mask)
	if err != nil {
		return err
	}
	switch category {
	case numericKeyNegativeInfinity, numericKeyZero, numericKeyPositiveInfinity, numericKeyNaN:
		return nil
	case numericKeyNegative:
		mask ^= 0xff
	case numericKeyPositive:
	default:
		return storage.ErrInvalidEncoding
	}
	if err := d.skipBytes(4); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		digit, err := d.byte(mask)
		if err != nil {
			return err
		}
		if digit == 0 {
			return nil
		}
		if digit < '0' || digit > '9' {
			return storage.ErrInvalidEncoding
		}
	}
}
