package builtin

import (
	"context"
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func concatenateArrays(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	for _, input := range arguments {
		if !input.IsNull {
			if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil {
				return model.Value{}, sqlError("2202E", "invalid array dimensions")
			}
		}
	}
	first, second := arguments[0], arguments[1]
	if first.IsNull && second.IsNull {
		return model.NullVal(model.TypeArray), nil
	}
	if first.IsNull {
		return copyArrayParts(ctx, second.Dimensions, second.ArrayVal, nil, budget)
	}
	if second.IsNull || len(second.Dimensions) == 0 {
		return copyArrayParts(ctx, first.Dimensions, first.ArrayVal, nil, budget)
	}

	if len(first.Dimensions) == 0 {
		return copyArrayParts(ctx, second.Dimensions, second.ArrayVal, nil, budget)
	}
	firstRank, secondRank := len(first.Dimensions), len(second.Dimensions)
	var scratch [6]model.ArrayDimension
	dimensions := scratch[:max(firstRank, secondRank)]
	var firstInner, secondInner []model.ArrayDimension
	growth := int64(1)
	switch {
	case firstRank == secondRank:
		copy(dimensions, first.Dimensions)
		firstInner, secondInner = first.Dimensions[1:], second.Dimensions[1:]
		growth = int64(second.Dimensions[0].Length)
	case firstRank+1 == secondRank:
		copy(dimensions, second.Dimensions)
		firstInner, secondInner = first.Dimensions, second.Dimensions[1:]
	case firstRank == secondRank+1:
		copy(dimensions, first.Dimensions)
		firstInner, secondInner = first.Dimensions[1:], second.Dimensions
	default:
		return model.Value{}, sqlError("2202E", "cannot concatenate incompatible arrays")
	}
	if !slices.Equal(firstInner, secondInner) {
		return model.Value{}, sqlError("2202E", "cannot concatenate incompatible arrays")
	}
	length := int64(dimensions[0].Length) + growth
	if length > math.MaxInt32 {
		return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
	}
	dimensions[0].Length = int32(length)
	for _, dimension := range dimensions {
		if int64(dimension.LowerBound)+int64(dimension.Length) > math.MaxInt32 {
			return model.Value{}, sqlError("54000", "array lower bound is too large")
		}
	}
	return copyArrayParts(ctx, dimensions, first.ArrayVal, second.ArrayVal, budget)
}
