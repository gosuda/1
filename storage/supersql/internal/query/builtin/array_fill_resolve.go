package builtin

import "github.com/gosuda/supersql-alpha/internal/model"

func resolveArrayFill(signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions) (Callable, error) {
	_, element, err := arrayCodecLayout(result, options)
	if err != nil {
		return Callable{}, err
	}
	if !sameArrayConstructionLayout(arguments[0], element) {
		return Callable{}, sqlError("42804", "array fill element differs from its result layout")
	}
	for _, argument := range arguments[1:] {
		_, dimension, err := arrayCodecLayout(argument, options)
		if err != nil {
			return Callable{}, err
		}
		if argument.Type.ID != 1007 || dimension.Type.ID != 23 || dimension.Type.Storage != model.TypeInt64 {
			return Callable{}, sqlError("42804", "array fill dimensions require int4 arrays")
		}
	}
	callable := Callable{signature: signature, kernel: compileArrayFill(signature), resultStorage: model.TypeArray}
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	callable.signature.Result = result.Type.ID
	return callable, nil
}
