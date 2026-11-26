package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func resolveArrayReverse(signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions) (Callable, error) {
	if _, _, err := arrayCodecLayout(result, options); err != nil {
		return Callable{}, err
	}
	if !sameArrayConstructionLayout(arguments[0], result) {
		return Callable{}, sqlError("42804", "array reverse result differs from its input layout")
	}
	signature.Arguments[0], signature.Result = arguments[0].Type.ID, result.Type.ID
	return Callable{signature: signature, kernel: reverseArray, resultStorage: model.TypeArray, argumentStorage: [8]model.DataType{model.TypeArray}}, nil
}

func reverseArray(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	input := arguments[0]
	if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil {
		return model.Value{}, sqlError("2202E", "invalid array dimensions")
	}
	rows := 0
	if len(input.Dimensions) != 0 {
		rows = int(input.Dimensions[0].Length)
	}
	if rows > 1 {
		for _, dimension := range input.Dimensions {
			if int64(dimension.LowerBound)+int64(dimension.Length) > math.MaxInt32 {
				return model.Value{}, sqlError("54000", "array lower bound is too large")
			}
		}
	}
	owned, err := copyArrayParts(ctx, input.Dimensions, input.ArrayVal, nil, budget)
	if err != nil {
		return model.Value{}, err
	}
	if rows < 2 {
		return owned, nil
	}
	stride := len(owned.ArrayVal) / rows
	for row := range rows / 2 {
		left, right := row*stride, (rows-row-1)*stride
		for start := 0; start < stride; start += 256 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			for offset := range min(256, stride-start) {
				i, j := left+start+offset, right+start+offset
				owned.ArrayVal[i], owned.ArrayVal[j] = owned.ArrayVal[j], owned.ArrayVal[i]
			}
		}
	}
	return owned, nil
}
