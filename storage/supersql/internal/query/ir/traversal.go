package ir

import "context"

func (g *Graph) walkRelScalars(ctx context.Context, n Rel, active CTEID, depth uint32, state *walkState) error {
	for _, r := range [...]extent{n.expressions, n.partition} {
		for _, id := range sliceAt(g.scalarRefs, r) {
			if err := g.walkScalar(ctx, id, active, depth, state); err != nil {
				return err
			}
		}
	}
	for _, r := range [...]extent{n.bindings, n.assignments, n.conflictAssignments} {
		for _, binding := range sliceAt(g.bindings, r) {
			if err := g.walkScalar(ctx, binding.Expr, active, depth, state); err != nil {
				return err
			}
		}
	}
	for _, key := range sliceAt(g.orders, n.order) {
		if err := g.walkScalar(ctx, key.Expr, active, depth, state); err != nil {
			return err
		}
	}
	for _, call := range sliceAt(g.aggregates, n.aggregates) {
		for _, r := range [...]extent{call.direct, call.args} {
			for _, id := range sliceAt(g.scalarRefs, r) {
				if err := g.walkScalar(ctx, id, active, depth, state); err != nil {
					return err
				}
			}
		}
		if call.Filter != 0 {
			if err := g.walkScalar(ctx, call.Filter, active, depth, state); err != nil {
				return err
			}
		}
		for _, key := range sliceAt(g.orders, call.order) {
			if err := g.walkScalar(ctx, key.Expr, active, depth, state); err != nil {
				return err
			}
		}
	}
	for _, call := range sliceAt(g.windows, n.windows) {
		if call.Filter != 0 {
			if err := g.walkScalar(ctx, call.Filter, active, depth, state); err != nil {
				return err
			}
		}
		for _, id := range sliceAt(g.scalarRefs, call.args) {
			if err := g.walkScalar(ctx, id, active, depth, state); err != nil {
				return err
			}
		}
	}
	for _, id := range [...]ScalarID{n.Predicate, n.Offset, n.Count, n.ConflictPredicate, n.Frame.Start.Offset, n.Frame.End.Offset, n.Recursion.CycleValue, n.Recursion.CycleDefault, n.Recursion.CycleContinue} {
		if id != 0 {
			if err := g.walkScalar(ctx, id, active, depth, state); err != nil {
				return err
			}
		}
	}
	for _, action := range sliceAt(g.actions, n.actions) {
		if action.Condition != 0 {
			if err := g.walkScalar(ctx, action.Condition, active, depth, state); err != nil {
				return err
			}
		}
		for _, binding := range sliceAt(g.bindings, action.assignments) {
			if err := g.walkScalar(ctx, binding.Expr, active, depth, state); err != nil {
				return err
			}
		}
	}
	for _, step := range sliceAt(g.viewMutationSteps, n.viewMutationSteps) {
		if step.Predicate != 0 {
			if err := g.walkScalar(ctx, step.Predicate, active, depth, state); err != nil {
				return err
			}
		}
		for _, projection := range sliceAt(g.scalarRefs, step.projections) {
			if err := g.walkScalar(ctx, projection, active, depth, state); err != nil {
				return err
			}
		}
	}
	return nil
}
