package ir

import "context"

func (b *Builder) AddRel(ctx context.Context, spec RelSpec) (RelID, error) {
	if err := b.check(ctx); err != nil {
		return 0, err
	}
	if err := b.nodeBudget(); err != nil {
		return 0, err
	}
	n := spec.Rel
	if n.hasArenaPayload() {
		return 0, b.fail(invalid("relation contains arena payload"))
	}
	if n.assignments != (extent{}) || n.conflictAssignments != (extent{}) || n.conflictArbiters != (extent{}) {
		return 0, b.fail(invalid("relation contains assignment arena payload"))
	}
	var err error
	if n.inputs, err = appendArena(b, &b.graph.relRefs, spec.Inputs, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.expressions, err = appendArena(b, &b.graph.scalarRefs, spec.Expressions, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.outputs, err = appendArena(b, &b.graph.slotRefs, spec.Outputs, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.targetKeys, err = appendArena(b, &b.graph.slotRefs, spec.TargetKeys, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.updatedColumns, err = appendArena(b, &b.graph.columnRefs, spec.UpdatedColumns, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.bindings, err = appendArena(b, &b.graph.bindings, spec.Bindings, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.assignments, err = appendArena(b, &b.graph.bindings, spec.Assignments, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.conflictAssignments, err = appendArena(b, &b.graph.bindings, spec.ConflictAssignments, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.conflictArbiters, err = appendArena(b, &b.graph.conflictArbiters, spec.ConflictArbiters, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.order, err = appendArena(b, &b.graph.orders, spec.Order, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.partition, err = appendArena(b, &b.graph.scalarRefs, spec.Partition, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	n.locks.start = uint32(len(b.graph.locks))
	for _, spec := range spec.Locks {
		lock := spec.RowLock
		if lock.keys != (extent{}) {
			return 0, b.fail(invalid("row lock contains arena payload"))
		}
		if lock.keys, err = appendArena(b, &b.graph.lockKeys, spec.Keys, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if _, err := appendOne(b, &b.graph.locks, lock, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		n.locks.count++
	}
	n.groups.start = uint32(len(b.graph.groups))
	for _, set := range spec.GroupingSets {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		r, err := appendArena(b, &b.graph.indices, set, b.limits.MaxOperands)
		if err != nil {
			return 0, err
		}
		if _, err := appendOne(b, &b.graph.groups, r, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		n.groups.count++
	}
	n.aggregates.start = uint32(len(b.graph.aggregates))
	for _, spec := range spec.Aggregates {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		call := spec.AggregateCall
		if call.direct != (extent{}) || call.args != (extent{}) || call.order != (extent{}) {
			return 0, b.fail(invalid("aggregate contains arena payload"))
		}
		if call.direct, err = appendArena(b, &b.graph.scalarRefs, spec.Direct, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if call.args, err = appendArena(b, &b.graph.scalarRefs, spec.Args, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if call.order, err = appendArena(b, &b.graph.orders, spec.Order, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if _, err := appendOne(b, &b.graph.aggregates, call, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		n.aggregates.count++
	}
	n.groupings.start = uint32(len(b.graph.groupings))
	for _, spec := range spec.Groupings {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		call := spec.GroupingCall
		if call.args != (extent{}) {
			return 0, b.fail(invalid("GROUPING contains arena payload"))
		}
		if call.args, err = appendArena(b, &b.graph.indices, spec.Args, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if _, err := appendOne(b, &b.graph.groupings, call, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		n.groupings.count++
	}
	n.windows.start = uint32(len(b.graph.windows))
	for _, spec := range spec.Windows {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		call := spec.WindowCall
		if call.args != (extent{}) {
			return 0, b.fail(invalid("window contains arena payload"))
		}
		if call.args, err = appendArena(b, &b.graph.scalarRefs, spec.Args, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if _, err := appendOne(b, &b.graph.windows, call, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		n.windows.count++
	}
	n.Recursion = spec.RecursionSpec.Recursion
	if n.Recursion.searchKeys != (extent{}) || n.Recursion.cycleKeys != (extent{}) {
		return 0, b.fail(invalid("recursion contains arena payload"))
	}
	if n.Recursion.searchKeys, err = appendArena(b, &b.graph.slotRefs, spec.RecursionSpec.SearchKeys, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	if n.Recursion.cycleKeys, err = appendArena(b, &b.graph.slotRefs, spec.RecursionSpec.CycleKeys, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	n.actions.start = uint32(len(b.graph.actions))
	for _, spec := range spec.Actions {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		action := spec.MergeAction
		if action.assignments != (extent{}) {
			return 0, b.fail(invalid("merge action contains arena payload"))
		}
		if action.assignments, err = appendArena(b, &b.graph.bindings, spec.Assignments, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if _, err := appendOne(b, &b.graph.actions, action, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		n.actions.count++
	}
	if n.jsonTableColumns, err = appendArena(b, &b.graph.jsonTableColumns, spec.JSONTableColumns, b.limits.MaxOperands); err != nil {
		return 0, err
	}
	n.viewMutationSteps.start = uint32(len(b.graph.viewMutationSteps))
	for _, spec := range spec.ViewMutationSteps {
		if err := b.check(ctx); err != nil {
			return 0, err
		}
		step := spec.ViewMutationStep
		if step.inputs != (extent{}) || step.projections != (extent{}) || step.outputColumns != (extent{}) {
			return 0, b.fail(invalid("view mutation step contains arena payload"))
		}
		if step.inputs, err = appendArena(b, &b.graph.slotRefs, spec.Inputs, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if step.projections, err = appendArena(b, &b.graph.scalarRefs, spec.Projections, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if step.outputColumns, err = appendArena(b, &b.graph.columnRefs, spec.OutputColumns, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		if _, err := appendOne(b, &b.graph.viewMutationSteps, step, b.limits.MaxOperands); err != nil {
			return 0, err
		}
		n.viewMutationSteps.count++
	}
	if err := b.graph.validateRel(ctx, n, b.limits); err != nil {
		return 0, b.fail(err)
	}
	id, err := appendOne(b, &b.graph.rels, n, b.limits.MaxNodes)
	return RelID(id), err
}

func (n *Rel) hasArenaPayload() bool {
	for _, r := range [...]extent{n.inputs, n.expressions, n.outputs, n.targetKeys, n.updatedColumns, n.bindings, n.assignments, n.conflictAssignments, n.conflictArbiters, n.order, n.groups, n.aggregates, n.groupings, n.windows, n.partition, n.locks, n.actions, n.jsonTableColumns, n.viewMutationSteps} {
		if r != (extent{}) {
			return true
		}
	}
	return n.Recursion != (Recursion{})
}
