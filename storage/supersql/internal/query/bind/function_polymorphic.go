package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func (b *binder) exactPolymorphicElement(signature builtin.Signature, args []expression, pos int) (model.TypeID, bool, error) {
	if _, err := b.polymorphicRangeFamily(signature, args, rangeExactOptional, pos); err != nil {
		return 0, true, err
	}
	var element model.TypeID
	requiresKnown := false
	for i, arg := range args {
		declared := signature.Arguments[i]
		if !polymorphic(declared) || compatiblePolymorphic(declared) {
			continue
		}
		switch declared {
		case 2277, 2283, 2776, 3500, 3831, 4537:
			requiresKnown = true
		}
		actual := b.argumentID(arg)
		if actual == 0 {
			continue
		}
		switch declared {
		case 2277:
			typ := b.argumentType(actual, arg)
			if len(typ.Children) != 1 || typ.Type.Storage != model.TypeArray {
				return 0, requiresKnown, bindError("42804", "polymorphic array argument requires array type", pos)
			}
			actual = typ.Children[0].Type.ID
		case 2776:
			if b.argumentType(actual, arg).Type.Storage == model.TypeArray {
				return 0, requiresKnown, bindError("42804", "polymorphic nonarray argument requires nonarray type", pos)
			}
		case 3831, 4537:
			family, ok := b.lookupRangeFamily(actual)
			if !ok || !rangePolymorphicMatches(family, declared, actual) {
				return 0, requiresKnown, bindError("42804", "polymorphic range argument requires matching range type", pos)
			}
			actual = family.Subtype
		}
		if declared == 2276 {
			continue
		}
		if element != 0 && element != actual {
			return 0, requiresKnown, bindError("42804", "polymorphic argument types differ", pos)
		}
		element = actual
	}
	return element, requiresKnown, nil
}

func (b *binder) exactPolymorphicArray(signature builtin.Signature, args []expression, pos int) (model.TypeID, error) {
	var array model.TypeID
	for i, arg := range args {
		if signature.Arguments[i] != 2277 {
			continue
		}
		actual := b.argumentID(arg)
		if actual == 0 {
			continue
		}
		if array != 0 && array != actual {
			return 0, bindError("42804", "polymorphic array argument types differ", pos)
		}
		array = actual
	}
	return array, nil
}

func (b *binder) polymorphicArrayType(element model.TypeID, pos int) (model.TypeID, error) {
	if array := b.arrayTypeID(element); array != 0 {
		return array, nil
	}
	if _, known := builtin.LookupType(element); known {
		return 0, bindError("42704", "could not find array type for input data type", pos)
	}
	return 0, missing("polymorphic array identity", pos)
}

// userPolymorphic records the type variables a user routine's declared
// pseudo-type marker parameters infer at one call site, following PostgreSQL's
// consistency rules: the exact family shares one scalar variable across
// anyelement/anynonarray positions, couples anyarray positions through their
// element, and chains anyrange -> subtype and anymultirange -> range ->
// subtype into the same variables. anyenum is an independent kind-constrained
// variable. The anycompatible family instead contributes each position's
// scalar element to a common-type computation, with anycompatiblerange and
// anycompatiblemultirange recording single-valued range variables whose
// subtypes must agree with the common type. An untyped argument matches
// without inferring; marker parameters and results substitute from this
// state, and any marker left unresolved fails as a polymorphic inference
// error rather than an unmatched signature.
type userPolymorphic struct {
	inferred    map[model.TypeID]model.TypeLayout
	compatibles []expression
	common      model.TypeLayout
	commonDone  bool
}

// containsPseudoMarker reports whether a declared layout is or embeds a
// pseudo-type marker — a procedure's composite record result carries marker
// children, for example.
func containsPseudoMarker(layout model.TypeLayout) bool {
	if model.IsPseudoTypeID(layout.Type.ID) {
		return true
	}
	for i := range layout.Children {
		if containsPseudoMarker(layout.Children[i]) {
			return true
		}
	}
	return false
}

