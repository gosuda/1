package builtin

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBNumericCast(signature Signature) scalarKernel {
	targetName := ""
	switch signature.ID {
	case 2580:
		targetName = "double precision"
	case 3450:
		targetName = "smallint"
	case 3451:
		targetName = "integer"
	case 3452:
		targetName = "bigint"
	case 3453:
		targetName = "real"
	case 3556:
		targetName = "boolean"
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (result model.Value, err error) {
		document, claim, budget, err := claimJSONBDocument(ctx, args[0].StrVal, budget)
		if err != nil {
			return model.Value{}, err
		}
		if claim != nil {
			defer claim.Release()
		}
		node := document.nodes[0]
		if node.kind == jsonTextNull {
			return model.NullVal(primitiveStorage(signature.Result)), nil
		}
		required := jsonTextNumber
		if signature.ID == 3556 {
			required = jsonTextBoolean
		}
		if node.kind != required {
			kind := ""
			switch node.kind {
			case jsonTextString:
				kind = "string"
			case jsonTextNumber:
				kind = "numeric"
			case jsonTextBoolean:
				kind = "boolean"
			case jsonTextArray:
				kind = "array"
			case jsonTextObject:
				kind = "object"
			}
			return model.Value{}, sqlError("22023", "cannot cast jsonb "+kind+" to type "+targetName)
		}
		if required == jsonTextBoolean {
			return model.BoolVal(node.text == "true"), nil
		}
		work, err := budget.NewWorkspace(ctx)
		if err != nil {
			return model.Value{}, numericAllocationError(err)
		}
		defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
		value, err := parseNumeric(ctx, work, node.text)
		if err != nil {
			return model.Value{}, err
		}
		return numericToPrimitive(ctx, work, value, signature.Result, budget)
	}
}
