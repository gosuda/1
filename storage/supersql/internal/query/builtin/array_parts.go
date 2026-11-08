package builtin

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// copyArrayParts admits both segments before allocating one output header array.
// Dimensions and element payloads are borrowed until the owned copy completes.
func copyArrayParts(ctx context.Context, dimensions []model.ArrayDimension, first, second []model.Value, budget int64) (model.Value, error) {
	count := uint64(len(first)) + uint64(len(second))
	if err := model.ValidateArrayShape(dimensions, count); err != nil {
		return model.Value{}, sqlError("2202E", "invalid composed array dimensions")
	}
	limit := uint64(min(budget, arrayInputMaxBytes))
	used := uint64(0)
	for i, part := range [2][]model.Value{first, second} {
		view := model.Value{Type: model.TypeArray, ArrayVal: part}
		if i == 0 {
			view.Dimensions = dimensions
		}
		bytes, err := model.ValueBytes(ctx, view, limit-used)
		if err != nil {
			return model.Value{}, arrayPartsCopyError(err)
		}
		used += bytes
	}
	owned := model.Value{Type: model.TypeArray, Dimensions: make([]model.ArrayDimension, len(dimensions)), ArrayVal: make([]model.Value, int(count))}
	copy(owned.Dimensions, dimensions)
	remaining := used - count*uint64(arrayValueBytes) - uint64(len(dimensions))*uint64(arrayDimensionBytes)
	at := 0
	for _, part := range [2][]model.Value{first, second} {
		for _, value := range part {
			copied, bytes, err := model.CopyValue(ctx, value, remaining)
			if err != nil {
				return model.Value{}, arrayPartsCopyError(err)
			}
			owned.ArrayVal[at] = copied
			remaining -= bytes
			at++
		}
	}
	return owned, nil
}

func arrayPartsCopyError(err error) error {
	if errors.Is(err, model.ErrValueBudget) || errors.Is(err, model.ErrValueDepth) {
		return &model.Error{Code: "54000", Message: "composed array exceeds resource limit", Cause: err}
	}
	return err
}
