package ir

import "context"

type promotionPhase uint8

const (
	promotionDirect promotionPhase = 1 << iota
	promotionRow
)

// scopePromotionPhases classifies the new construction before promotion mutates
// it. Operand edges select scalars; a subquery selects its complete new scope
// subtree, including capture lists. Sibling query scopes are not selected.
func (b *Builder) scopePromotionPhases(ctx context.Context, mark *ScopeMark, directRoots, rowRoots []ScalarID) ([]promotionPhase, []promotionPhase, error) {
	scalars := make([]promotionPhase, len(b.graph.scalars)-mark.scalars)
	scopes := make([]promotionPhase, len(b.graph.scopes)-mark.scopes)
	var visit func(ScalarID, promotionPhase, uint32, *uint64) error
	visit = func(id ScalarID, phase promotionPhase, depth uint32, remaining *uint64) error {
		if err := b.check(ctx); err != nil {
			return err
		}
		if depth >= b.limits.MaxDepth || *remaining == 0 {
			return b.fail(limitError())
		}
		*remaining -= 1
		if !validID(id, b.graph.scalars) || int(id) <= mark.scalars {
			return b.fail(invalid("scope promotion phase reuses an earlier or invalid scalar"))
		}
		index := int(id) - mark.scalars - 1
		if scalars[index]&phase != 0 {
			return nil
		}
		scalars[index] |= phase
		scalar := b.graph.scalars[id-1]
		if scalar.Kind == Subquery {
			if !validID(scalar.Relation, b.graph.rels) {
				return b.fail(invalid("scope promotion phase has an invalid subquery"))
			}
			scope := b.graph.rels[scalar.Relation-1].Scope
			if !validID(scope, b.graph.scopes) || int(scope) <= mark.scopes {
				return b.fail(invalid("scope promotion phase reuses an earlier subquery"))
			}
			scopes[int(scope)-mark.scopes-1] |= phase
		}
		for _, operand := range sliceAt(b.graph.scalarRefs, scalar.operands) {
			if err := visit(operand, phase, depth+1, remaining); err != nil {
				return err
			}
		}
		return nil
	}
	for _, selection := range []struct {
		roots []ScalarID
		phase promotionPhase
	}{
		{directRoots, promotionDirect},
		{rowRoots, promotionRow},
	} {
		remaining := uint64(b.limits.MaxNodes) + uint64(b.limits.MaxOperands)
		for _, root := range selection.roots {
			if !validID(root, b.graph.scalars) || int(root) <= mark.scalars || b.graph.scalars[root-1].Scope != mark.source {
				return nil, nil, b.fail(invalid("scope promotion phase root is outside the marked source"))
			}
			if err := visit(root, selection.phase, 0, &remaining); err != nil {
				return nil, nil, err
			}
		}
	}
	// Builder scope parents precede their children. One forward pass therefore
	// propagates each selected subquery's phase to all of its new descendants.
	for index := mark.scopes; index < len(b.graph.scopes); index++ {
		if err := b.check(ctx); err != nil {
			return nil, nil, err
		}
		parent := b.graph.scopes[index].parent
		if int(parent) > mark.scopes {
			scopes[index-mark.scopes] |= scopes[int(parent)-mark.scopes-1]
		}
	}
	for index := mark.scalars; index < len(b.graph.scalars); index++ {
		if err := b.check(ctx); err != nil {
			return nil, nil, err
		}
		scope := b.graph.scalars[index].Scope
		if int(scope) > mark.scopes {
			scalars[index-mark.scalars] |= scopes[int(scope)-mark.scopes-1]
		}
	}
	return scalars, scopes, nil
}
