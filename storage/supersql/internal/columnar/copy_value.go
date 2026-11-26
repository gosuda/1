package columnar

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// CopyValue owns one selected value without materializing a batch of rows.
// The budget covers copied payloads and nested value/dimension headers, excluding
// the borrowed vector and returned Value header. Only the selected lane is read.
func CopyValue(ctx context.Context, vector *Vector, row int, maxBytes uint64) (model.Value, uint64, error) {
	state := valueCopyBudget{limit: maxBytes}
	value, err := state.copy(ctx, vector, row, 0)
	if err != nil {
		return model.Value{}, 0, err
	}
	return value, state.used, nil
}

// ValueBytes measures the allocations CopyValue needs without copying payloads.
func ValueBytes(ctx context.Context, vector *Vector, row int, maxBytes uint64) (uint64, error) {
	state := valueCopyBudget{limit: maxBytes, measure: true}
	_, err := state.copy(ctx, vector, row, 0)
	return state.used, err
}

type valueCopyBudget struct {
	limit, used uint64
	measure     bool
}

func (b *valueCopyBudget) reserve(count, size uint64) error {
	if size != 0 && count > (b.limit-b.used)/size {
		return ErrBatchBudget
	}
	b.used += count * size
	return nil
}

func (b *valueCopyBudget) copy(ctx context.Context, v *Vector, row, depth int) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if depth > 64 {
		return model.Value{}, ErrInvalidVector
	}
	index, null, err := selectedIndex(v, row)
	if err != nil {
		return model.Value{}, err
	}
	value := model.Value{Type: v.Type.Storage, IsNull: null}
	if null {
		return value, nil
	}
	switch value.Type {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		if index >= len(v.Int64) {
			return model.Value{}, ErrInvalidVector
		}
		value.IntVal = v.Int64[index]
		if value.Type == model.TypeBoolean && value.IntVal != 0 && value.IntVal != 1 {
			return model.Value{}, ErrInvalidVector
		}
	case model.TypeFloat64:
		if index >= len(v.Float64) {
			return model.Value{}, ErrInvalidVector
		}
		value.FloatVal = v.Float64[index]
	case model.TypeInterval:
		if index >= len(v.Intervals) {
			return model.Value{}, ErrInvalidVector
		}
		value.IntervalVal = v.Intervals[index]
	case model.TypeBytes, model.TypeString, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		r, err := selectedRange(v.Ranges, index, len(v.Data))
		if err != nil {
			return model.Value{}, err
		}
		if err := b.reserve(uint64(r.Length), 1); err != nil {
			return model.Value{}, err
		}
		if b.measure {
			break
		}
		data := v.Data[r.Offset : uint64(r.Offset)+uint64(r.Length)]
		if value.Type == model.TypeBytes {
			value.BytesVal = make([]byte, len(data))
			copy(value.BytesVal, data)
		} else {
			value.StrVal = string(data)
		}
	case model.TypeVectorF32:
		r, err := selectedRange(v.Ranges, index, len(v.Float32))
		if err != nil {
			return model.Value{}, err
		}
		if err := b.reserve(uint64(r.Length), 4); err != nil {
			return model.Value{}, err
		}
		if b.measure {
			break
		}
		value.VectorVal = make([]float32, int(r.Length))
		copy(value.VectorVal, v.Float32[r.Offset:uint64(r.Offset)+uint64(r.Length)])
	case model.TypeArray:
		return b.copyArray(ctx, v, index, depth)
	case model.TypeComposite:
		if err := b.reserve(uint64(len(v.Children)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return model.Value{}, err
		}
		if !b.measure {
			value.ArrayVal = make([]model.Value, len(v.Children))
		}
		for i := range v.Children {
			field, err := b.copy(ctx, &v.Children[i], index, depth+1)
			if err != nil {
				return model.Value{}, err
			}
			if !b.measure {
				value.ArrayVal[i] = field
			}
		}
	default:
		return model.Value{}, ErrInvalidVector
	}
	return value, nil
}

func (b *valueCopyBudget) copyArray(ctx context.Context, v *Vector, index, depth int) (model.Value, error) {
	if len(v.Children) != 1 {
		return model.Value{}, ErrInvalidVector
	}
	elements, err := selectedRange(v.Ranges, index, v.Children[0].Length)
	if err != nil {
		return model.Value{}, err
	}
	shape, err := selectedRange(v.ShapeRanges, index, len(v.Dimensions))
	if err != nil {
		return model.Value{}, err
	}
	dimensions := v.Dimensions[shape.Offset : uint64(shape.Offset)+uint64(shape.Length)]
	if err := model.ValidateArrayShape(dimensions, uint64(elements.Length)); err != nil {
		return model.Value{}, err
	}
	if err := b.reserve(uint64(elements.Length), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return model.Value{}, err
	}
	if err := b.reserve(uint64(shape.Length), uint64(unsafe.Sizeof(model.ArrayDimension{}))); err != nil {
		return model.Value{}, err
	}
	value := model.Value{Type: model.TypeArray}
	if !b.measure {
		value.Dimensions = make([]model.ArrayDimension, len(dimensions))
		value.ArrayVal = make([]model.Value, int(elements.Length))
		copy(value.Dimensions, dimensions)
	}
	for i := range int(elements.Length) {
		element, err := b.copy(ctx, &v.Children[0], int(elements.Offset)+i, depth+1)
		if err != nil {
			return model.Value{}, err
		}
		if !b.measure {
			value.ArrayVal[i] = element
		}
	}
	return value, nil
}

func selectedRange(ranges []Range, index, size int) (Range, error) {
	if index < 0 || index >= len(ranges) || size < 0 {
		return Range{}, ErrInvalidVector
	}
	r := ranges[index]
	if uint64(r.Offset)+uint64(r.Length) > uint64(size) {
		return Range{}, ErrInvalidVector
	}
	return r, nil
}

func selectedIndex(v *Vector, row int) (int, bool, error) {
	if v == nil || row < 0 {
		return 0, false, ErrInvalidVector
	}
	if row >= v.Length || v.Offset < 0 || v.Offset > math.MaxInt-row || v.Type.ID == 0 || !v.Type.Storage.Valid() {
		return 0, false, ErrInvalidVector
	}
	index := v.Offset + row
	if v.Encoding == Constant {
		if v.Offset != 0 {
			return 0, false, ErrInvalidVector
		}
		index = 0
	} else if v.Encoding != Flat && v.Encoding != Dictionary {
		return 0, false, ErrInvalidVector
	}
	if len(v.Nulls) != 0 {
		if index/8 >= len(v.Nulls) {
			return 0, false, ErrInvalidVector
		}
		if v.Nulls[index/8]&(1<<uint(index%8)) != 0 {
			return index, true, nil
		}
	}
	if v.Encoding == Dictionary {
		if index >= len(v.Indices) || uint64(v.Indices[index]) > math.MaxInt {
			return 0, false, ErrInvalidVector
		}
		index = int(v.Indices[index])
	}
	return index, false, nil
}
