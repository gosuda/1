package ir

import "context"

func (g *Graph) verify(ctx context.Context, root RelID, limits Limits, accepted []bool, scalarHeights []uint32, activeCTEs []bool, countedCTEs []uint64) error {
	for i, scope := range g.scopes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if uint64(scope.parent) > uint64(i) || scope.captures.count > scope.captureCapacity || !validExtent(extent{scope.captures.start, scope.captureCapacity}, g.slotRefs) {
			return invalid("invalid scope parent or capture range")
		}
		for _, slot := range sliceAt(g.slotRefs, scope.captures) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !validID(slot, g.slots) || !g.ancestor(g.slots[slot-1].Scope, scope.parent) {
				return invalid("capture outside parent scope")
			}
		}
	}
	for i, n := range g.scalars {
		if err := g.validateScalar(ctx, n, limits); err != nil {
			return err
		}
		height := uint32(1)
		relational := n.Kind == Subquery
		for _, id := range sliceAt(g.scalarRefs, n.operands) {
			if uint64(id) > uint64(i) {
				return invalid("scalar cycle or forward handle")
			}
			childHeight := scalarHeights[id-1]
			relational = relational || childHeight == 0
			height = max(height, childHeight+1)
		}
		if !relational {
			scalarHeights[i] = height
		}
	}
	for i, n := range g.rels {
		if err := g.validateRel(ctx, n, limits); err != nil {
			return err
		}
		for _, id := range sliceAt(g.relRefs, n.inputs) {
			if uint64(id) > uint64(i) {
				return invalid("relation cycle or forward handle")
			}
		}
		if n.Kind == CTEProducer && (!validID(g.ctes[n.CTE-1].Body, g.rels) || g.relRefs[n.inputs.start] != g.ctes[n.CTE-1].Body) {
			return invalid("CTE producer input differs from declared body")
		}
	}
	state := walkState{limits: limits, accepted: accepted, scalarHeights: scalarHeights, activeCTEs: activeCTEs, countedCTEs: countedCTEs, deferredRecursion: true}
	for i, cte := range g.ctes {
		if !validID(cte.Body, g.rels) {
			return invalid("unbound CTE")
		}
		if err := g.validateCTEBodyScope(ctx, cte, g.rels[cte.Body-1]); err != nil {
			return err
		}
		if _, err := g.walkCTE(ctx, CTEID(i+1), 0, &state); err != nil {
			return err
		}
	}
	for i := range g.scalars {
		if err := g.walkScalar(ctx, ScalarID(i+1), 0, 0, &state); err != nil {
			return err
		}
	}
	state.deferredRecursion = false
	if _, err := g.walkRel(ctx, root, 0, false, 0, &state); err != nil {
		return err
	}
	for i, n := range g.rels {
		if n.Kind == CTEConsumer && n.RecursiveReference && !accepted[i] {
			return invalid("orphan recursive reference")
		}
	}
	return ctx.Err()
}

type walkState struct {
	steps                                 uint64
	limits                                Limits
	accepted                              []bool
	activeCTEs                            []bool
	countedCTEs                           []uint64
	recursiveOwner                        CTEID
	subqueryScope                         ScopeID
	recursiveGeneration, nextGeneration   uint64
	recursiveReferences                   uint32
	deferredRecursion, suppressReferences bool
	// Zero denotes a subtree containing a relational subquery; its validity
	// depends on the active CTE and must not be cached across contexts.
	scalarHeights []uint32
}

func (s *walkState) step(ctx context.Context, depth uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.steps++
	if depth >= s.limits.MaxDepth || s.steps > uint64(s.limits.MaxNodes)*uint64(s.limits.MaxOperands) {
		return limitError()
	}
	return nil
}
func (g *Graph) walkRel(ctx context.Context, id RelID, active CTEID, recursive bool, depth uint32, state *walkState) (uint32, error) {
	if err := state.step(ctx, depth); err != nil {
		return 0, err
	}
	n := g.rels[id-1]
	if n.Kind == CTEConsumer {
		if n.RecursiveReference {
			if state.recursiveOwner == 0 && state.deferredRecursion {
				return 0, nil
			}
			if !recursive || n.CTE != state.recursiveOwner {
				return 0, invalid("recursive reference outside its recursive member")
			}
			state.accepted[id-1] = true
			if !state.suppressReferences {
				state.recursiveReferences++
				if state.recursiveReferences > 1 {
					return 0, invalid("multiple recursive references")
				}
			}
			return 1, nil
		}
		if n.CTE == active {
			return 0, invalid("CTE self-reference must be explicit and recursive")
		}
		return g.walkCTE(ctx, n.CTE, depth+1, state)
	}
	if n.Kind == RecursiveUnion {
		return g.walkRecursiveUnion(ctx, n, active, depth, state)
	}
	var references uint32
	for index, child := range sliceAt(g.relRefs, n.inputs) {
		count, err := g.walkRel(ctx, child, active, recursive, depth+1, state)
		if err != nil {
			return 0, err
		}
		if count > 1 || references > 1-count {
			return 0, invalid("multiple recursive references")
		}
		if count != 0 && n.nullableJoinInput(index) {
			return 0, invalid("recursive reference on nullable side of outer join")
		}
		references += count
	}
	forbidsRecursion := n.Kind == Aggregate || n.Kind == Modify || n.Kind == LockRows
	if references > 0 && forbidsRecursion {
		return 0, invalid("recursive reference beneath forbidden operator")
	}
	if err := g.walkRelScalars(ctx, n, active, depth+1, state); err != nil {
		return 0, err
	}
	return references, nil
}
func (n *Rel) nullableJoinInput(index int) bool {
	if n.Kind != Join {
		return false
	}
	switch n.JoinKind {
	case FullJoin:
		return true
	case LeftJoin:
		return index == 1
	case RightJoin:
		return index == 0
	default:
		return false
	}
}
func (g *Graph) walkScalar(ctx context.Context, id ScalarID, active CTEID, depth uint32, state *walkState) error {
	if err := state.step(ctx, depth); err != nil {
		return err
	}
	n := g.scalars[id-1]
	if height := state.scalarHeights[id-1]; height != 0 {
		if uint64(depth)+uint64(height) > uint64(state.limits.MaxDepth) {
			return limitError()
		}
		return nil
	}
	if n.Kind == Subquery {
		previous := state.subqueryScope
		if previous == 0 {
			state.subqueryScope = g.rels[n.Relation-1].Scope
		}
		_, err := g.walkRel(ctx, n.Relation, active, false, depth+1, state)
		state.subqueryScope = previous
		if err != nil {
			return err
		}
	}
	for _, id := range sliceAt(g.scalarRefs, n.operands) {
		if err := g.walkScalar(ctx, id, active, depth+1, state); err != nil {
			return err
		}
	}
	return nil
}
