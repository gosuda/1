package builtin

import "github.com/gosuda/supersql-alpha/internal/model"

// TypeCollatable follows catalog collation support, including array element types.
func TypeCollatable(typ model.Type) bool {
	info, ok := LookupType(typ.ID)
	if !ok {
		return false
	}
	if info.Collation != 0 {
		return true
	}
	if typ.Storage == model.TypeArray && info.Element != 0 {
		element, found := LookupType(info.Element)
		return found && element.Collation != 0
	}
	return false
}

// InputCollation uses bound derivation when available. Zero preserves an
// indeterminate argument; the database default never resolves a conflict.
func InputCollation(bound model.CollationID, arguments []model.Type) model.CollationID {
	if bound != 0 {
		return bound
	}
	var collation model.CollationID
	for _, argument := range arguments {
		if !TypeCollatable(argument) {
			continue
		}
		if argument.Collation == 0 {
			return 0
		}
		if collation == 0 || collation == 100 {
			collation = argument.Collation
			continue
		}
		if argument.Collation != 100 && argument.Collation != collation {
			return 0
		}
	}
	return collation
}
