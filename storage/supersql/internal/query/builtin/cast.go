package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileCast(signature Signature) scalarKernel {
	from, to := signature.Arguments[0], signature.Result
	return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		value := args[0]
		if to == 16 {
			return model.BoolVal(value.IntVal != 0), nil
		}
		if isInteger(to) {
			if isFloat(from) {
				return floatToInteger(value.FloatVal, to)
			}
			if err := integerRange(value.IntVal, to); err != nil {
				return model.Value{}, err
			}
			return model.Int64Val(value.IntVal), nil
		}
		if isInteger(from) {
			if to == 700 {
				return model.Float64Val(float64(float32(value.IntVal))), nil
			}
			return model.Float64Val(float64(value.IntVal)), nil
		}
		if to == 700 {
			return narrowFloat(value.FloatVal)
		}
		return model.Float64Val(value.FloatVal), nil
	}
}
