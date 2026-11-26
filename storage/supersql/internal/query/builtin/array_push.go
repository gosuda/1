package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func appendArray(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	input := arguments[0]
	dimensions := [1]model.ArrayDimension{{LowerBound: 1, Length: 1}}
	var elements []model.Value
	if !input.IsNull {
		if len(input.Dimensions) > 1 {
			return model.Value{}, sqlError("22000", "argument must be empty or one-dimensional array")
		}
		if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		elements = input.ArrayVal
		if len(input.Dimensions) == 1 {
			dimension := input.Dimensions[0]
			if int64(dimension.LowerBound)+int64(dimension.Length) > math.MaxInt32 {
				return model.Value{}, sqlError("22003", "integer out of range")
			}
			if dimension.Length == math.MaxInt32 || int64(dimension.LowerBound)+int64(dimension.Length)+1 > math.MaxInt32 {
				return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
			}
			dimension.Length++
			dimensions[0] = dimension
		}
	}
	inserted := [1]model.Value{arguments[1]}
	return copyArrayParts(ctx, dimensions[:], elements, inserted[:], budget)
}

func prependArray(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	input := arguments[1]
	dimensions := [1]model.ArrayDimension{{LowerBound: 1, Length: 1}}
	var elements []model.Value
	if !input.IsNull {
		if len(input.Dimensions) > 1 {
			return model.Value{}, sqlError("22000", "argument must be empty or one-dimensional array")
		}
		if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		elements = input.ArrayVal
		if len(input.Dimensions) == 1 {
			dimension := input.Dimensions[0]
			// PostgreSQL inserts at LB-1 before restoring the original lower bound.
			if dimension.LowerBound == math.MinInt32 {
				return model.Value{}, sqlError("22003", "integer out of range")
			}
			// The insertion at LB-1 checks one-past-last before rebasing to LB.
			if dimension.Length == math.MaxInt32 || int64(dimension.LowerBound)+int64(dimension.Length) > math.MaxInt32 {
				return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
			}
			dimension.Length++
			dimensions[0] = dimension
		}
	}
	inserted := [1]model.Value{arguments[0]}
	return copyArrayParts(ctx, dimensions[:], inserted[:], elements, budget)
}
