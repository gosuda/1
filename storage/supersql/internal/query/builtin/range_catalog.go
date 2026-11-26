package builtin

import (
	"iter"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// RangeFamilyInfo is the pinned relationship between one range, its multirange,
// and its declared subtype. It carries no query, catalog snapshot, or values.
type RangeFamilyInfo struct {
	Range          model.TypeID
	Multirange     model.TypeID
	Subtype        model.TypeID
	SubtypeClass   model.OID
	Collation      model.CollationID
	Canonical      model.FunctionID
	Difference     model.FunctionID
	SubtypeStorage model.DataType
	Representation model.TypeID
	canonicalCall  TypeScalarCall
	differenceCall TypeScalarCall
	enum           *ResolvedType
}

// LookupRangeFamily accepts either a range or multirange identity.
func LookupRangeFamily(id model.TypeID) (RangeFamilyInfo, bool) {
	for _, family := range rangeFamilies {
		if family.Range == id || family.Multirange == id {
			return family, true
		}
	}
	return RangeFamilyInfo{}, false
}

func (family RangeFamilyInfo) subtypeStorage() model.DataType {
	if family.SubtypeStorage != model.TypeAny {
		return family.SubtypeStorage
	}
	return primitiveStorage(family.Subtype)
}
func (family RangeFamilyInfo) textStorage() bool {
	switch family.subtypeStorage() {
	case model.TypeNumeric, model.TypeString, model.TypeUUID:
		return true
	default:
		return false
	}
}

func (family RangeFamilyInfo) representation() model.TypeID {
	if family.Representation != 0 {
		return family.Representation
	}
	return family.Subtype
}

func RangeFamilies() iter.Seq[RangeFamilyInfo] {
	return func(yield func(RangeFamilyInfo) bool) {
		for _, family := range rangeFamilies {
			if !yield(family) {
				return
			}
		}
	}
}
