package builtin

import (
	"cmp"
	"context"
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileArrayComparison(signature Signature, element Callable, lookupErr error) scalarKernel {
	var op primitiveOp
	switch signature.ID {
	case 744:
		op = opEq
	case 390:
		op = opNe
	case 391:
		op = opLt
	case 392:
		op = opGt
	case 393:
		op = opLe
	case 396:
		op = opGe
	case 382:
		op = opCmp
	case 515, 516:
		op = opCmp
	default:
		return nil
	}
	equality := op == opEq || op == opNe
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		left, right := args[0], args[1]
		if err := model.ValidateArrayShape(left.Dimensions, uint64(len(left.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		if err := model.ValidateArrayShape(right.Dimensions, uint64(len(right.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		if equality && !slices.Equal(left.Dimensions, right.Dimensions) {
			return comparisonResult(1, op), nil
		}
		// PostgreSQL resolves equality only after the dimensionality fast path.
		if lookupErr != nil {
			return model.Value{}, lookupErr
		}
		order, err := compareArrayElements(ctx, left.ArrayVal, right.ArrayVal, element, equality, callBudget)
		if err != nil {
			return model.Value{}, err
		}
		if order == 0 && !equality {
			order = compareArrayDimensions(left, right)
		}
		if signature.ID == 515 || signature.ID == 516 {
			selected := left
			if signature.ID == 515 && order < 0 || signature.ID == 516 && order > 0 { selected = right }
			owned, _, err := model.CopyValue(ctx, selected, uint64(max(callBudget.OutputBytes, 0)))
			if err != nil { return model.Value{}, &model.Error{Code: "54000", Message: "array extremum exceeds output budget", Cause: err} }
			return owned, nil
		}
		return comparisonResult(order, op), nil
	}
}

func compareArrayElements(ctx context.Context, left, right []model.Value, element Callable, equality bool, callBudget CallBudget) (int, error) {
	var arguments [2]model.Value
	for i := range min(len(left), len(right)) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if left[i].IsNull && right[i].IsNull {
			continue
		}
		if left[i].IsNull {
			return 1, nil
		}
		if right[i].IsNull {
			return -1, nil
		}
		arguments[0], arguments[1] = left[i], right[i]
		if equality {
			matched, err := arrayElementEqual(ctx, element, &arguments, callBudget)
			if err != nil {
				return 0, err
			}
			if !matched {
				return 1, nil
			}
			continue
		}
		result, err := element.Call(ctx, arguments[:], callBudget.WithOutput(0))
		clear(arguments[:])
		if err != nil {
			return 0, err
		}
		if result.IsNull || result.Type != model.TypeInt64 || result.IntVal < math.MinInt32 || result.IntVal > math.MaxInt32 {
			return 0, sqlError("XX000", "array element comparison function did not return nonnull int4")
		}
		if result.IntVal != 0 {
			return cmp.Compare(result.IntVal, 0), nil
		}
	}
	return 0, nil
}

func compareArrayDimensions(left, right model.Value) int {
	if order := cmp.Compare(len(left.ArrayVal), len(right.ArrayVal)); order != 0 {
		return order
	}
	if order := cmp.Compare(len(left.Dimensions), len(right.Dimensions)); order != 0 {
		return order
	}
	for i, dimension := range left.Dimensions {
		if order := cmp.Compare(dimension.Length, right.Dimensions[i].Length); order != 0 {
			return order
		}
	}
	for i, dimension := range left.Dimensions {
		if order := cmp.Compare(dimension.LowerBound, right.Dimensions[i].LowerBound); order != 0 {
			return order
		}
	}
	return 0
}
