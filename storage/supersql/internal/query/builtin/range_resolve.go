package builtin

import "github.com/gosuda/supersql-alpha/internal/model"

func rangeCallFamily(signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions) (RangeFamilyInfo, error) {
	var family RangeFamilyInfo
	accept := func(layout model.TypeLayout) error {
		candidate, known := lookupRangeFamily(options, layout.Type.ID)
		if known && options.Types != nil {
			if resolved, declared := options.Types.ResolveType(layout.Type.ID); declared {
				if (resolved.Kind() != model.TypeKindRange && resolved.Kind() != model.TypeKindMultirange) || !sameArrayConstructionLayout(layout, resolved.Layout()) {
					return sqlError("42804", "range argument layout differs from its declaration")
				}
			}
		}
		if !known && layout.Type.Storage == model.TypeArray && len(layout.Children) == 1 {
			if _, _, err := arrayCodecLayout(layout, options); err != nil {
				return err
			}
			candidate, known = lookupRangeFamily(options, layout.Children[0].Type.ID)
		}
		if !known {
			return nil
		}
		if family.Range != 0 && family.Range != candidate.Range {
			return sqlError("42804", "range argument families differ")
		}
		family = candidate
		return nil
	}
	if err := accept(result); err != nil {
		return family, err
	}
	for _, argument := range arguments {
		if err := accept(argument); err != nil {
			return family, err
		}
	}
	if family.Range == 0 {
		for _, candidate := range rangeFamilies {
			if candidate.Difference == signature.ID {
				return candidate, nil
			}
		}
		return family, sqlError("42804", "range call requires a concrete declared family")
	}
	return family, nil
}
func lookupRangeFamily(options ResolveOptions, id model.TypeID) (RangeFamilyInfo, bool) {
	if family, known := LookupRangeFamily(id); known {
		return family, true
	}
	if options.Types == nil {
		return RangeFamilyInfo{}, false
	}
	resolved, known := options.Types.ResolveType(id)
	if !known {
		return RangeFamilyInfo{}, false
	}
	for depth := 0; depth < 64 && resolved.metadata.DeclaredKind() == model.TypeKindDomain; depth++ {
		layout := resolved.BaseLayout()
		if family, known := LookupRangeFamily(layout.Type.ID); known {
			return family, true
		}
		resolved, known = options.Types.ResolveType(layout.Type.ID)
		if !known {
			return RangeFamilyInfo{}, false
		}
	}
	family, known := resolved.RangeFamily()
	if !known {
		return RangeFamilyInfo{}, false
	}
	family.SubtypeStorage = resolved.metadata.Range.Subtype.Type.Storage
	family.Representation = family.Subtype
	layout := resolved.metadata.Range.Subtype
	for range 64 {
		base, custom := options.Types.ResolveType(layout.Type.ID)
		if !custom || base.Kind() != model.TypeKindDomain {
			family.Representation = layout.Type.ID
			break
		}
		layout = base.BaseLayout()
	}
	return family, true
}
// ResolveRangeFamily resolves built-in and snapshot-pinned declared families.
func ResolveRangeFamily(options ResolveOptions, id model.TypeID) (RangeFamilyInfo, bool) {
	return lookupRangeFamily(options, id)
}

// ResolveRangeArrayLayout resolves the owned array of a range type.
func ResolveRangeArrayLayout(options ResolveOptions, rangeID model.TypeID) (model.TypeLayout, bool) {
	if info, known := LookupType(rangeID); known && info.Array != 0 {
		element, known := resolvedTypeLayout(options, rangeID)
		if !known {
			return model.TypeLayout{}, false
		}
		return model.TypeLayout{
			Type: model.Type{ID: info.Array, Storage: model.TypeArray, Modifier: -1},
			Children: []model.TypeLayout{element},
		}, true
	}
	if options.Types == nil {
		return model.TypeLayout{}, false
	}
	resolved, known := options.Types.ResolveType(rangeID)
	if !known || resolved.resolver == nil || resolved.metadata.Array == 0 {
		return model.TypeLayout{}, false
	}
	array, known := resolved.resolver.objects[resolved.metadata.Array]
	if !known || array.metadata.DeclaredKind() != model.TypeKindArray || array.metadata.Base != resolved.descriptor.ID() {
		return model.TypeLayout{}, false
	}
	return array.Layout(), true
}

func rangeTypedCallable(signature Signature, arguments []model.TypeLayout, result model.TypeLayout, kernel scalarKernel, metadata uint64) Callable {
	callable := Callable{signature: signature, kernel: kernel, resultStorage: result.Type.Storage, metadataBytes: metadata}
	callable.signature.Result = result.Type.ID
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	return callable
}
