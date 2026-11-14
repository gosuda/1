package builtin

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)


func compilePostgresJSONPath(signature Signature, options ResolveOptions) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		switch signature.ID {
		case 4001:
			plan, err := CompileJSONPath(ctx, arguments[0].StrVal, jsonPathMemoryLimit(budget))
			if err != nil { return model.Value{}, err }
			remaining, claim, err := claimJSONPathPlan(plan, budget)
			if err != nil { return model.Value{}, err }
			if claim != nil { defer claim.Release() }
			binary, err := EncodeJSONPathBinary(ctx, plan, jsonPathMemoryLimit(remaining))
			if err != nil { return model.Value{}, err }
			if err := outputBudget(int64(len(binary)), budget.OutputBytes); err != nil { return model.Value{}, err }
			return model.BytesVal(binary), nil
		case 4002:
			return jsonPathReceive(ctx, arguments[0].BytesVal, budget)
		case 4003:
			plan, err := DecodeJSONPathBinary(ctx, arguments[0].BytesVal, jsonPathMemoryLimit(budget))
			if err != nil { return model.Value{}, err }
			remaining, claim, err := claimJSONPathPlan(plan, budget)
			if err != nil { return model.Value{}, err }
			if claim != nil { defer claim.Release() }
			return ownedText(ctx, min(budget.OutputBytes, int64(min(remaining.MemoryBytes, uint64(math.MaxInt64)))), plan.canonical)
		case 4004:
			plan, err := DecodeJSONPathBinary(ctx, arguments[0].BytesVal, jsonPathMemoryLimit(budget))
			if err != nil { return model.Value{}, err }
			remaining, claim, err := claimJSONPathPlan(plan, budget)
			if err != nil { return model.Value{}, err }
			if claim != nil { defer claim.Release() }
			binary, err := EncodeJSONPathBinary(ctx, plan, jsonPathMemoryLimit(remaining))
			if err != nil { return model.Value{}, err }
			if err := outputBudget(int64(len(binary)), budget.OutputBytes); err != nil { return model.Value{}, err }
			return model.BytesVal(binary), nil
		default:
			return executeJSONPathScalar(ctx, signature, arguments, budget, options)
		}
	}
}

// ReceiveJSONPath consumes PostgreSQL's native binary jsonpath frame without
// exposing the catalog's internal pseudo-type as a SQL-callable value.
func ReceiveJSONPath(ctx context.Context, data []byte, budget CallBudget) (model.Value, error) {
	return jsonPathReceive(ctx, data, budget)
}

func jsonPathReceive(ctx context.Context, data []byte, budget CallBudget) (model.Value, error) {
	plan, err := DecodeJSONPathBinary(ctx, data, jsonPathMemoryLimit(budget))
	if err != nil { return model.Value{}, err }
	remaining, claim, err := claimJSONPathPlan(plan, budget)
	if err != nil { return model.Value{}, err }
	if claim != nil { defer claim.Release() }
	binary, err := EncodeJSONPathBinary(ctx, plan, jsonPathMemoryLimit(remaining))
	if err != nil { return model.Value{}, err }
	if err := outputBudget(int64(len(binary)), budget.OutputBytes); err != nil { return model.Value{}, err }
	return model.BytesVal(binary), nil
}

func jsonPathMemoryLimit(budget CallBudget) uint64 {
	limit := budget.MemoryBytes
	if budget.Allocator != nil { limit = min(limit, budget.Allocator.AvailableBytes()) }
	return limit
}
func claimJSONPathPlan(plan *JSONPathPlan, budget CallBudget) (CallBudget, columnar.Allocation, error) {
	remaining, err := budget.After(plan.OwnedBytes())
	if err != nil { return CallBudget{}, nil, err }
	if budget.Allocator == nil { return remaining, nil, nil }
	claim, err := budget.Allocator.Reserve(plan.OwnedBytes())
	if err != nil { return CallBudget{}, nil, err }
	return remaining, claim, nil
}


