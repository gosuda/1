package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileWidthBucketArray(element Callable, lookupErr error) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
		thresholds := &arguments[1]
		if err := model.ValidateArrayShape(thresholds.Dimensions, uint64(len(thresholds.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		if len(thresholds.Dimensions) > 1 {
			return model.Value{}, sqlError("2202E", "thresholds must be one-dimensional array")
		}
		for i := range len(thresholds.ArrayVal) {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			if thresholds.ArrayVal[i].IsNull {
				return model.Value{}, sqlError("22004", "thresholds array must not contain NULLs")
			}
		}
		if lookupErr != nil {
			return model.Value{}, lookupErr
		}

		var comparisonArguments [2]model.Value
		left, right := 0, len(thresholds.ArrayVal)
		for left < right {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			// PostgreSQL uses this probe order for both fixed- and variable-width elements.
			mid := left + (right-left)/2
			comparisonArguments[0], comparisonArguments[1] = arguments[0], thresholds.ArrayVal[mid]
			result, err := element.Call(ctx, comparisonArguments[:], callBudget.WithOutput(0))
			clear(comparisonArguments[:])
			if err != nil {
				return model.Value{}, err
			}
			if result.IsNull || result.Type != model.TypeInt64 || result.IntVal < math.MinInt32 || result.IntVal > math.MaxInt32 {
				return model.Value{}, sqlError("XX000", "array element comparison function did not return nonnull int4")
			}
			if result.IntVal < 0 {
				right = mid
			} else {
				left = mid + 1
			}
		}
		return model.Int64Val(int64(left)), nil
	}
}
