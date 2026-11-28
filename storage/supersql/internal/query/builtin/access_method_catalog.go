package builtin

import (
	"iter"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type AccessMethodOperatorInfo struct {
	OID         model.OID
	Family      model.OID
	Left, Right model.TypeID
	Strategy    uint16
	Purpose     byte
	Operator    model.OperatorID
	Method      model.OID
	SortFamily  model.OID
}

type AccessMethodProcedureInfo struct {
	OID         model.OID
	Family      model.OID
	Left, Right model.TypeID
	Number      uint16
	Procedure   model.FunctionID
}

var generatedAccessMethodOperators []AccessMethodOperatorInfo
var generatedAccessMethodProcedures []AccessMethodProcedureInfo

func registerAccessMethodCatalog(operators []AccessMethodOperatorInfo, procedures []AccessMethodProcedureInfo) {
	generatedAccessMethodOperators = operators
	generatedAccessMethodProcedures = procedures
}

func AccessMethodCatalogReady() bool {
	return generatedAccessMethodOperators != nil && generatedAccessMethodProcedures != nil
}

func AccessMethodOperators() iter.Seq[AccessMethodOperatorInfo] {
	return func(yield func(AccessMethodOperatorInfo) bool) {
		for _, info := range generatedAccessMethodOperators {
			if !yield(info) {
				return
			}
		}
	}
}

func AccessMethodProcedures() iter.Seq[AccessMethodProcedureInfo] {
	return func(yield func(AccessMethodProcedureInfo) bool) {
		for _, info := range generatedAccessMethodProcedures {
			if !yield(info) {
				return
			}
		}
	}
}
