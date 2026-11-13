package builtin

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func resolveWidthBucketArray(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if result.Type.ID != 23 || result.Type.Storage != model.TypeInt64 {
		return Callable{}, sqlError("42804", "width_bucket requires an int4 result")
	}
	_, element, err := arrayCodecLayout(arguments[1], options)
	if err != nil {
		return Callable{}, err
	}
	if !sameArrayConstructionLayout(arguments[0], element) {
		return Callable{}, sqlError("42804", "width_bucket operand differs from its threshold element layout")
	}
	metadata := uint64(unsafe.Sizeof(Signature{})) + uint64(unsafe.Sizeof(Callable{})) + 4*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "width_bucket metadata budget exceeded")
	}
	comparison, err := resolveArrayElementComparison(ctx, arguments[0], element, false, options, maximum-metadata)
	if err != nil {
		return Callable{}, err
	}
	callable := Callable{signature: signature, kernel: compileWidthBucketArray(comparison.callable, comparison.lookupErr), resultStorage: model.TypeInt64, metadataBytes: metadata + comparison.metadataBytes}
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	return callable, nil
}