func executeJSONPathScalar(ctx context.Context, signature Signature, arguments []model.Value, budget CallBudget, options ResolveOptions) (model.Value, error) {
	if len(arguments) < 2 { return model.Value{}, sqlError("42883", "jsonpath function requires a path argument") }
	plan, err := DecodeJSONPathBinary(ctx, arguments[1].BytesVal, jsonPathMemoryLimit(budget))
	if err != nil { return model.Value{}, err }
	budget, claim, err := claimJSONPathPlan(plan, budget)
	if err != nil { return model.Value{}, err }
	if claim != nil { defer claim.Release() }
	variables := "{}"
	if len(arguments) >= 3 { variables = arguments[2].StrVal }
	silent := false
	if len(arguments) >= 4 { silent = arguments[3].IntVal != 0 }
	useTZ := jsonPathFunctionUseTZ(signature.ID)
	switch signature.ID {
	case 4005, 4010, 1177:
		found := false
		err = executeJSONPathItems(ctx, arguments[0].StrVal, plan, variables, useTZ, budget, options, func(_ *jsonPathExecution, _ jsonPathItem) error { found = true; return errJSONPathStop })
		if errors.Is(err, errJSONPathStop) { err = nil }
		if err != nil {
			if silent && isSuppressibleJSONPathError(err) { return model.NullVal(model.TypeBoolean), nil }
			return model.Value{}, err
		}
		return model.BoolVal(found), nil
	case 4008, 2023:
		var result string
		err = ExecuteJSONPath(ctx, arguments[0].StrVal, plan, variables, useTZ, budget, options, func(value string) error { result = value; return errJSONPathStop })
		if errors.Is(err, errJSONPathStop) { err = nil }
		if err != nil {
			if silent && isSuppressibleJSONPathError(err) { return model.NullVal(model.TypeJSONB), nil }
			return model.Value{}, err
		}
		if result == "" { return model.NullVal(model.TypeJSONB), nil }
		return model.Value{Type: model.TypeJSONB, StrVal: result}, nil
	case 4007, 1180:
		var output strings.Builder
		output.WriteByte('[')
		first := true
		err = ExecuteJSONPath(ctx, arguments[0].StrVal, plan, variables, useTZ, budget, options, func(value string) error {
			added := len(value); if !first { added += 2 }
			if int64(output.Len()+added+1) > budget.OutputBytes { return sqlError("54000", "builtin output byte budget exceeded") }
			if !first { output.WriteString(", ") }
			first = false; output.WriteString(value)
			return nil
		})
		if err != nil && !(silent && isSuppressibleJSONPathError(err)) { return model.Value{}, err }
		output.WriteByte(']')
		return model.Value{Type: model.TypeJSONB, StrVal: output.String()}, nil
	case 4009, 4011, 2030:
		count := 0
		truth := model.NullVal(model.TypeBoolean)
		err = executeJSONPathItems(ctx, arguments[0].StrVal, plan, variables, useTZ, budget, options, func(_ *jsonPathExecution, item jsonPathItem) error {
			count++
			if count > 1 { return sqlError("22038", "single boolean result is expected") }
			if item.kind == jsonTextNull { truth = model.NullVal(model.TypeBoolean) } else if item.kind == jsonTextBoolean { truth = model.BoolVal(item.scalarText() == "true") } else { return sqlError("22038", "single boolean result is expected") }
			return nil
		})
		if err != nil {
			if silent && isSuppressibleJSONPathError(err) { return model.NullVal(model.TypeBoolean), nil }
			return model.Value{}, err
		}
		if count != 1 {
			if silent { return model.NullVal(model.TypeBoolean), nil }
			return model.Value{}, sqlError("22038", "single boolean result is expected")
		}
		return truth, nil
	default:
		return model.Value{}, &MissingImplementationError{Signature: signature}
	}
}
