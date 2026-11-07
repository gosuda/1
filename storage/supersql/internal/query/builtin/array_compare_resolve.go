package builtin

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func resolveArrayComparison(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	extremum := signature.ID == 515 || signature.ID == 516
	if len(arguments) != 2 || (!extremum && (result.Type.ID != signature.Result || result.Type.Storage != primitiveStorage(signature.Result))) || (extremum && (result.Type.ID != arguments[0].Type.ID || result.Type.Storage != model.TypeArray)) {
		return Callable{}, sqlError("42804", "array comparison signature mismatch")
	}
	_, left, err := arrayCodecLayout(arguments[0], options)
	if err != nil {
		return Callable{}, err
	}
	_, right, err := arrayCodecLayout(arguments[1], options)
	if err != nil {
		return Callable{}, err
	}
	if left.Type.ID != right.Type.ID || left.Type.Storage != right.Type.Storage {
		return Callable{}, sqlError("42804", "cannot compare arrays of different element types")
	}
	metadata := uint64(unsafe.Sizeof(Signature{})) + uint64(unsafe.Sizeof(Callable{})) + 4*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "array comparison metadata budget exceeded")
	}
	membership := primitiveOperation(signature.ID) == opArrayMembership
	if membership && signature.ID == 2748 {
		left, right = right, left
	}
	equality := membership || signature.ID == 744 || signature.ID == 390
	selection, err := resolveArrayElementComparison(ctx, left, right, equality, options, maximum-metadata)
	if err != nil {
		return Callable{}, err
	}
	callable := Callable{signature: signature, resultStorage: result.Type.Storage, metadataBytes: metadata + selection.metadataBytes}
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = model.TypeArray
	}
	if membership {
		callable.kernel = compileArrayMembership(signature, selection.callable, selection.lookupErr)
	} else {
		callable.kernel = compileArrayComparison(signature, selection.callable, selection.lookupErr)
	}
	return callable, nil
}

func defaultComparisonClass(ctx context.Context, method string, typ model.Type, options ResolveOptions) (OperatorClassInfo, bool, error) {
	info, known := LookupType(typ.ID)
	for depth := 0; ; depth++ {
		if err := ctx.Err(); err != nil {
			return OperatorClassInfo{}, false, err
		}
		if depth >= 64 {
			return OperatorClassInfo{}, false, sqlError("54000", "type base chain exceeds limit")
		}
		if known && info.Base != 0 {
			typ.ID = info.Base
			info, known = LookupType(typ.ID)
			continue
		}
		if !known && options.Types != nil {
			if resolved, custom := options.Types.ResolveType(typ.ID); custom && resolved.Kind() == model.TypeKindDomain {
				typ = resolved.BaseLayout().Type
				info, known = LookupType(typ.ID)
				continue
			}
		}
		if !known && options.InformationSchemaTypes != nil {
			if binding, logical := options.InformationSchemaTypes.ResolveInformationSchemaType(typ.ID); logical && binding.Kind == model.TypeKindDomain {
				typ = binding.BaseLayout.Type
				info, known = LookupType(typ.ID)
				continue
			}
		}
		break
	}
	if exact, found := LookupDefaultOperatorClass(method, typ.ID); found {
		return exact, true, nil
	}
	var compatible, preferred OperatorClassInfo
	matches, preferredMatches := 0, 0
	for _, candidate := range operatorClassCatalog {
		if err := ctx.Err(); err != nil {
			return OperatorClassInfo{}, false, err
		}
		if !candidate.Default || candidate.Method != method || !comparisonBinaryCoercible(typ, candidate.InputType, options) {
			continue
		}
		compatible = candidate
		matches++
		input, inputKnown := LookupType(candidate.InputType)
		if known && inputKnown && input.Category == info.Category && input.Preferred {
			preferred = candidate
			preferredMatches++
		}
	}
	if preferredMatches == 1 {
		return preferred, true, nil
	}
	if preferredMatches == 0 && matches == 1 {
		return compatible, true, nil
	}
	return OperatorClassInfo{}, false, nil
}

