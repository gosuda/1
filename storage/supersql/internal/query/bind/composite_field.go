package bind

import (
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// Anonymous field names require descriptor provenance, not just physical child types.

type recordSource uint8

const (
	recordUnspecified recordSource = iota
	recordConstructor
	recordVariable
	recordDeclared
)

func (b *binder) compositeField(s *scope, source expression, name string, pos int) (expression, error) {
	index, err := b.compositeFieldIndex(source, name, pos)
	if err != nil {
		return expression{}, err
	}
	return b.compositeFieldAt(s, source, index, pos)
}

func (b *binder) compositeFieldIndex(source expression, name string, pos int) (int, error) {
	if source.typ.Type.Storage != model.TypeComposite {
		return -1, bindError("42809", "field notation requires a composite type", pos)
	}
	index := -1
	if source.record == recordDeclared {
		for i, field := range source.recordFields {
			if field.Name == name {
				if index >= 0 {
					return -1, bindError("42702", "composite field reference is ambiguous", pos)
				}
				index = i
			}
		}
		if index >= 0 {
			if field := source.recordFields[index]; field.unsupported != "" {
				err := unsupported(FeatureID(field.unsupported), pos)
				if diagnostic, ok := err.(*model.Error); ok {
					diagnostic.Detail = field.unsupportedDetail
				}
				return -1, err
			}
			return index, nil
		}
		if source.recordFields != nil {
			return -1, bindError("42703", "composite field does not exist", pos)
		}
	}
	if source.typ.Type.ID == 2249 {
		if source.record == recordVariable {
			return -1, bindError("42809", "record type has not been registered", pos)
		}
		if source.record == recordConstructor {
			index = anonymousFieldIndex(name, len(source.typ.Children))
		}
	} else {
		fields, err := b.namedCompositeFields(source.typ, pos)
		if err != nil {
			return -1, err
		}
		for i, field := range fields {
			if err := b.ctx.Err(); err != nil {
				return -1, err
			}
			if field.Name == name {
				index = i
				break
			}
		}
	}
	if index < 0 {
		return -1, bindError("42703", "composite field does not exist", pos)
	}
	return index, nil
}

func (b *binder) compositeFieldAt(s *scope, source expression, index int, pos int) (expression, error) {
	typ := source.typ.Children[index]
	value := expression{typ: typ, nullable: true, position: pos}
	if len(source.recordFields) > index {
		value.record = source.recordFields[index].record
		value.recordFields = source.recordFields[index].recordFields
	}
	if typ.Type.Collation != 0 {
		value.collation = collationState{typ.Type.Collation, collationImplicit, pos}
	}
	return b.addScalar(s, value, ir.Scalar{Kind: ir.Field, FieldIndex: uint32(index)}, []expression{source})
}

func anonymousFieldIndex(name string, count int) int {
	if len(name) < 2 || name[0] != 'f' {
		return -1
	}
	if name[1] < '1' || name[1] > '9' {
		return -1
	}
	number, err := strconv.ParseUint(name[1:], 10, 32)
	if err != nil || number > uint64(count) {
		return -1
	}
	return int(number) - 1
}
