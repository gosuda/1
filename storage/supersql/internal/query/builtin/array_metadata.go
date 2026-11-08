package builtin

import (
	"context"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileArrayMetadata(signature Signature) scalarKernel {
	switch signature.ID {
	case 747, 748, 2091, 2092, 2176, 3179:
	default:
		return nil
	}
	return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		array := args[0]
		if err := model.ValidateArrayShape(array.Dimensions, uint64(len(array.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		if signature.ID == 3179 {
			count := int64(len(array.ArrayVal))
			if err := integerRange(count, 23); err != nil {
				return model.Value{}, err
			}
			return model.Int64Val(count), nil
		}
		if len(array.Dimensions) == 0 {
			return model.NullVal(primitiveStorage(signature.Result)), nil
		}
		switch signature.ID {
		case 747:
			return arrayDimensionText(array.Dimensions, budget)
		case 748:
			return model.Int64Val(int64(len(array.Dimensions))), nil
		}
		requested := args[1].IntVal
		if requested <= 0 || requested > int64(len(array.Dimensions)) {
			return model.NullVal(model.TypeInt64), nil
		}
		dimension := array.Dimensions[requested-1]
		switch signature.ID {
		case 2091:
			return model.Int64Val(int64(dimension.LowerBound)), nil
		case 2092:
			return model.Int64Val(int64(dimension.LowerBound) + int64(dimension.Length) - 1), nil
		default:
			return model.Int64Val(int64(dimension.Length)), nil
		}
	}
}

func arrayDimensionText(dimensions []model.ArrayDimension, budget int64) (model.Value, error) {
	// Six dimensions, each with two signed int32 bounds and three delimiters.
	var buffer [6 * 25]byte
	output := buffer[:0]
	for _, dimension := range dimensions {
		output = append(output, '[')
		output = strconv.AppendInt(output, int64(dimension.LowerBound), 10)
		output = append(output, ':')
		output = strconv.AppendInt(output, int64(dimension.LowerBound)+int64(dimension.Length)-1, 10)
		output = append(output, ']')
	}
	if err := outputBudget(int64(len(output)), budget); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(string(output)), nil
}