func comparisonBinaryCoercible(source model.Type, target model.TypeID, options ResolveOptions) bool {
	if source.ID == target {
		return true
	}
	for depth := 0; depth < 64; depth++ {
		if options.Types != nil {
			if resolved, known := options.Types.ResolveType(source.ID); known && resolved.Kind() == model.TypeKindDomain {
				source = resolved.BaseLayout().Type
				if source.ID == target {
					return true
				}
				continue
			}
		}
		if options.InformationSchemaTypes != nil {
			if binding, known := options.InformationSchemaTypes.ResolveInformationSchemaType(source.ID); known && binding.Kind == model.TypeKindDomain {
				source = binding.BaseLayout.Type
				if source.ID == target {
					return true
				}
				continue
			}
		}
		break
	}
	if target == 2277 && source.Storage == model.TypeArray {
		// anyarray comparison uses the element type's own operator class, so a
		// missing element operator surfaces when the array kernel runs.
		return true
	}
	if target == 2249 && source.Storage == model.TypeComposite {
		// Every composite value compares through the record comparison
		// functions; the caller validates the concrete field declarations.
		return true
	}
	if target == 3500 {
		if info, found := LookupType(source.ID); found {
			return info.Kind == 'e'
		}
		resolved, found := resolvedType(options, source.ID, model.TypeKindEnum)
		return found && resolved != nil
	}
	if target == 3831 || target == 4537 {
		family, found := ResolveRangeFamily(options, source.ID)
		return found && source.Storage == model.TypeBytes && ((target == 3831 && source.ID == family.Range) || (target == 4537 && source.ID == family.Multirange))
	}
	cast, found := LookupCast(source.ID, target)
	return found && cast.Context == 'i' && cast.Method == 'b'
}

func arrayCodecLayout(array model.TypeLayout, options ResolveOptions) (TypeInfo, model.TypeLayout, error) {
	info, known := LookupType(array.Type.ID)
	validLayout := array.Type.Storage == model.TypeArray && len(array.Children) == 1
	if !known && validLayout && options.Types != nil {
		if resolved, declared := options.Types.ResolveType(array.Type.ID); declared && resolved.Kind() == model.TypeKindArray && sameArrayConstructionLayout(array, resolved.Layout()) {
			io := resolved.TypeIO()
			info = TypeInfo{ID: array.Type.ID, Category: 'A', Kind: 'b', Element: resolved.BaseLayout().Type.ID, Input: io.Input, Output: io.Output, Delimiter: io.Delimiter}
			known = true
		}
	}
	if !known && validLayout && options.InformationSchemaTypes != nil {
		if binding, declared := options.InformationSchemaTypes.ResolveInformationSchemaType(array.Type.ID); declared && binding.Kind == model.TypeKindArray && sameArrayConstructionLayout(array, binding.Layout) {
			info = TypeInfo{ID: array.Type.ID, Category: 'A', Kind: 'b', Element: binding.BaseLayout.Type.ID, Input: binding.TypeIO.Input, Output: binding.TypeIO.Output, Delimiter: binding.TypeIO.Delimiter}
			known = true
		}
	}
	// PostgreSQL array types all share the array representation codec, so a
	// concrete one-element layout is self-describing even when its identity has
	// no declaration. The element layout still has to match a catalog identity.
	if !known && validLayout && array.Type.ID != 0 && array.Children[0].Type.ID != 0 {
		info = TypeInfo{ID: array.Type.ID, Category: 'A', Kind: 'b', Element: array.Children[0].Type.ID, Input: 750, Output: 751, Delimiter: ','}
		known = true
	}
	validIdentity := known && info.IsArray()
	if !validIdentity || !validLayout {
		return TypeInfo{}, model.TypeLayout{}, sqlError("42804", "array operation requires a concrete array layout")
	}
	if array.Children[0].Type.ID != info.Element {
		return TypeInfo{}, model.TypeLayout{}, sqlError("42804", "array element layout differs from catalog identity")
	}
	return info, array.Children[0], nil
}
