package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Array output capacity is bounded by limit; resizing temporarily holds at most
// two admitted header arrays. Element payloads move between them without copying.
type arrayValueBuffer struct {
	values         []model.Value
	payload, limit uint64
	dimensions     int
}

func newArrayValueBuffer(dimensionCount int, budget int64) arrayValueBuffer {
	return arrayValueBuffer{limit: uint64(min(budget, arrayInputMaxBytes)), dimensions: dimensionCount}
}

func (buffer *arrayValueBuffer) append(ctx context.Context, value model.Value) error {
	if value.IsNull {
		value = model.NullVal(value.Type)
	}
	width := uint64(arrayValueBytes)
	dimensions := uint64(buffer.dimensions) * uint64(arrayDimensionBytes)
	base := dimensions + uint64(len(buffer.values))*width + buffer.payload
	if base > buffer.limit {
		return arrayPartsCopyError(model.ErrValueBudget)
	}
	single := [1]model.Value{value}
	size, err := model.ValueBytes(ctx, model.Value{Type: model.TypeArray, ArrayVal: single[:]}, buffer.limit-base)
	if err != nil {
		return arrayPartsCopyError(err)
	}
	payload := size - width
	maximum := int((buffer.limit - dimensions - buffer.payload - payload) / width)
	if err := buffer.capacity(ctx, len(buffer.values)+1, maximum); err != nil {
		return err
	}
	owned, bytes, err := model.CopyValue(ctx, value, payload)
	if err != nil {
		return arrayPartsCopyError(err)
	}
	buffer.values = append(buffer.values, owned)
	buffer.payload += bytes
	return nil
}

func (buffer *arrayValueBuffer) capacity(ctx context.Context, required, maximum int) error {
	if required > maximum {
		return arrayPartsCopyError(model.ErrValueBudget)
	}
	old := buffer.values
	if cap(old) >= required && cap(old) <= maximum {
		return nil
	}
	capacity := min(max(required, 2*cap(old)), maximum)
	if cap(old) > maximum {
		capacity = required
	}
	values := make([]model.Value, len(old), capacity)
	if err := copyArrayValueHeaders(ctx, values, old); err != nil {
		return err
	}
	buffer.values = values
	return nil
}

func (buffer *arrayValueBuffer) finish(ctx context.Context, dimensions []model.ArrayDimension) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if len(buffer.values) == 0 {
		return model.Value{Type: model.TypeArray}, nil
	}
	if len(dimensions) != buffer.dimensions {
		return model.Value{}, sqlError("XX000", "array output dimension admission mismatch")
	}
	if err := model.ValidateArrayShape(dimensions, uint64(len(buffer.values))); err != nil {
		return model.Value{}, sqlError("2202E", "invalid array output dimensions")
	}
	if cap(buffer.values) != len(buffer.values) {
		compact := make([]model.Value, len(buffer.values))
		if err := copyArrayValueHeaders(ctx, compact, buffer.values); err != nil {
			return model.Value{}, err
		}
		buffer.values = compact
	}
	ownedDimensions := make([]model.ArrayDimension, len(dimensions))
	copy(ownedDimensions, dimensions)
	output := model.Value{Type: model.TypeArray, Dimensions: ownedDimensions, ArrayVal: buffer.values}
	buffer.values = nil
	return output, nil
}

func copyArrayValueHeaders(ctx context.Context, destination, source []model.Value) error {
	for start := 0; start < len(source); start += 256 {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+256, len(source))
		copy(destination[start:end], source[start:end])
	}
	return ctx.Err()
}
