package bind

import (
	"cmp"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func polymorphic(id model.TypeID) bool {
	switch id {
	case 2276, 2277, 2283, 2776, 3500, 3831, 4537, 4538, 5077, 5078, 5079, 5080:
		return true
	}
	return false
}
func (b *binder) specialize(sig builtin.Signature, args []expression, pos int) (builtin.Signature, error) {
	compatible, _, err := b.compatibleElement(sig, args, pos)
	if err != nil {
		return sig, err
	}
	element, requiresKnown, err := b.exactPolymorphicElement(sig, args, pos)
	if err != nil {
		return sig, err
	}
	ranges, err := b.polymorphicRangeFamily(sig, args, rangeExactRequired, pos)
	if err != nil {
		return sig, err
	}
	compatibleRanges, err := b.polymorphicRangeFamily(sig, args, rangeCompatibleRequired, pos)
	if err != nil {
		return sig, err
	}
	if requiresKnown && element == 0 {
		return sig, bindError("42804", "could not determine polymorphic type because input has type unknown", pos)
	}
	element = cmp.Or(element, 25)
	array, err := b.exactPolymorphicArray(sig, args, pos)
	if err != nil {
		return sig, err
	}
	for i := range args {
		switch sig.Arguments[i] {
		case 2276:
			switch {
			case args[i].typ.Type.ID != 0:
				sig.Arguments[i] = args[i].typ.Type.ID
			case args[i].inferred != 0:
				// An unresolved literal keeps the type its text implies, so
				// pg_typeof(1.5) reports numeric rather than text.
				sig.Arguments[i] = args[i].inferred
			default:
				sig.Arguments[i] = 25
			}
		case 5077, 5079:
			sig.Arguments[i] = compatible.Type.ID
		case 5078:
			sig.Arguments[i], err = b.polymorphicArrayType(compatible.Type.ID, pos)
			if err != nil {
				return sig, err
			}
		case 3831:
			sig.Arguments[i] = ranges.Range
		case 4537:
			sig.Arguments[i] = ranges.Multirange
		case 5080:
			sig.Arguments[i] = compatibleRanges.Range
		case 4538:
			sig.Arguments[i] = compatibleRanges.Multirange
		case 2283, 2776, 3500:
			sig.Arguments[i] = element
		case 2277:
			sig.Arguments[i] = array
			if array == 0 {
				sig.Arguments[i], err = b.polymorphicArrayType(element, pos)
			}
			if err != nil {
				return sig, err
			}
		default:
			if polymorphic(sig.Arguments[i]) {
				return sig, missing("polymorphic range binding", pos)
			}
		}
	}
	switch sig.Result {
	case 5077, 5079:
		sig.Result = compatible.Type.ID
	case 5078:
		sig.Result, err = b.polymorphicArrayType(compatible.Type.ID, pos)
	case 3831:
		sig.Result = ranges.Range
	case 4537:
		sig.Result = ranges.Multirange
	case 5080:
		sig.Result = compatibleRanges.Range
	case 4538:
		sig.Result = compatibleRanges.Multirange
	case 2283, 2776, 3500:
		sig.Result = element
	case 2277:
		sig.Result = array
		if array == 0 {
			sig.Result, err = b.polymorphicArrayType(element, pos)
		}
	default:
		if polymorphic(sig.Result) {
			return sig, missing("polymorphic result binding", pos)
		}
	}
	return sig, err
}

func (b *binder) functionResult(sig builtin.Signature, args []expression) (model.TypeLayout, error) {
	operation, informationFunction := b.options.CatalogRelations.InformationSchemaFunction(sig.ID)
	if informationFunction && operation == builtin.InformationExpandArray && len(args) == 1 {
		array := b.argumentType(b.argumentID(args[0]), args[0])
		if array.Type.Storage != model.TypeArray || len(array.Children) != 1 {
			return model.TypeLayout{}, bindError("42804", "information_schema._pg_expandarray requires a concrete array argument", args[0].position)
		}
		return model.TypeLayout{
			Type:       model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1},
			Children:   []model.TypeLayout{array.Children[0], b.primitive(23)},
			FieldNames: []string{"x", "n"},
		}, nil
	}
	if (sig.ID == 6373 || sig.ID == 6374) && len(args) == 1 {
		result := b.argumentType(b.argumentID(args[0]), args[0])
		if result.Type.Storage != model.TypeComposite {
			return model.TypeLayout{}, bindError("42804", "record aggregate requires a composite argument", args[0].position)
		}
		result.Type.Modifier = -1
		return result, nil
	}
	switch sig.ID {
	case 3382:
		return model.TypeLayout{
			Type:       model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1},
			Children:   []model.TypeLayout{b.primitive(25), b.primitive(1009), b.primitive(1009)},
			FieldNames: []string{"type", "object_names", "object_args"},
		}, nil
	case 3839:
		return model.TypeLayout{
			Type:       model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1},
			Children:   []model.TypeLayout{b.primitive(25), b.primitive(25), b.primitive(25), b.primitive(25)},
			FieldNames: []string{"type", "schema", "name", "identity"},
		}, nil
	case 3954:
		return model.TypeLayout{
			Type:       model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1},
			Children:   []model.TypeLayout{b.primitive(26), b.primitive(26), b.primitive(23)},
			FieldNames: []string{"classid", "objid", "objsubid"},
		}, nil
	}
	declared, ok := b.lookupSignature(sig.ID)
	if ok && compatiblePolymorphic(declared.Result) {
		if rangePolymorphic(declared.Result) {
			return b.primitive(sig.Result), nil
		}
		element, _, err := b.compatibleElement(declared, args, 0)
		if err != nil {
			return model.TypeLayout{}, err
		}
		if declared.Result == 5078 {
			return b.compatibleArray(element), nil
		}
		return element, nil
	}
	if ok && polymorphic(declared.Result) {
		for _, arg := range args {
			typ := b.argumentType(b.argumentID(arg), arg)
			if typ.Type.ID == sig.Result {
				result := typ
				result.Type.Modifier = -1
				return result, nil
			}
		}
		if declared.Result == 2277 {
			for _, arg := range args {
				typ := b.argumentType(b.argumentID(arg), arg)
				if typ.Type.ID != 0 && b.arrayTypeID(typ.Type.ID) == sig.Result {
					return model.TypeLayout{
						Type:     model.Type{ID: sig.Result, Storage: model.TypeArray, Modifier: -1, Collation: typ.Type.Collation},
						Children: []model.TypeLayout{typ},
					}, nil
				}
			}
		}
	}
	if result, known := b.typeLayoutByID(sig.Result); known {
		return result, nil
	}
	return b.primitive(sig.Result), nil
}
func (b *binder) argumentType(id model.TypeID, arg expression) model.TypeLayout {
	if arg.typ.Type.ID == id {
		return arg.typ
	}
	if typ, ok := b.params[arg.parameter]; arg.parameter != 0 && ok && typ.Type.ID == id {
		return typ
	}
	if typ, ok := b.typeLayoutByID(id); ok {
		return typ
	}
	return b.primitive(id)
}

