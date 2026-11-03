package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	exmodify "github.com/gosuda/supersql-alpha/internal/query/exec/modify"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) mergePlan(node physical.NodeSpec, graph *ir.Graph, relation ir.Rel, slots []physical.Slot) (*exmodify.MergePlan, error) {
	if err := c.charge(uint64(unsafe.Sizeof(exmodify.MergePlan{})) + uint64(relation.ActionCount())*uint64(unsafe.Sizeof(exmodify.MergeAction{}))); err != nil {
		return nil, err
	}
	plan := &exmodify.MergePlan{SourcePresent: -1, TargetPresent: -1, Actions: make([]exmodify.MergeAction, relation.ActionCount())}
	for i, slot := range slots {
		if slot.ID == relation.MergeSourcePresent {
			plan.SourcePresent = i
		}
		if slot.ID == relation.MergeTargetPresent {
			plan.TargetPresent = i
		}
	}
	if plan.SourcePresent < 0 || plan.TargetPresent < 0 || relation.TargetKeyCount() == 0 {
		return nil, ErrInvalid
	}
	for i := range plan.Actions {
		action, ok := graph.MergeAction(node.Logical.Rel, uint32(i))
		if !ok {
			return nil, ErrInvalid
		}
		compiled := &plan.Actions[i]
		compiled.Match, compiled.Action = action.Match, action.Action
		var err error
		if action.Condition != 0 {
			compiled.Predicate, err = c.program(node.Logical.Graph, []ir.ScalarID{action.Condition}, slots)
			if err != nil {
				return nil, err
			}
		}
		if action.AssignmentCount() == 0 {
			continue
		}
		if err := c.charge(uint64(action.AssignmentCount()) * (uint64(unsafe.Sizeof(ir.ScalarID(0))) + uint64(unsafe.Sizeof(model.ColumnID(0))))); err != nil {
			return nil, err
		}
		roots := make([]ir.ScalarID, action.AssignmentCount())
		compiled.AssignmentColumns = make([]model.ColumnID, len(roots))
		for j := range roots {
			assignment, ok := graph.MergeAssignment(node.Logical.Rel, uint32(i), uint32(j))
			if !ok {
				return nil, ErrInvalid
			}
			info, ok := graph.Slot(assignment.Slot)
			if !ok || info.Object != relation.Object || info.Column == 0 {
				return nil, ErrInvalid
			}
			roots[j], compiled.AssignmentColumns[j] = assignment.Expr, info.Column
		}
		compiled.Assignment, err = c.program(node.Logical.Graph, roots, slots)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}
