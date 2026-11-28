package builtin

import (
	"context"
	"fmt"

	"github.com/gosuda/supersql-alpha/internal/model"
)
func compileContextOnly(signature Signature) scalarKernel {
	if kernel := compileSQLCallableSupport(signature); kernel != nil {
		return kernel
	}
	requirement := contextOnlyRequirement(signature)
	return func(context.Context, []model.Value, CallBudget) (model.Value, error) {
		return model.Value{}, sqlError("0A000", fmt.Sprintf("%s requires %s", signature.Name, requirement))
	}
}

func contextOnlyRequirement(signature Signature) string {
	switch signature.ID {
	case 2246, 2247, 2248:
		return "a pinned function-catalog validation context"
	case 3970, 3971, 3973, 3975, 3977, 3979, 3981, 3983, 3985, 3987, 3989, 3991, 3993:
		return "a bound ordered-set aggregate executor"
	case 4299, 4300, 6225, 6226:
		return "a bound range aggregate executor"
	case 3121:
		return "a PostgreSQL void pseudo-value"
	case 6362, 6363, 6397, 6398:
		return "a mutable statistics-catalog context"
	}
	for index := 0; index < int(signature.ArgumentCount); index++ {
		if requirement := pseudoTypeRequirement(signature.Arguments[index]); requirement != "" { return requirement }
	}
	if requirement := pseudoTypeRequirement(signature.Result); requirement != "" { return requirement }
	return "its PostgreSQL executor callback context"
}

func pseudoTypeRequirement(id model.TypeID) string {
	switch id {
	case 2281:
		return "a PostgreSQL server-internal callback context"
	case 2275:
		return "a PostgreSQL type-I/O cstring context"
	case 2279:
		return "an active trigger invocation"
	case 3838:
		return "an active event-trigger invocation"
	case 2280, 3115, 325, 3310, 269:
		return "a PostgreSQL handler callback context"
	default:
		return ""
	}
}
