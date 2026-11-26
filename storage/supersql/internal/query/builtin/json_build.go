package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBuild(signature Signature) scalarKernel {
	switch signature.ID {
	case 3202, 3203:
		return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
			return jsonObjectArrayDocument(ctx, args, budget, false)
		}
	case 3199, 3201:
		return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
			return jsonBuildDocument(ctx, budget, false, func(writer *jsonbBuildWriter) error {
				if signature.ID == 3199 {
					return writer.raw("[]")
				}
				return writer.raw("{}")
			})
		}
	default:
		return nil
	}
}
