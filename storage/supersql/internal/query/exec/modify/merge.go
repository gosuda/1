package modify

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type mergeActionState struct {
	predicate  *expr.State
	assignment *expr.State
	columns    []int
}

func (c *cursor) configureMerge(ctx context.Context) error {
	o := &c.options
	plan := *o.Merge
	if plan.SourcePresent == plan.TargetPresent || len(plan.Actions) == 0 {
		return runtime.ErrInvalidConfig
	}
	for _, position := range []int{plan.SourcePresent, plan.TargetPresent} {
		if position < 0 || position >= len(o.InputLayouts) || o.InputLayouts[position].Type.Storage != model.TypeBoolean {
			return storage.ErrInvalidLayout
		}
	}
	plan.Actions = slices.Clone(plan.Actions)
	o.Merge = &plan
	c.mergeActions = make([]mergeActionState, len(plan.Actions))
	var unconditional [4]bool
	for i := range plan.Actions {
		action := &plan.Actions[i]
		state := &c.mergeActions[i]
		if action.Match < ir.Matched || action.Match > ir.NotMatchedBySource || unconditional[action.Match] {
			return runtime.ErrInvalidConfig
		}
		if action.Action != ir.Insert && action.Action != ir.Update && action.Action != ir.Delete && action.Action != ir.DoNothing {
			return runtime.ErrInvalidConfig
		}
		if action.Match == ir.NotMatchedByTarget {
			if action.Action != ir.Insert && action.Action != ir.DoNothing {
				return runtime.ErrInvalidConfig
			}
		} else if action.Action == ir.Insert {
			return runtime.ErrInvalidConfig
		}
		if action.Predicate == nil {
			unconditional[action.Match] = true
		} else {
			layouts := action.Predicate.OutputLayouts()
			if len(layouts) != 1 || layouts[0].Type.Storage != model.TypeBoolean {
				return storage.ErrInvalidLayout
			}
			var err error
			state.predicate, err = expr.NewState(action.Predicate, c.stateOptions())
			if err != nil {
				return err
			}
		}
		action.AssignmentColumns = slices.Clone(action.AssignmentColumns)
		if action.Action == ir.Delete || action.Action == ir.DoNothing {
			if action.Assignment != nil || len(action.AssignmentColumns) != 0 {
				return runtime.ErrInvalidConfig
			}
			continue
		}
		if action.Action == ir.Update && action.Assignment == nil && len(action.AssignmentColumns) == 0 {
			continue
		}
		if action.Assignment == nil || len(action.AssignmentColumns) == 0 {
			return runtime.ErrInvalidConfig
		}
		layouts := action.Assignment.OutputLayouts()
		if len(layouts) != len(action.AssignmentColumns) {
			return storage.ErrInvalidLayout
		}
		state.columns = make([]int, len(layouts))
		for j, column := range action.AssignmentColumns {
			index := slices.Index(c.columnIDs, column)
			if index < 0 || slices.Contains(action.AssignmentColumns[:j], column) || !model.TypeLayoutsEqual(layouts[j], c.tableLayouts[index]) {
				return storage.ErrInvalidLayout
			}
			state.columns[j] = index
		}
		var err error
		state.assignment, err = expr.NewState(action.Assignment, c.stateOptions())
		if err != nil {
			return err
		}
	}
	layouts := make([]model.TypeLayout, len(c.keyColumns))
	for i, column := range c.keyColumns {
		layouts[i] = c.tableLayouts[column]
	}
	var err error
	c.mutatedTargets, err = distinctset.New(ctx, distinctset.Options{Query: o.Query, Encoder: o.Encoder, Layouts: layouts, MemoryBytes: o.WorkBytes / 16, WorkBytes: o.WorkBytes / 2, MaxKeyBytes: o.MaxRecordBytes, MaxKeys: o.MaxRows, MaxLevels: 64})
	return err
}

func (c *cursor) mergeMatch(input []model.Value) (ir.MatchKind, error) {
	plan := c.options.Merge
	source, target := input[plan.SourcePresent], input[plan.TargetPresent]
	for _, marker := range []model.Value{source, target} {
		if marker.Type != model.TypeBoolean || (!marker.IsNull && marker.IntVal != 1) {
			return 0, storage.ErrInvalidLayout
		}
	}
	if source.IsNull && target.IsNull {
		return 0, storage.ErrInvalidLayout
	}
	if target.IsNull {
		return ir.NotMatchedByTarget, nil
	}
	if source.IsNull {
		return ir.NotMatchedBySource, nil
	}
	return ir.Matched, nil
}

func (c *cursor) mergePredicate(ctx context.Context, state *expr.State, input []model.Value) (bool, error) {
	c.assignmentInput.Reset()
	if err := c.assignmentInput.Append(ctx, input); err != nil {
		return false, err
	}
	var batch, output columnar.Batch
	if err := c.assignmentInput.Publish(&batch); err != nil {
		return false, err
	}
	if err := state.Eval(ctx, &batch, c.options.Parameters, &output, expr.Direct); err != nil {
		return false, err
	}
	if output.Rows() != 1 || len(output.Columns) != 1 {
		return false, storage.ErrInvalidLayout
	}
	value, _, err := columnar.CopyValue(ctx, &output.Columns[0], output.Lane(0), c.options.MaxRecordBytes)
	return !value.IsNull && value.IntVal == 1, err
}

func (c *cursor) merge(ctx context.Context, frozen []model.Value) (bool, error) {
	width := len(c.options.InputLayouts)
	match, err := c.mergeMatch(frozen[:width])
	if err != nil {
		return false, err
	}
	for i, action := range c.options.Merge.Actions {
		if err := c.check(ctx); err != nil {
			return false, err
		}
		if action.Match != match {
			continue
		}
		state := &c.mergeActions[i]
		if state.predicate != nil {
			selected, err := c.mergePredicate(ctx, state.predicate, frozen[:width])
			if err != nil {
				return false, err
			}
			if !selected {
				continue
			}
		}
		if action.Action == ir.DoNothing {
			return false, nil
		}
		if action.Action == ir.Insert {
			return c.insert(ctx, frozen, state.assignment, state.columns, action.AssignmentColumns)
		}
		operation := storage.MutationUpdate
		for j, column := range c.keyColumns {
			c.keyValues[j] = frozen[width+column]
		}
		inserted, err := c.mutatedTargets.Insert(ctx, c.keyValues)
		if err != nil {
			return false, err
		}
		if !inserted {
			return false, &model.Error{Code: "21000", Message: "MERGE command cannot affect row a second time"}
		}
		if action.Action == ir.Delete {
			operation = storage.MutationDelete
		}
		mutated, err := c.mutate(ctx, frozen, operation, state.assignment, state.columns, action.AssignmentColumns)
		if err != nil {
			return false, err
		}
		return mutated, nil
	}
	return false, nil
}
