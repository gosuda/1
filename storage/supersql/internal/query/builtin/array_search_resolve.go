package builtin

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func resolveArraySearch(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	_, element, err := arrayCodecLayout(arguments[0], options)
	if err != nil {
		return Callable{}, err
	}
	if !sameArrayConstructionLayout(element, arguments[1]) {
		return Callable{}, sqlError("42804", "array search value differs from the element layout")
	}
	replacement := signature.ID == 3167 || signature.ID == 3168
	if replacement {
		if !sameArrayConstructionLayout(arguments[0], result) {
			return Callable{}, sqlError("42804", "array replacement result differs from its input layout")
		}
		if signature.ID == 3168 && !sameArrayConstructionLayout(element, arguments[2]) {
			return Callable{}, sqlError("42804", "replacement value differs from the element layout")
		}
	} else if signature.ID == 3279 {
		_, position, err := arrayCodecLayout(result, options)
		if err != nil {
			return Callable{}, err
		}
		if result.Type.ID != 1007 || position.Type.ID != 23 || position.Type.Storage != model.TypeInt64 {
			return Callable{}, sqlError("42804", "array positions requires an int4 array result")
		}
	} else {
		if result.Type.ID != 23 || result.Type.Storage != model.TypeInt64 {
			return Callable{}, sqlError("42804", "array position requires an int4 result")
		}
		if signature.ID == 3278 && (arguments[2].Type.ID != 23 || arguments[2].Type.Storage != model.TypeInt64) {
			return Callable{}, sqlError("42804", "array position requires an int4 starting subscript")
		}
	}
	metadata := uint64(unsafe.Sizeof(Signature{})) + uint64(unsafe.Sizeof(Callable{})) + 4*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "array search metadata budget exceeded")
	}
	selection, err := resolveArrayElementComparison(ctx, element, arguments[1], true, options, maximum-metadata)
	if err != nil {
		return Callable{}, err
	}
	callable := Callable{signature: signature, resultStorage: result.Type.Storage, metadataBytes: metadata + selection.metadataBytes}
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	callable.signature.Result = result.Type.ID
	if replacement {
		callable.kernel = compileArrayReplace(signature, selection.callable, selection.lookupErr)
	} else {
		callable.kernel = compileArrayPosition(signature, selection.callable, selection.lookupErr)
	}
	return callable, nil
}
