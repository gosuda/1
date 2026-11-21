package builtin

import "github.com/gosuda/supersql-alpha/internal/model"

// ScalarMayError describes SQL value errors, not cancellation or resource
// failures. Unknown and allocation-bearing kernels remain conservative.
func ScalarMayError(id model.FunctionID) bool {
	signature, known := LookupSignature(id)
	if !known || signature.ArgumentCount != 2 {
		return true
	}
	switch id {
	// Date, time, timestamp, timestamptz comparisons.
	case 1086, 1087, 1088, 1089, 1090, 1091, 1092, 1102, 1103, 1104, 1105, 1106, 1107, 1145:
		return false
	case 1152, 1153, 1154, 1155, 1156, 1157, 1314, 2045, 2052, 2053, 2054, 2055, 2056, 2057:
		return false
	// OID, bytea, numeric, UUID, and enum comparisons on validated typed values
	// never raise SQL value errors.
	case 184, 185, 356, 716, 717, 1638, 1639:
		return false
	case 1718, 1719, 1720, 1721, 1722, 1723, 1769:
		return false
	case 1948, 1949, 1950, 1951, 1952, 1953, 1954:
		return false
	case 2954, 2955, 2956, 2957, 2958, 2959, 2960:
		return false
	case 3508, 3509, 3510, 3511, 3512, 3513, 3514:
		return false
	}
	operation := primitiveOperation(id)
	if !isComparison(operation) && operation != opBtreeComparison {
		return true
	}
	for _, typ := range signature.Arguments[:2] {
		if !isInteger(typ) && !isFloat(typ) && typ != 16 {
			return true
		}
	}
	return false
}

// ScalarMayErrorWithCollation refines ScalarMayError when the binder knows the
// expression's input collation and the database's default collation. Text and
// bpchar comparisons cannot raise a collation error once resolved to C/POSIX.
func ScalarMayErrorWithCollation(id model.FunctionID, collation, defaultCollation model.CollationID) bool {
	if !ScalarMayError(id) {
		return false
	}
	switch id {
	case 67, 157, 360, 740, 741, 742, 743, 1048, 1049, 1050, 1051, 1052, 1053, 1078:
		effective := collation
		if effective == 100 {
			effective = defaultCollation
		}
		return !IsSupportedCollation(effective, nil)
	}
	return true
}
