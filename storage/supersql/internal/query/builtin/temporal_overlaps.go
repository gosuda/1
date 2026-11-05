package builtin

import (
	"context"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type temporalEndpoint struct {
	value int64
	null  bool
}
type temporalPeriod struct {
	start, end       int64
	endNull, unknown bool
}

func compileTemporalOverlaps(signature Signature) scalarKernel {
	switch signature.ID {
	case 1304, 1305, 1306, 1307, 1308, 1309, 1310, 1311, 2041, 2042, 2043, 2044:
	default:
		return nil
	}
	return func(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		var endpoints [4]temporalEndpoint
		for index := range endpoints {
			endpoints[index] = temporalEndpoint{value: args[index].IntVal, null: args[index].IsNull}
		}
		for _, index := range [2]int{1, 3} {
			if signature.Arguments[index] != 1186 {
				continue
			}
			if args[index-1].IsNull || args[index].IsNull {
				endpoints[index] = temporalEndpoint{null: true}
				continue
			}
			var end model.Value
			var err error
			if signature.Arguments[0] == 1083 {
				end, err = addTimeInterval(args[index-1].IntVal, args[index].IntervalVal, false)
			} else {
				end, err = timestampAddInterval(args[index-1].IntVal, args[index].IntervalVal)
			}
			if err != nil {
				return model.Value{}, err
			}
			endpoints[index] = temporalEndpoint{value: end.IntVal, null: end.IsNull}
		}
		left := orderedTemporalPeriod(endpoints[0], endpoints[1])
		right := orderedTemporalPeriod(endpoints[2], endpoints[3])
		if left.unknown || right.unknown {
			return model.NullVal(model.TypeBoolean), nil
		}
		if left.start > right.start {
			left, right = right, left
		}
		if left.start < right.start {
			if left.endNull {
				return model.NullVal(model.TypeBoolean), nil
			}
			if right.start < left.end {
				return model.BoolVal(true), nil
			}
			if right.endNull {
				return model.NullVal(model.TypeBoolean), nil
			}
			return model.BoolVal(false), nil
		}
		if left.endNull || right.endNull {
			return model.NullVal(model.TypeBoolean), nil
		}
		return model.BoolVal(true), nil
	}
}

func orderedTemporalPeriod(start, end temporalEndpoint) temporalPeriod {
	if start.null {
		if end.null {
			return temporalPeriod{unknown: true}
		}
		return temporalPeriod{start: end.value, endNull: true}
	}
	if !end.null && start.value > end.value {
		start, end = end, start
	}
	return temporalPeriod{start: start.value, end: end.value, endNull: end.null}
}
