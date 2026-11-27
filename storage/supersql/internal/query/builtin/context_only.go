package builtin

import (
	"context"
	"fmt"

	"github.com/gosuda/supersql-alpha/internal/model"
)
// featureStatisticsCatalogMutation names the embedded_pg18 profile exclusion
// the pg_clear_*_stats kernels raise for want of a mutable statistics
// catalog. The bind package owns the canonical FeatureID
// (bind.StatisticsCatalogMutation) but cannot be imported here: bind
// imports builtin. gates_test.go's manifest-driven exclusion table fails if
// this id drifts from testdata/postgres/embedded_features.json.
const featureStatisticsCatalogMutation = "statistics_catalog_mutation"

func compileContextOnly(signature Signature) scalarKernel {
	if signature.ID == 2298 || signature.ID == 2299 {
		return func(context.Context, []model.Value, CallBudget) (model.Value, error) {
			return model.StringVal(""), nil
		}
	}
	if kernel := compileSQLCallableSupport(signature); kernel != nil {
		return kernel
	}
	requirement := contextOnlyRequirement(signature)
	featureID := contextOnlyFeatureID(signature)
	return func(context.Context, []model.Value, CallBudget) (model.Value, error) {
		return model.Value{}, &model.Error{Code: "0A000", Message: fmt.Sprintf("%s requires %s", signature.Name, requirement), FeatureID: featureID}
	}
}

func contextOnlyFeatureID(signature Signature) string {
	switch signature.ID {
	case 6362, 6363, 6397, 6398:
		return featurePhysicalMaintenance
	default:
		return ""
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
