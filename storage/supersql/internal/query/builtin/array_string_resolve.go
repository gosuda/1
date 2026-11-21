package builtin

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func resolveArrayToString(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if result.Type.ID != 25 || result.Type.Storage != model.TypeString {
		return Callable{}, sqlError("42804", "array string conversion requires text result")
	}
	_, elementLayout, err := arrayCodecLayout(arguments[0], options)
	if err != nil {
		return Callable{}, err
	}
	for _, argument := range arguments[1:] {
		if argument.Type.ID != 25 || argument.Type.Storage != model.TypeString {
			return Callable{}, sqlError("42804", "array string separator and null marker must be text")
		}
	}
	info, known := lookupResolvedTypeIO(options, elementLayout)
	if !known || info.Output == 0 {
		return Callable{}, sqlError("42804", "array element has no resolved output metadata")
	}
	metadata := uint64(unsafe.Sizeof(Signature{})) + uint64(unsafe.Sizeof(Callable{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "array string conversion metadata budget exceeded")
	}
	cstring := model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
	element, err := ResolveTyped(ctx, info.Output, []model.TypeLayout{elementLayout}, cstring, options, maximum-metadata)
	if err != nil {
		return Callable{}, err
	}
	callable := Callable{signature: signature, resultStorage: model.TypeString, metadataBytes: metadata + element.OwnedBytes()}
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	callable.kernel = compileArrayToString(signature, element)
	return callable, nil
}
