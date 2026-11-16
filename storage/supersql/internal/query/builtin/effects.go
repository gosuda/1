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
	case 1086, 1087, 1088, 1089, 1090, 1091, 1092, 1102, 1103, 1104, 1105, 1106, 1107, 1145:
		return false
	case 1152, 1153, 1154, 1155, 1156, 1157, 1314, 2045, 2052, 2053, 2054, 2055, 2056, 2057:
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
