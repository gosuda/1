package ir

func (g *Graph) relationEffects(n Rel) Effects {
	var effects Effects
	for _, id := range sliceAt(g.relRefs, n.inputs) {
		effects = effects.Union(g.rels[id-1].Effects)
	}
	if n.CTE != 0 {
		effects = effects.Union(g.ctes[n.CTE-1].Effects)
	}
	for _, r := range [...]extent{n.expressions, n.partition} {
		for _, id := range sliceAt(g.scalarRefs, r) {
			effects = effects.Union(g.scalars[id-1].Effects)
		}
	}
	for _, r := range [...]extent{n.bindings, n.assignments, n.conflictAssignments} {
		for _, binding := range sliceAt(g.bindings, r) {
			effects = effects.Union(g.scalars[binding.Expr-1].Effects)
		}
	}
	for _, key := range sliceAt(g.orders, n.order) {
		effects = effects.Union(g.scalars[key.Expr-1].Effects)
	}
	for _, call := range sliceAt(g.aggregates, n.aggregates) {
		effects = effects.Union(call.Effects)
	}
	for _, call := range sliceAt(g.windows, n.windows) {
		effects = effects.Union(call.Effects)
	}
	for _, id := range [...]ScalarID{n.Predicate, n.Offset, n.Count, n.ConflictPredicate, n.Frame.Start.Offset, n.Frame.End.Offset, n.Recursion.CycleValue, n.Recursion.CycleDefault, n.Recursion.CycleContinue} {
		if id != 0 {
			effects = effects.Union(g.scalars[id-1].Effects)
		}
	}
	for _, id := range [...]SignatureID{n.Frame.Start.InRange, n.Frame.End.InRange} {
		if validID(id, g.signatures) {
			effects = effects.Union(g.signatures[id-1].Effects)
		}
	}
	for _, action := range sliceAt(g.actions, n.actions) {
		if action.Condition != 0 {
			effects = effects.Union(g.scalars[action.Condition-1].Effects)
		}
		for _, binding := range sliceAt(g.bindings, action.assignments) {
			effects = effects.Union(g.scalars[binding.Expr-1].Effects)
		}
	}
	for _, step := range sliceAt(g.viewMutationSteps, n.viewMutationSteps) {
		if step.Predicate != 0 {
			effects = effects.Union(g.scalars[step.Predicate-1].Effects)
		}
		for _, projection := range sliceAt(g.scalarRefs, step.projections) {
			effects = effects.Union(g.scalars[projection-1].Effects)
		}
	}
	if n.Kind == Modify || n.Kind == LockRows {
		effects = effects.Union(Effects{Volatility: Volatile, MayError: true})
	}
	if n.Kind == Apply && n.ApplyKind == ScalarApply {
		effects.MayError = true
	}
	boundedWindow := n.Kind == Window && (n.Frame.Start.Offset != 0 || n.Frame.End.Offset != 0)
	tableFunction := n.Kind == GenerateSeries || n.Kind == GenerateSubscripts || n.Kind == Unnest || n.Kind == RowsFrom || n.Kind == ProjectSet
	if n.Kind == Limit || tableFunction || boundedWindow {
		effects.MayError = true
	}
	return effects
}
