package builtin

import (
	"iter"
	"sort"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Operator identifies a PostgreSQL operator independently of its function kernel.
type Operator struct {
	ID                          model.OperatorID
	Name                        string
	Left, Right, Result         model.TypeID
	Function                    model.FunctionID
	Commutator, Negator         model.OperatorID
	Restriction, Join           model.FunctionID
	HashJoinable, MergeJoinable bool
}

func OperatorCatalogMetadataReady() bool {
	for _, operator := range operatorCatalog {
		if operator.Restriction != 0 || operator.Join != 0 {
			return true
		}
	}
	return false
}

type CastInfo struct {
	OID             model.OID
	Source, Target  model.TypeID
	Function        model.FunctionID
	Context, Method byte
}

// TypeInfo is catalog identity/category metadata, not an executable value codec.
type TypeInfo struct {
	ID                   model.TypeID
	Name                 string
	Category             byte
	Preferred            bool
	Element, Array, Base model.TypeID
	Kind                 byte
	Collation            model.CollationID
	Input, Output, Receive, Send        model.FunctionID
	ModifierInput, ModifierOutput       model.FunctionID
	Subscript, Analyze                  model.FunctionID
	Delimiter                           byte
}

// IsArray includes record[], whose catalog category is pseudo rather than array.
func (info TypeInfo) IsArray() bool {
	sharedCodec := info.Input == 750 && info.Output == 751
	return info.Element != 0 && (info.Category == 'A' || sharedCodec)
}

func TypeCatalogMetadataReady() bool {

	integer, ok := LookupType(23)
	if !ok || integer.Receive == 0 || integer.Send == 0 {
		return false
	}
	array, ok := LookupType(1007)
	return ok && array.Receive != 0 && array.Send != 0 && array.Subscript != 0 && array.Analyze != 0
}
// BuiltinTypeLayout returns the executable storage layout for a pg_catalog type.
func BuiltinTypeLayout(id model.TypeID, modifier int32) (model.TypeLayout, bool) {
	info, known := LookupType(id)
	if !known { return model.TypeLayout{}, false }
	layout := model.TypeLayout{Type: model.Type{ID: id, Modifier: modifier, Collation: info.Collation, Storage: primitiveStorage(id)}}
	if info.IsArray() {
		element, ok := BuiltinTypeLayout(info.Element, modifier)
		if !ok { return model.TypeLayout{}, false }
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{element}
	}
	if layout.Type.Storage == model.TypeAny { return model.TypeLayout{}, false }
	return layout, true
}

// CollationInfo describes a pinned pg_catalog identity, not execution support.
type CollationInfo struct {
	ID            model.CollationID
	Name          string
	Provider      byte
	Deterministic bool
	Encoding      int32
}

func LookupCollation(id model.CollationID) (CollationInfo, bool) {
	i := sort.Search(len(collationCatalog), func(i int) bool { return collationCatalog[i].ID >= id })
	if i == len(collationCatalog) || collationCatalog[i].ID != id {
		return CollationInfo{}, false
	}
	return collationCatalog[i], true
}

func Collations() iter.Seq[CollationInfo] {
	return func(yield func(CollationInfo) bool) {
		for _, info := range collationCatalog {
			if !yield(info) {
				return
			}
		}
	}
}

// LookupCollationName matches an exact collname; callers resolve the namespace.
func LookupCollationName(name string) (CollationInfo, bool) {
	for _, info := range collationCatalog {
		if info.Name == name {
			return info, true
		}
	}
	return CollationInfo{}, false
}

func LookupOperator(id model.OperatorID) (Operator, bool) {
	i := sort.Search(len(operatorCatalog), func(i int) bool { return operatorCatalog[i].ID >= id })
	if i == len(operatorCatalog) || operatorCatalog[i].ID != id {
		return Operator{}, false
	}
	return operatorCatalog[i], true
}

func Operators() iter.Seq[Operator] {
	return func(yield func(Operator) bool) {
		for _, operator := range operatorCatalog {
			if !yield(operator) {
				return
			}
		}
	}
}
func OperatorsNamed(name string) iter.Seq[Operator] {
	return func(yield func(Operator) bool) {
		first := sort.Search(len(operatorNames), func(i int) bool { return operatorCatalog[operatorNames[i]].Name >= name })
		for _, index := range operatorNames[first:] {
			operator := operatorCatalog[index]
			if operator.Name != name || !yield(operator) {
				return
			}
		}
	}
}
func LookupCast(source, target model.TypeID) (CastInfo, bool) {
	i := sort.Search(len(castCatalog), func(i int) bool {
		cast := castCatalog[i]
		return cast.Source > source || cast.Source == source && cast.Target >= target
	})
	if i == len(castCatalog) || castCatalog[i].Source != source || castCatalog[i].Target != target {
		return CastInfo{}, false
	}
	return castCatalog[i], true
}

func Casts() iter.Seq[CastInfo] {
	return func(yield func(CastInfo) bool) {
		for _, cast := range castCatalog {
			if !yield(cast) {
				return
			}
		}
	}
}
func LookupType(id model.TypeID) (TypeInfo, bool) {
	i := sort.Search(len(typeCatalog), func(i int) bool { return typeCatalog[i].ID >= id })
	if i == len(typeCatalog) || typeCatalog[i].ID != id {
		return TypeInfo{}, false
	}
	return typeCatalog[i], true
}

func Types() iter.Seq[TypeInfo] {
	return func(yield func(TypeInfo) bool) {
		for _, info := range typeCatalog {
			if !yield(info) {
				return
			}
		}
	}
}
func TypesNamed(name string) iter.Seq[TypeInfo] {
	return func(yield func(TypeInfo) bool) {
		first := sort.Search(len(typeNames), func(i int) bool { return typeCatalog[typeNames[i]].Name >= name })
		for _, index := range typeNames[first:] {
			info := typeCatalog[index]
			if info.Name != name || !yield(info) {
				return
			}
		}
	}
}
