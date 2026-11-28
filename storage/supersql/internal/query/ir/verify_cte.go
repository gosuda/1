package ir

import "context"

// Definition cycles and working-table references are different edges: a local
// CTE inside a recursive member may read that member's current working table.
func (g *Graph) walkCTE(ctx context.Context, id CTEID, depth uint32, state *walkState) (uint32, error) {
	cte := g.ctes[id-1]
	if !validID(cte.Body, g.rels) {
		return 0, invalid("unbound CTE reference")
	}
	if state.activeCTEs[id-1] {
		return 0, invalid("cyclic CTE dependency")
	}
	repeated := state.recursiveOwner != 0 && state.countedCTEs[id-1] == state.recursiveGeneration
	suppressed := state.suppressReferences
	state.suppressReferences = suppressed || repeated
	state.activeCTEs[id-1] = true
	boundary := state.subqueryScope
	insideScalar := boundary != 0 && g.ancestor(boundary, cte.Scope)
	if !insideScalar {
		state.subqueryScope = 0
	}
	count, err := g.walkRel(ctx, cte.Body, id, state.recursiveOwner != 0 && !insideScalar, depth, state)
	state.subqueryScope = boundary
	state.activeCTEs[id-1] = false
	state.suppressReferences = suppressed
	if err != nil {
		return 0, err
	}
	if state.recursiveOwner != 0 {
		state.countedCTEs[id-1] = state.recursiveGeneration
	}
	if repeated {
		return 0, nil
	}
	return count, nil
}

func (g *Graph) walkRecursiveUnion(ctx context.Context, n Rel, active CTEID, depth uint32, state *walkState) (uint32, error) {
	if active != 0 && active != n.CTE {
		return 0, invalid("recursive union belongs to another CTE")
	}
	deferred, boundary := state.deferredRecursion, state.subqueryScope
	state.deferredRecursion, state.subqueryScope = false, 0
	inputs := sliceAt(g.relRefs, n.inputs)
	if _, err := g.walkRel(ctx, inputs[0], n.CTE, false, depth+1, state); err != nil {
		state.deferredRecursion, state.subqueryScope = deferred, boundary
		return 0, err
	}
	count, err := g.walkRecursiveMember(ctx, inputs[1], n.CTE, depth+1, state)
	state.deferredRecursion, state.subqueryScope = deferred, boundary
	if err != nil {
		return 0, err
	}
	if count != 1 {
		return 0, invalid("recursive member needs exactly one self-reference")
	}
	return 0, nil
}

func (g *Graph) walkRecursiveMember(ctx context.Context, id RelID, owner CTEID, depth uint32, state *walkState) (uint32, error) {
	previousOwner, previousGeneration := state.recursiveOwner, state.recursiveGeneration
	previousCount, suppressed := state.recursiveReferences, state.suppressReferences
	state.nextGeneration++
	if state.nextGeneration == 0 {
		return 0, limitError()
	}
	state.recursiveOwner, state.recursiveGeneration = owner, state.nextGeneration
	state.recursiveReferences, state.suppressReferences = 0, false
	_, err := g.walkRel(ctx, id, owner, true, depth, state)
	count := state.recursiveReferences
	state.recursiveOwner, state.recursiveGeneration = previousOwner, previousGeneration
	state.recursiveReferences, state.suppressReferences = previousCount, suppressed
	return count, err
}

func (g *Graph) validateCTEBodyScope(ctx context.Context, cte CTE, body Rel) error {
	if body.Scope == cte.Scope {
		return nil
	}
	parent, ok := g.ScopeParent(body.Scope)
	if !ok || parent != cte.Scope || cte.Recursive {
		return invalid("CTE body is outside its declaration query")
	}
	for i := range g.CaptureCount(body.Scope) {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, _ := g.Capture(body.Scope, i)
		slot, ok := g.Slot(id)
		if !ok || slot.Scope == cte.Scope {
			return invalid("CTE body captures declaration-scope row slots")
		}
	}
	return nil
}
