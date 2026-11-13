package builtin

import (
	"context"
	"fmt"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type arrayElementComparison struct {
	callable      Callable
	lookupErr     error
	metadataBytes uint64
}

func resolveArrayElementComparison(ctx context.Context, left, right model.TypeLayout, equality bool, options ResolveOptions, maximum uint64) (arrayElementComparison, error) {
	var err error
	left, err = resolvedDomainBaseLayout(left, options)
	if err != nil {
		return arrayElementComparison{}, err
	}
	right, err = resolvedDomainBaseLayout(right, options)
	if err != nil {
		return arrayElementComparison{}, err
	}
	class, found, err := defaultComparisonClass(ctx, "btree", left.Type, options)
	if err != nil {
		return arrayElementComparison{}, err
	}
	if equality && (!found || class.EqualityFunction == 0) {
		class, found, err = defaultComparisonClass(ctx, "hash", left.Type, options)
		if err != nil {
			return arrayElementComparison{}, err
		}
	}
	function := class.CompareFunction
	if equality {
		function = class.EqualityFunction
	}
	if !found || function == 0 {
		header := uint64(unsafe.Sizeof(model.Error{}))
		if header+96 > maximum {
			return arrayElementComparison{}, sqlError("54000", "array comparison error metadata budget exceeded")
		}
		kind := "comparison function"
		if equality {
			kind = "equality operator"
		}
		message := fmt.Sprintf("could not identify a %s for type %d", kind, left.Type.ID)
		return arrayElementComparison{lookupErr: sqlError("42883", message), metadataBytes: header + uint64(len(message))}, nil
	}
	operands := [2]model.TypeLayout{left, right}
	if class.InputType != 2277 && class.InputType != 2249 && class.InputType != 3500 && class.InputType != 3831 && class.InputType != 4537 {
		for i := range operands {
			operands[i].Type.ID = class.InputType
			operands[i].Type.Modifier = -1
		}
	}
	output := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	if equality {
		output.Type.ID, output.Type.Storage = 16, model.TypeBoolean
	}
	callable, err := ResolveTyped(ctx, function, operands[:], output, options, maximum)
	if err != nil {
		return arrayElementComparison{}, err
	}
	return arrayElementComparison{callable: callable, metadataBytes: callable.OwnedBytes()}, nil
}

func arrayElementEqual(ctx context.Context, element Callable, arguments *[2]model.Value, callBudget CallBudget) (bool, error) {
	result, err := element.Call(ctx, arguments[:], callBudget.WithOutput(0))
	clear(arguments[:])
	if err != nil {
		return false, err
	}
	if result.IsNull {
		return false, nil
	}
	return arrayEqualityBoolean(result)
}

func arrayElementSearchEqual(ctx context.Context, element Callable, arguments *[2]model.Value, callBudget CallBudget) (bool, error) {
	result, err := element.Call(ctx, arguments[:], callBudget.WithOutput(0))
	clear(arguments[:])
	if err != nil {
		return false, err
	}
	if result.IsNull {
		return false, sqlError("XX000", "array element equality operator returned NULL")
	}
	return arrayEqualityBoolean(result)
}

func arrayEqualityBoolean(result model.Value) (bool, error) {
	if result.Type != model.TypeBoolean || (result.IntVal != 0 && result.IntVal != 1) {
		return false, sqlError("XX000", "array element equality operator did not return boolean")
	}
	return result.IntVal != 0, nil
}
