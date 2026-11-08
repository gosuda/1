package builtin

import (
	"iter"
	"sort"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// OperatorClassInfo describes a pinned pg_catalog identity, not an executable key codec.
type OperatorClassInfo struct {
	ID                   model.OID
	Name                 string
	Method               string
	InputType            model.TypeID
	Family               model.OID
	FamilyName           string
	Default              bool
	KeyType              model.TypeID
	EqualityFunction     model.FunctionID
	CompareFunction      model.FunctionID
	HashFunction         model.FunctionID
	HashExtendedFunction model.FunctionID
}

func LookupOperatorClass(id model.OID) (OperatorClassInfo, bool) {
	i := sort.Search(len(operatorClassCatalog), func(i int) bool { return operatorClassCatalog[i].ID >= id })
	if i == len(operatorClassCatalog) || operatorClassCatalog[i].ID != id {
		return OperatorClassInfo{}, false
	}
	return operatorClassCatalog[i], true
}

func OperatorClasses() iter.Seq[OperatorClassInfo] {
	return func(yield func(OperatorClassInfo) bool) {
		for _, info := range operatorClassCatalog {
			if !yield(info) {
				return
			}
		}
	}
}

// LookupOperatorClassName matches exact method and opcname; callers resolve the namespace.
func LookupOperatorClassName(method, name string) (OperatorClassInfo, bool) {
	i := sort.Search(len(operatorClassNames), func(i int) bool {
		info := operatorClassCatalog[operatorClassNames[i]]
		return info.Method > method || info.Method == method && info.Name >= name
	})
	if i == len(operatorClassNames) {
		return OperatorClassInfo{}, false
	}
	info := operatorClassCatalog[operatorClassNames[i]]
	if info.Method != method || info.Name != name {
		return OperatorClassInfo{}, false
	}
	return info, true
}

// LookupDefaultOperatorClass requires an exact input type, without binary-coercible aliases.
func LookupDefaultOperatorClass(method string, input model.TypeID) (OperatorClassInfo, bool) {
	i := sort.Search(len(operatorClassDefaults), func(i int) bool {
		info := operatorClassCatalog[operatorClassDefaults[i]]
		return info.Method > method || info.Method == method && info.InputType >= input
	})
	if i == len(operatorClassDefaults) {
		return OperatorClassInfo{}, false
	}
	info := operatorClassCatalog[operatorClassDefaults[i]]
	if info.Method != method || info.InputType != input {
		return OperatorClassInfo{}, false
	}
	return info, true
}
