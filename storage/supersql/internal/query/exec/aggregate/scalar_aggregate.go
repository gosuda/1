package aggregate

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type scalarAggregatePlan struct {
	initial builtin.Callable
	combine builtin.Callable
}

func isScalarAggregate(function model.FunctionID) bool {
	return function == 2112 || function == 2242 || function == 2243 || function == 6167
}

func compileScalarAggregate(function model.FunctionID) (*scalarAggregatePlan, error) {
	plan := new(scalarAggregatePlan)
	transition := model.FunctionID(894)
	switch function {
	case 2242:
		transition = 1673
	case 2243:
		transition = 1674
	case 6167:
		transition = 1675
	}
	var err error
	plan.combine, err = builtin.Resolve(transition, builtin.ResolveOptions{})
	if err != nil {
		return nil, err
	}
	if function != 2112 {
		// bit_send validates and owns the canonical payload without changing it.
		// XOR cannot initialize by combining a value with itself.
		plan.initial, err = builtin.Resolve(2457, builtin.ResolveOptions{})
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func (c *cursor) advanceScalarAggregate(ctx context.Context, call compiledCall, state *transition, value model.Value) error {
	budget := builtin.CallBudget{OutputBytes: int64(c.options.MaxRecordBytes), MemoryBytes: c.options.MaxRecordBytes}
	var result model.Value
	var err error
	if call.inputs[0].ID == 1560 && state.value.IsNull {
		result, err = call.scalar.initial.Call(ctx, []model.Value{value}, budget)
	} else {
		left := state.value
		if left.IsNull {
			left = model.Int64Val(0)
		}
		result, err = call.scalar.combine.Call(ctx, []model.Value{left, value}, budget)
	}
	if err != nil {
		return err
	}
	state.value = result
	state.count++
	return nil
}
