package builtin

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func resolveArrayConstruction(signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions) (Callable, error) {
	_, element, err := arrayCodecLayout(result, options)
	if err != nil {
		return Callable{}, err
	}
	callable := Callable{signature: signature, resultStorage: model.TypeArray}
	for i, argument := range arguments {
		expected := result
		appendElement := signature.ID == 378 && i == 1
		prependElement := signature.ID == 379 && i == 0
		if appendElement || prependElement {
			expected = element
		}
		if !sameArrayConstructionLayout(argument, expected) {
			return Callable{}, sqlError("42804", "array construction argument differs from its result layout")
		}
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	callable.signature.Result = result.Type.ID
	switch signature.ID {
	case 378:
		callable.kernel = appendArray
	case 379:
		callable.kernel = prependArray
	case 383:
		callable.kernel = concatenateArrays
	}
	return callable, nil
}

func sameArrayConstructionLayout(left, right model.TypeLayout) bool {
	leftType, rightType := left.Type, right.Type
	// Collation belongs to consuming operators, not the copied representation.
	leftType.Collation, rightType.Collation = 0, 0
	return leftType == rightType && slices.EqualFunc(left.Children, right.Children, sameArrayConstructionLayout)
}
