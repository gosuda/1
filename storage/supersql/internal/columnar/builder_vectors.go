package columnar

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// AppendVectors copies one selected lane per output column into owned buffers.
// A nil vector appends a typed NULL and ignores its lane. Non-nil vectors must
// match the complete output layout. Only selected payloads are read; no datum
// tree is materialized. Failure restores row lengths as Append does.
func (b *Builder) AppendVectors(ctx context.Context, vectors []*Vector, lanes []int) error {
	if b.closed {
		return ErrInvalidBatch
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vectors) != b.columns || len(lanes) != b.columns {
		return ErrValueLayout
	}
	if b.rows == b.maxRows {
		return ErrBatchBudget
	}
	b.saveMarks()
	for i, source := range vectors {
		if source != nil {
			if err := matchingVectorLayout(ctx, &b.nodes[i], source, 0); err != nil {
				b.restoreMarks()
				return err
			}
		}
		if err := b.appendVector(ctx, &b.nodes[i], source, lanes[i]); err != nil {
			b.restoreMarks()
			return err
		}
	}
	b.rows++
	return nil
}

func matchingVectorLayout(ctx context.Context, expected, actual *Vector, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 || expected.Type != actual.Type || len(expected.Children) != len(actual.Children) {
		return ErrValueLayout
	}
	for i := range expected.Children {
		if err := matchingVectorLayout(ctx, &expected.Children[i], &actual.Children[i], depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (b *Builder) appendVector(ctx context.Context, target, source *Vector, row int) error {
	if source == nil {
		return b.appendValue(ctx, target, model.NullVal(target.Type.Storage))
	}
	index, null, err := selectedIndex(source, row)
	if err != nil {
		return err
	}
	if null {
		return b.appendValue(ctx, target, model.NullVal(target.Type.Storage))
	}
	if err := b.appendNull(ctx, target, false); err != nil {
		return err
	}
	if err := b.appendVectorPayload(ctx, target, source, index); err != nil {
		return err
	}
	target.Length++
	return nil
}

func (b *Builder) appendVectorPayload(ctx context.Context, target, source *Vector, index int) error {
	switch target.Type.Storage {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		if index >= len(source.Int64) {
			return ErrInvalidVector
		}
		value := source.Int64[index]
		if target.Type.Storage == model.TypeBoolean && value != 0 && value != 1 {
			return ErrInvalidVector
		}
		return appendScalar(b, &target.Int64, value, false)
	case model.TypeFloat64:
		if index >= len(source.Float64) {
			return ErrInvalidVector
		}
		return appendScalar(b, &target.Float64, source.Float64[index], false)
	case model.TypeInterval:
		if index >= len(source.Intervals) {
			return ErrInvalidVector
		}
		return appendScalar(b, &target.Intervals, source.Intervals[index], false)
	case model.TypeBytes, model.TypeString, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		r, err := selectedRange(source.Ranges, index, len(source.Data))
		if err != nil {
			return err
		}
		copied, err := appendVectorBuffer(ctx, b, &target.Data, source.Data[r.Offset:uint64(r.Offset)+uint64(r.Length)])
		if err != nil {
			return err
		}
		return appendScalar(b, &target.Ranges, copied, false)
	case model.TypeVectorF32:
		r, err := selectedRange(source.Ranges, index, len(source.Float32))
		if err != nil {
			return err
		}
		copied, err := appendVectorBuffer(ctx, b, &target.Float32, source.Float32[r.Offset:uint64(r.Offset)+uint64(r.Length)])
		if err != nil {
			return err
		}
		return appendScalar(b, &target.Ranges, copied, false)
	case model.TypeArray:
		return b.appendVectorArray(ctx, target, source, index)
	case model.TypeComposite:
		for i := range target.Children {
			if err := b.appendVector(ctx, &target.Children[i], &source.Children[i], index); err != nil {
				return err
			}
		}
		return nil
	default:
		return ErrValueLayout
	}
}

func appendVectorBuffer[T any](ctx context.Context, b *Builder, target *[]T, source []T) (Range, error) {
	start := len(*target)
	if uint64(start)+uint64(len(source)) > math.MaxUint32 {
		return Range{}, ErrBatchBudget
	}
	if err := reserve(b, target, len(source)); err != nil {
		return Range{}, err
	}
	for offset := 0; offset < len(source); {
		if err := ctx.Err(); err != nil {
			return Range{}, err
		}
		end := offset + min(4096, len(source)-offset)
		copy((*target)[start+offset:start+end], source[offset:end])
		offset = end
	}
	return Range{Offset: uint32(start), Length: uint32(len(source))}, nil
}

func (b *Builder) appendVectorArray(ctx context.Context, target, source *Vector, index int) error {
	elements, err := selectedRange(source.Ranges, index, source.Children[0].Length)
	if err != nil {
		return err
	}
	shape, err := selectedRange(source.ShapeRanges, index, len(source.Dimensions))
	if err != nil {
		return err
	}
	dimensions := source.Dimensions[shape.Offset : uint64(shape.Offset)+uint64(shape.Length)]
	if err := model.ValidateArrayShape(dimensions, uint64(elements.Length)); err != nil {
		return err
	}
	start := target.Children[0].Length
	if uint64(start)+uint64(elements.Length) > math.MaxUint32 {
		return ErrBatchBudget
	}
	copied, err := appendVectorBuffer(ctx, b, &target.Dimensions, dimensions)
	if err != nil {
		return err
	}
	if err := appendScalar(b, &target.ShapeRanges, copied, false); err != nil {
		return err
	}
	if err := appendScalar(b, &target.Ranges, Range{Offset: uint32(start), Length: elements.Length}, false); err != nil {
		return err
	}
	for i := range int(elements.Length) {
		if err := b.appendVector(ctx, &target.Children[0], &source.Children[0], int(elements.Offset)+i); err != nil {
			return err
		}
	}
	return nil
}
