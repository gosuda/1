package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func compatiblePolymorphic(id model.TypeID) bool {
	return id == 5077 || id == 5078 || id == 5079 || id == 5080 || id == 4538
}

func (b *binder) compatibleElement(signature builtin.Signature, args []expression, pos int) (model.TypeLayout, bool, error) {
	var elements [8]expression
	count := 0
	for i, arg := range args {
		declared := signature.Arguments[i]
		if !compatiblePolymorphic(declared) {
			continue
		}
		typ := b.argumentType(b.argumentID(arg), arg)
		if typ.Type.ID != 0 && declared == 5078 {
			if typ.Type.Storage != model.TypeArray || len(typ.Children) != 1 {
				return model.TypeLayout{}, true, bindError("42804", "polymorphic array argument requires array type", pos)
			}
			typ = typ.Children[0]
		}
		if declared == 5079 && typ.Type.Storage == model.TypeArray {
			return model.TypeLayout{}, true, bindError("42804", "polymorphic nonarray argument requires nonarray type", pos)
		}
		if typ.Type.ID != 0 && rangePolymorphic(declared) {
			family, ok := b.lookupRangeFamily(typ.Type.ID)
			if !ok || !rangePolymorphicMatches(family, declared, typ.Type.ID) {
				return model.TypeLayout{}, true, bindError("42804", "polymorphic range argument requires matching range type", pos)
			}
			typ, ok = b.typeLayoutByID(family.Subtype)
			if !ok {
				return model.TypeLayout{}, true, bindError("42704", "range subtype does not exist", pos)
			}
		}
		elements[count] = expression{typ: typ, position: arg.position, collation: b.expressionCollation(arg)}
		count++
	}
	if count == 0 {
		return model.TypeLayout{}, false, nil
	}
	typ, err := b.commonType(elements[:count])
	if err != nil {
		return typ, true, err
	}
	family, err := b.polymorphicRangeFamily(signature, args, rangeCompatibleOptional, pos)
	if err != nil {
		return typ, true, err
	}
	if family.Range != 0 && typ.Type.ID != family.Subtype {
		return typ, true, bindError("42804", "compatible range subtype differs from common type", pos)
	}
	return typ, true, err
}

func (b *binder) compatibleArray(element model.TypeLayout) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: b.arrayTypeID(element.Type.ID), Storage: model.TypeArray, Modifier: -1, Collation: element.Type.Collation}, Children: []model.TypeLayout{element}}
}

func (b *binder) functionArguments(signature builtin.Signature, args []expression, pos int) ([8]model.TypeLayout, error) {
	var targets [8]model.TypeLayout
	declared, _ := b.lookupSignature(signature.ID)
	element, compatible, err := b.compatibleElement(declared, args, pos)
	if err != nil {
		return targets, err
	}
	for i, arg := range args {
		targets[i] = b.argumentType(signature.Arguments[i], arg)
		if signature.Arguments[i] == 2249 {
			source := b.argumentType(b.argumentID(arg), arg)
			if source.Type.Storage == model.TypeComposite {
				if err := b.validateIOLayout(source, pos); err != nil {
					return targets, err
				}
				targets[i] = source
			}
		}
		if !compatible || !compatiblePolymorphic(declared.Arguments[i]) {
			continue
		}
		if rangePolymorphic(declared.Arguments[i]) {
			continue
		}
		targets[i] = element
		if declared.Arguments[i] == 5078 {
			targets[i] = b.compatibleArray(element)
		}
	}
	return targets, nil
}
