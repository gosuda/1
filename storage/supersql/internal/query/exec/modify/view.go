package modify

import (
	"context"
	"fmt"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func (c *cursor) configureViewMutations(ctx context.Context) error {
	currentColumns := slices.Clone(c.columnIDs)
	currentLayouts := slices.Clone(c.tableLayouts)
	if len(c.options.ViewMutationSteps) != 0 && c.options.Partitions != nil {
		for i := range c.partitionViewValues {
			c.partitionViewValues[i] = make([]model.Value, len(c.tableLayouts))
		}
	}
	c.viewMutationSteps = make([]viewMutationState, len(c.options.ViewMutationSteps))
	for i, plan := range c.options.ViewMutationSteps {
		if plan.View == 0 || plan.Projection == nil || len(plan.InputColumns) == 0 || len(plan.OutputColumns) == 0 || len(plan.OutputColumns) != len(plan.Projection.OutputLayouts()) || plan.Check != (plan.Predicate != nil) {
			return runtime.ErrInvalidConfig
		}
		state := &c.viewMutationSteps[i]
		state.view = plan.View
		state.check = plan.Check
		state.outputColumns = slices.Clone(plan.OutputColumns)
		state.layouts = slices.Clone(plan.Projection.OutputLayouts())
		state.inputColumns = make([]int, len(plan.InputColumns))
		state.inputValues = make([]model.Value, len(plan.InputColumns))
		inputLayouts := make([]model.TypeLayout, len(plan.InputColumns))
		for j, column := range plan.InputColumns {
			position := slices.Index(currentColumns, column)
			if position < 0 {
				return runtime.ErrInvalidConfig
			}
			state.inputColumns[j] = position
			inputLayouts[j] = currentLayouts[position]
		}
		for j := range state.outputValues {
			state.outputValues[j] = make([]model.Value, len(state.layouts))
		}
		var err error
		state.projection, err = expr.NewState(plan.Projection, c.stateOptions())
		if err != nil {
			return err
		}
		if plan.Predicate != nil {
			state.predicate, err = expr.NewState(plan.Predicate, c.stateOptions())
			if err != nil {
				return err
			}
		}
		state.input, err = columnar.NewBuilder(inputLayouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: c.options.BatchBytes, Allocator: c.options.Query.Allocator(runtime.WorkMemory)})
		if err != nil {
			return err
		}
		currentColumns = state.outputColumns
		currentLayouts = state.layouts
	}
	c.options.ViewMutationSteps = nil
	return nil
}
func (c *cursor) partitionViewRow(route *partitionChild, row *model.Row, bank int) (*model.Row, error) {
	if row == nil || route == nil || c.options.Partitions == nil || len(c.viewMutationSteps) == 0 || bank < 0 || bank >= len(c.partitionViewValues) {
		return row, nil
	}
	values, err := c.options.Partitions.Project(route, row.Values, c.partitionViewValues[bank])
	if err != nil {
		return nil, err
	}
	c.partitionViewRows[bank] = model.Row{Key: row.Key, Values: values}
	return &c.partitionViewRows[bank], nil
}

func (c *cursor) projectViewRow(ctx context.Context, row *model.Row, enforce, output bool, bank int) (*model.Row, bool, error) {
	if row == nil || len(c.viewMutationSteps) == 0 {
		return row, false, nil
	}
	if bank < 0 || bank >= 2 {
		return nil, false, runtime.ErrInvalidConfig
	}
	last := len(c.viewMutationSteps) - 1
	if !output {
		last = -1
		for i := range c.viewMutationSteps {
			if enforce && c.viewMutationSteps[i].check {
				last = i
			}
		}
		if last < 0 {
			return row, false, nil
		}
	}
	current := row.Values
	owned := false
	for i := 0; i <= last; i++ {
		step := &c.viewMutationSteps[i]
		inputs := step.inputValues
		for j, position := range step.inputColumns {
			if position < 0 || position >= len(current) {
				return nil, owned, runtime.ErrInvalidConfig
			}
			inputs[j] = current[position]
		}
		step.input.Reset()
		appendErr := step.input.Append(ctx, inputs)
		clear(inputs)
		if appendErr != nil {
			return nil, owned, appendErr
		}
		var batch columnar.Batch
		if err := step.input.Publish(&batch); err != nil {
			return nil, owned, err
		}
		if enforce && step.check {
			var selected columnar.Batch
			if err := step.predicate.Eval(ctx, &batch, c.options.Parameters, &selected, expr.Direct); err != nil {
				return nil, owned, err
			}
			value, _, err := columnar.CopyValue(ctx, &selected.Columns[0], selected.Lane(0), c.options.MaxRecordBytes)
			if err != nil {
				return nil, owned, err
			}
			if value.IsNull || value.Type != model.TypeBoolean || value.IntVal == 0 {
				return nil, owned, &model.Error{Code: "44000", Message: "new row violates check option for view", Detail: fmt.Sprintf("view object %d", step.view)}
			}
		}
		if i == last && !output {
			return row, owned, nil
		}
		var projected columnar.Batch
		if err := step.projection.Eval(ctx, &batch, c.options.Parameters, &projected, expr.Direct); err != nil {
			return nil, owned, err
		}
		next := step.outputValues[bank]
		for j := range next {
			value, _, err := columnar.CopyValue(ctx, &projected.Columns[j], projected.Lane(0), c.options.MaxRecordBytes)
			if err != nil {
				return nil, owned, err
			}
			next[j] = value
		}
		current = next
		owned = true
	}
	c.viewRows[bank] = model.Row{Values: current}
	return &c.viewRows[bank], owned, nil
}
