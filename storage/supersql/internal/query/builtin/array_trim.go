package builtin

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func resolveArrayTrim(signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions) (Callable, error) {
	if len(arguments) != 2 || arguments[1].Type.ID != 23 || arguments[1].Type.Storage != model.TypeInt64 {
		return Callable{}, sqlError("42804", "array trim requires an array and int4 count")
	}
	if _, _, err := arrayCodecLayout(arguments[0], options); err != nil {
		return Callable{}, err
	}
	if !sameArrayTrimLayout(arguments[0], result) {
		return Callable{}, sqlError("42804", "array trim result differs from its input layout")
	}
	signature.Arguments[0], signature.Result = arguments[0].Type.ID, result.Type.ID
	return Callable{signature: signature, kernel: trimArray, resultStorage: model.TypeArray, argumentStorage: [8]model.DataType{model.TypeArray, model.TypeInt64}}, nil
}

func sameArrayTrimLayout(left, right model.TypeLayout) bool {
	return left.Type == right.Type && slices.EqualFunc(left.Children, right.Children, sameArrayTrimLayout)
}

func trimArray(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	input, count := arguments[0], arguments[1].IntVal
	if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil {
		return model.Value{}, sqlError("2202E", "invalid array dimensions")
	}
	length := int64(0)
	if len(input.Dimensions) != 0 {
		length = int64(input.Dimensions[0].Length)
	}
	if count < 0 || count > length {
		return model.Value{}, sqlError("2202E", "number of elements to trim is out of bounds")
	}
	if count == length {
		return model.Value{Type: model.TypeArray}, nil
	}
	keep := len(input.ArrayVal) / int(length) * int(length-count)
	view := model.Value{Type: model.TypeArray, ArrayVal: input.ArrayVal[:keep], Dimensions: input.Dimensions}
	limit := uint64(min(budget, arrayInputMaxBytes))
	if _, err := model.ValueBytes(ctx, view, limit); err != nil {
		return model.Value{}, arrayTrimCopyError(err)
	}
	owned, _, err := model.CopyValue(ctx, view, limit)
	if err != nil {
		return model.Value{}, arrayTrimCopyError(err)
	}
	for i := range owned.Dimensions {
		owned.Dimensions[i].LowerBound = 1
	}
	owned.Dimensions[0].Length = int32(length - count)
	return owned, nil
}

func arrayTrimCopyError(err error) error {
	if errors.Is(err, model.ErrValueBudget) || errors.Is(err, model.ErrValueDepth) {
		return &model.Error{Code: "54000", Message: "array trim result exceeds resource limit", Cause: err}
	}
	return err
}
