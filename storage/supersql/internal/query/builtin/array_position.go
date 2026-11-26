package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileArrayPosition(signature Signature, element Callable, lookupErr error) scalarKernel {
	allPositions := false
	switch signature.ID {
	case 3277, 3278:
	case 3279:
		allPositions = true
	default:
		return nil
	}
	withStart := signature.ID == 3278
	return func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		noMatch := model.NullVal(model.TypeInt64)
		if allPositions {
			noMatch = model.Value{Type: model.TypeArray}
		}
		input, searched := arguments[0], arguments[1]
		if input.IsNull {
			return model.NullVal(noMatch.Type), nil
		}
		if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		if len(input.Dimensions) > 1 {
			return model.Value{}, sqlError("0A000", "searching for elements in multidimensional arrays is not supported")
		}
		if len(input.Dimensions) == 0 {
			return noMatch, nil
		}
		if searched.IsNull {
			hasNull, err := arrayPositionHasNull(ctx, input.ArrayVal)
			if err != nil {
				return model.Value{}, err
			}
			if !hasNull {
				return noMatch, nil
			}
		}
		lowerBound := int64(input.Dimensions[0].LowerBound)
		start := lowerBound
		if withStart {
			if arguments[2].IsNull {
				return model.Value{}, sqlError("22004", "initial position must not be null")
			}
			start = max(start, arguments[2].IntVal)
		}
		// Even a NULL search or a start beyond the end reaches PostgreSQL's lookup.
		if lookupErr != nil {
			return model.Value{}, lookupErr
		}
		positions := newArrayValueBuffer(1, budget)
		first := min(start-lowerBound, int64(len(input.ArrayVal)))
		var equalityArguments [2]model.Value
		for offset := range int64(len(input.ArrayVal)) - first {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			i := first + offset
			value := input.ArrayVal[i]
			matches := searched.IsNull && value.IsNull
			if !searched.IsNull && !value.IsNull {
				equalityArguments[0], equalityArguments[1] = searched, value
				nested, err := callBudget.After(uint64(cap(positions.values))*uint64(arrayValueBytes) + positions.payload)
				if err != nil {
					return model.Value{}, err
				}
				matches, err = arrayElementSearchEqual(ctx, element, &equalityArguments, nested)
				if err != nil {
					return model.Value{}, err
				}
			}
			if !matches {
				continue
			}
			position := model.Int64Val(lowerBound + i)
			if !allPositions {
				return position, nil
			}
			if err := positions.append(ctx, position); err != nil {
				return model.Value{}, err
			}
		}
		if !allPositions {
			return noMatch, nil
		}
		return positions.finish(ctx, []model.ArrayDimension{{LowerBound: 1, Length: int32(len(positions.values))}})
	}
}

func arrayPositionHasNull(ctx context.Context, values []model.Value) (bool, error) {
	for i := range values {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if values[i].IsNull {
			return true, nil
		}
	}
	return false, nil
}