func (b *binder) polymorphicArgumentCompatible(target, source model.TypeID, arg expression) bool {
	if source == 0 {
		return true
	}
	typ := b.argumentType(source, arg)
	switch target {
	case 2277, 5078:
		return typ.Type.Storage == model.TypeArray && len(typ.Children) == 1
	case 2776, 5079:
		return typ.Type.Storage != model.TypeArray
	case 3500:
		return b.declaredTypeKind(source, model.TypeKindEnum)
	case 3831, 5080:
		return b.declaredTypeKind(source, model.TypeKindRange)
	case 4537, 4538:
		return b.declaredTypeKind(source, model.TypeKindMultirange)
	}
	return true
}
func (b *binder) declaredTypeKind(id model.TypeID, kind model.TypeKind) bool {
	remaining := b.options.Limits.MaxDepth
	for {
		if info, known := builtin.LookupType(id); known {
			switch kind {
			case model.TypeKindEnum:
				return info.Kind == 'e'
			case model.TypeKindRange:
				return info.Kind == 'r'
			case model.TypeKindMultirange:
				return info.Kind == 'm'
			}
		}
		descriptor, err := b.findNamedType(id, 0)
		if err != nil || descriptor == nil {
			return false
		}
		spec, err := descriptor.SpecWithin(b.ctx, b.descriptorLimits())
		if err != nil || spec.Type == nil {
			return false
		}
		if spec.Type.DeclaredKind() != model.TypeKindDomain {
			return spec.Type.DeclaredKind() == kind
		}
		if remaining == 0 {
			return false
		}
		remaining--
		id = spec.Type.BaseLayout.Type.ID
	}
}
