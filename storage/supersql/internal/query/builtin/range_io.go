package builtin

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type rangeIOCodec struct {
	ops           rangeTypeOps
	element       Callable
	parameter     model.TypeID
	multi, binary bool
}

func resolveRangeIO(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	family, err := rangeCallFamily(signature, arguments, result, options)
	if err != nil {
		return Callable{}, err
	}
	input := signature.ID == 3834 || signature.ID == 4231
	multi := signature.ID == 4231 || signature.ID == 4232 || signature.ID == 4234
	binary := signature.ID == 3837 || signature.ID == 4234
	id := family.Range
	if multi {
		id = family.Multirange
	}
	declaration := result
	if !input && len(arguments) != 0 {
		declaration = arguments[0]
	}
	if options.Types != nil {
		if resolved, known := options.Types.ResolveType(id); known {
			kind := model.TypeKindRange
			if multi {
				kind = model.TypeKindMultirange
			}
			if resolved.Kind() != kind || !sameArrayConstructionLayout(declaration, resolved.Layout()) {
				return Callable{}, sqlError("42804", "range IO layout differs from its declaration")
			}
		}
	}
	if input {
		if len(arguments) != 3 || result.Type.ID != id || result.Type.Storage != model.TypeBytes {
			return Callable{}, sqlError("42804", "range input layout mismatch")
		}
		for i, wanted := range []model.TypeID{2275, 26, 23} {
			if arguments[i].Type.ID != wanted || arguments[i].Type.Storage != primitiveStorage(wanted) {
				return Callable{}, sqlError("42804", "range input metadata layout mismatch")
			}
		}
	} else {
		output := model.TypeID(2275)
		if binary {
			output = 17
		}
		if len(arguments) != 1 || arguments[0].Type.ID != id || arguments[0].Type.Storage != model.TypeBytes || result.Type.ID != output || result.Type.Storage != primitiveStorage(output) {
			return Callable{}, sqlError("42804", "range output layout mismatch")
		}
	}
	metadata := uint64(unsafe.Sizeof(rangeIOCodec{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "range IO metadata budget exceeded")
	}
	ops, owned, err := newRangeTypeOps(ctx, family, options, maximum-metadata)
	if err != nil {
		return Callable{}, err
	}
	metadata += owned
	layout, known := resolvedTypeLayout(options, family.Subtype)
	if !known {
		return Callable{}, sqlError("42704", "range subtype does not exist")
	}
	layout.Type.Collation = family.Collation
	info, known := lookupResolvedTypeIO(options, layout)
	if !known {
		return Callable{}, sqlError("42804", "range subtype has no IO metadata")
	}
	var element Callable
	elementLayout := layout
	if input {
		element, err = resolveElementInput(ctx, info.Input, layout, options, maximum-metadata)
	} else {
		function, output := info.Output, model.TypeID(2275)
		if binary {
			output = 17
			switch family.representation() {
			case 23:
				function = 2407
			case 20:
				function = 2409
			case 1700:
				function = 2461
			case 1082:
				function = 2469
			case 1114:
				function = 2475
			case 1184:
				function = 2477
			default:
				if family.enum != nil {
					function = 3533
					break
				}
				return Callable{}, sqlError("42804", "range subtype has no send capability")
			}
			if family.enum == nil && family.representation() != family.Subtype {
				var known bool
				elementLayout, known = resolvedTypeLayout(options, family.representation())
				if !known {
					return Callable{}, sqlError("42704", "range subtype representation does not exist")
				}
			}
		}
		element, err = ResolveTyped(ctx, function, []model.TypeLayout{elementLayout}, model.TypeLayout{Type: model.Type{ID: output, Storage: primitiveStorage(output), Modifier: -1}}, options, maximum-metadata)
	}
	if err != nil {
		return Callable{}, err
	}
	codec := &rangeIOCodec{ops: ops, element: element, parameter: info.Parameter, multi: multi, binary: binary}
	kernel := codec.output
	if input {
		kernel = codec.input
	}
	return rangeTypedCallable(signature, arguments, result, kernel, metadata+element.OwnedBytes()), nil
}