func (p *userPolymorphic) infer(id model.TypeID, layout model.TypeLayout) bool {
	if seen, ok := p.inferred[id]; ok {
		return sameType(seen, layout)
	}
	p.inferred[id] = layout
	return true
}

func (b *binder) matchPolymorphicParameter(marker model.TypeID, arg expression, inferred *userPolymorphic) bool {
	actual := arg.typ
	if actual.Type.ID == 0 {
		return true
	}
	switch marker {
	case 2277, 5078:
		if actual.Type.Storage != model.TypeArray || len(actual.Children) != 1 {
			return false
		}
	case 2776, 5079:
		if actual.Type.Storage == model.TypeArray {
			return false
		}
	case 3500:
		if !b.declaredTypeKind(actual.Type.ID, model.TypeKindEnum) {
			return false
		}
	case 3831, 4537, 5080, 4538:
		kind := model.TypeKindRange
		if marker == 4537 || marker == 4538 {
			kind = model.TypeKindMultirange
		}
		if !b.declaredTypeKind(actual.Type.ID, kind) {
			return false
		}
	}
	if compatiblePolymorphic(marker) {
		return b.matchCompatibleParameter(marker, arg, actual, inferred)
	}
	switch marker {
	case 2283, 2776:
		return inferred.infer(2283, actual)
	case 3500:
		return inferred.infer(3500, actual)
	case 2277:
		return inferred.infer(2277, actual) && inferred.infer(2283, actual.Children[0])
	case 3831:
		return inferred.infer(3831, actual) && b.inferRangeSubtype(actual, 2283, inferred)
	case 4537:
		return inferred.infer(4537, actual) && b.inferRangeSubtype(actual, 3831, inferred) && b.inferRangeSubtype(actual, 2283, inferred)
	}
	return true
}

// inferRangeSubtype couples a range or multirange argument into the family
// variable target selects: 3831 records the range type, 2283 records the
// range's subtype.
func (b *binder) inferRangeSubtype(actual model.TypeLayout, target model.TypeID, inferred *userPolymorphic) bool {
	family, ok := b.lookupRangeFamily(actual.Type.ID)
	if !ok {
		return false
	}
	id := family.Range
	if target == 2283 {
		id = family.Subtype
	}
	layout, ok := b.typeLayoutByID(id)
	return ok && inferred.infer(target, layout)
}

func (b *binder) matchCompatibleParameter(marker model.TypeID, arg expression, actual model.TypeLayout, inferred *userPolymorphic) bool {
	element := expression{typ: actual, position: arg.position, collation: b.expressionCollation(arg)}
	switch marker {
	case 5077, 5079:
	case 5078:
		element.typ = actual.Children[0]
	case 5080:
		if !inferred.infer(5080, actual) {
			return false
		}
	case 4538:
		family, ok := b.lookupRangeFamily(actual.Type.ID)
		if !ok {
			return false
		}
		rng, ok := b.typeLayoutByID(family.Range)
		if !ok || !inferred.infer(4538, actual) || !inferred.infer(5080, rng) {
			return false
		}
	}
	if rangePolymorphic(marker) {
		family, ok := b.lookupRangeFamily(actual.Type.ID)
		if !ok {
			return false
		}
		subtype, ok := b.typeLayoutByID(family.Subtype)
		if !ok {
			return false
		}
		element.typ = subtype
	}
	inferred.compatibles = append(inferred.compatibles, element)
	return true
}

// commonCompatible computes the anycompatible family's common type once per
// call, then verifies kinded compatible positions against it: a compatible
// range or multirange argument's subtype must be the common type.
func (b *binder) commonCompatible(inferred *userPolymorphic, pos int) (model.TypeLayout, error) {
	if !inferred.commonDone {
		inferred.commonDone = true
		if len(inferred.compatibles) != 0 {
			common, err := b.commonType(inferred.compatibles)
			if err != nil {
				return model.TypeLayout{}, err
			}
			inferred.common = common
		}
	}
	if inferred.common.Type.ID == 0 {
		return model.TypeLayout{}, bindError("42804", "could not determine polymorphic type", pos)
	}
	for _, id := range []model.TypeID{5080, 4538} {
		declared, ok := inferred.inferred[id]
		if !ok {
			continue
		}
		family, ok := b.lookupRangeFamily(declared.Type.ID)
		if !ok {
			return model.TypeLayout{}, bindError("42804", "polymorphic range argument requires matching range type", pos)
		}
		if family.Subtype != inferred.common.Type.ID {
			return model.TypeLayout{}, bindError("42804", "compatible range subtype differs from common type", pos)
		}
	}
	return inferred.common, nil
}

// resolvePolymorphicLayout substitutes one declared marker layout with the
// actual type the call site inferred. Array results derive from the scalar
// variable and multirange results derive from the range variable; a marker
// position or result that no argument could infer fails as PostgreSQL's
// polymorphic inference error. Non-marker composite layouts (such as a
// routine's record result assembled from OUT parameters) resolve marker
// children recursively.
func (b *binder) resolvePolymorphicLayout(marker model.TypeLayout, inferred *userPolymorphic, pos int) (model.TypeLayout, error) {
	if !model.IsPseudoTypeID(marker.Type.ID) {
		if len(marker.Children) == 0 {
			return marker, nil
		}
		resolved := marker
		resolved.Children = make([]model.TypeLayout, len(marker.Children))
		for i := range marker.Children {
			child, err := b.resolvePolymorphicLayout(marker.Children[i], inferred, pos)
			if err != nil {
				return model.TypeLayout{}, err
			}
			resolved.Children[i] = child
		}
		return resolved, nil
	}
	id := marker.Type.ID
	if !model.IsRoutineParameterPseudoTypeID(id) || id == 2281 {
		// Result-only pseudo-types (void, trigger, ...) and internal are not
		// polymorphic inference variables; they pass through unchanged.
		return marker, nil
	}
	arrayOf := func(element model.TypeLayout) (model.TypeLayout, error) {
		arrayID, err := b.polymorphicArrayType(element.Type.ID, pos)
		if err != nil {
			return model.TypeLayout{}, err
		}
		if layout, ok := b.typeLayoutByID(arrayID); ok {
			return layout, nil
		}
		return model.TypeLayout{}, bindError("42804", "could not determine polymorphic type", pos)
	}
	multirangeOf := func(rng model.TypeLayout) (model.TypeLayout, error) {
		if family, ok := b.lookupRangeFamily(rng.Type.ID); ok {
			if layout, ok := b.typeLayoutByID(family.Multirange); ok {
				return layout, nil
			}
		}
		return model.TypeLayout{}, bindError("42804", "could not determine polymorphic type", pos)
	}
	switch id {
	case 2283, 2776:
		if resolved, ok := inferred.inferred[2283]; ok {
			return resolved, nil
		}
	case 3500:
		if resolved, ok := inferred.inferred[3500]; ok {
			return resolved, nil
		}
	case 2277:
		if resolved, ok := inferred.inferred[2277]; ok {
			return resolved, nil
		}
		if scalar, ok := inferred.inferred[2283]; ok {
			return arrayOf(scalar)
		}
	case 3831:
		if resolved, ok := inferred.inferred[3831]; ok {
			return resolved, nil
		}
	case 4537:
		if resolved, ok := inferred.inferred[4537]; ok {
			return resolved, nil
		}
		if rng, ok := inferred.inferred[3831]; ok {
			return multirangeOf(rng)
		}
	case 5077, 5079:
		return b.commonCompatible(inferred, pos)
	case 5078:
		common, err := b.commonCompatible(inferred, pos)
		if err != nil {
			return model.TypeLayout{}, err
		}
		return b.compatibleArray(common), nil
	case 5080:
		if resolved, ok := inferred.inferred[5080]; ok {
			return resolved, nil
		}
		// PostgreSQL would look up a range over the common subtype here; no
		// subtype->range index exists, so this case errors out below.
	case 4538:
		if resolved, ok := inferred.inferred[4538]; ok {
			return resolved, nil
		}
		if rng, ok := inferred.inferred[5080]; ok {
			return multirangeOf(rng)
		}
	}
	return model.TypeLayout{}, bindError("42804", "could not determine polymorphic type", pos)
}
