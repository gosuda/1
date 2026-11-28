package normalize

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// predicateCrossJoin lowers Filter-over-CrossJoin nests into left-deep inner
// joins, pushing each immutable conjunct to the earliest join whose leaves
// supply all its slots. Conjunct order is preserved, so the per-pair
// evaluation order of the original AND is unchanged; volatile conjuncts stay
// in a residual filter above the new joins. Only immutable conjuncts move:
// pushing a volatile conjunct would change how many times it evaluates.
//
// Only pure cross subtrees convert: a non-cross node stops the descent and
// becomes an opaque leaf, and an explicit inner join inside the subtree keeps
// the whole nest as bound. The rebuilt joins carry folded effects, so
// verification sees the same coverage the bound relations declared.
func (n *normalizer) predicateCrossJoin(id ir.RelID, source ir.Rel, spec ir.RelSpec, depth uint32) (relResult, bool) {
	if source.Kind != ir.Filter || source.SecurityBarrier || source.Predicate == 0 || source.InputCount() != 1 {
		return relResult{}, false
	}
	if depth >= n.o.Limits.MaxDepth || !n.step(1) {
		if depth >= n.o.Limits.MaxDepth {
			n.err = resourceError()
			return relResult{}, true
		}
		return relResult{}, false
	}
	inputID, _ := n.g.Input(id, 0)
	input, ok := n.g.Rel(inputID)
	if !ok || input.Kind != ir.Join || input.JoinKind != ir.CrossJoin {
		return relResult{}, false
	}
	var leaves []ir.RelID
	var gather func(ir.RelID, uint32) bool
	gather = func(rid ir.RelID, d uint32) bool {
		if d >= n.o.Limits.MaxDepth || !n.step(1) {
			if d >= n.o.Limits.MaxDepth {
				n.err = resourceError()
			}
			return false
		}
		r, ok := n.g.Rel(rid)
		if !ok {
			n.err = ErrInvalid
			return false
		}
		if r.Kind == ir.Join && r.JoinKind == ir.CrossJoin {
			if r.InputCount() != 2 || r.SecurityBarrier || r.Predicate != 0 || r.Scope != source.Scope {
				return false
			}
			left, _ := n.g.Input(rid, 0)
			right, _ := n.g.Input(rid, 1)
			return gather(left, d+1) && gather(right, d+1)
		}
		leaves = append(leaves, rid)
		return true
	}
	if !gather(inputID, depth+1) || len(leaves) < 2 {
		if n.err != nil {
			return relResult{}, true
		}
		return relResult{}, false
	}
	pred := n.scalar(source.Predicate, depth+1)
	if n.err != nil {
		return relResult{}, true
	}
	var parts []scalarResult
	if ps, ok := n.b.Scalar(pred.id); ok && ps.Kind == ir.Bool && ps.BoolOp == ir.And {
		for i := range ps.OperandCount() {
			child, _ := n.b.Operand(pred.id, i)
			cs, ok := n.b.Scalar(child)
			if !ok {
				n.err = ErrInvalid
				return relResult{}, true
			}
			parts = append(parts, scalarResult{id: child, n: cs})
		}
	} else {
		parts = append(parts, pred)
	}
	// A volatile conjunct evaluates once per row the filter sees, and an
	// erroring conjunct raises for the first failing row: moving any other
	// conjunct into a join changes how many rows reach them, so the sequence
	// advances fewer times or the error goes silent. Keep the original
	// filter and cross joins intact when any conjunct can vary or fail.
	for _, part := range parts {
		if part.n.Effects.Volatility == ir.Volatile || part.n.Effects.MayError {
			return relResult{}, false
		}
	}
	// Slot sets use normalized operands, which is what the rebuilt joins
	// evaluate. Slots outside every leaf (parameters, outer captures) never
	// constrain placement.
	leafOf := make(map[model.SlotID]int)
	type segment struct {
		id        ir.RelID
		kind      ir.JoinKind
		predicate ir.ScalarID
		outputs   []model.SlotID
		effects   ir.Effects
	}
	segments := make([]segment, len(leaves))
	for i, leaf := range leaves {
		res := n.rels[leaf]
		if res.id == 0 {
			n.err = ErrInvalid
			return relResult{}, true
		}
		segments[i] = segment{id: res.id, outputs: res.outputs, effects: res.n.Effects}
		for _, slot := range res.outputs {
			if _, dup := leafOf[slot]; !dup {
				leafOf[slot] = i
			}
		}
	}
	slotLeaves := func(sid ir.ScalarID) (map[int]bool, bool) {
		found := make(map[int]bool)
		seen := make(map[ir.ScalarID]bool)
		var walk func(ir.ScalarID, uint32) bool
		walk = func(cur ir.ScalarID, d uint32) bool {
			if cur == 0 || d >= n.o.Limits.MaxDepth || !n.step(1) {
				return d < n.o.Limits.MaxDepth
			}
			if seen[cur] {
				return true
			}
			seen[cur] = true
			s, ok := n.b.Scalar(cur)
			if !ok {
				return false
			}
			if s.Kind == ir.Slot {
				if leaf, ok := leafOf[s.Slot]; ok {
					found[leaf] = true
				}
				return true
			}
			for j := range s.OperandCount() {
				child, _ := n.b.Operand(cur, j)
				if !walk(child, d+1) {
					return false
				}
			}
			return true
		}
		if !walk(sid, 0) {
			return nil, false
		}
		return found, true
	}
	// hasSubquery reports whether the scalar tree contains a subquery. A
	// subquery reads its outer captures, which are not ordinary scalar
	// operands, so placement by operand leaves alone would attach it to a
	// join that cannot supply those captures. Such conjuncts stay in the
	// residual filter above the new joins.
	hasSubquery := func(sid ir.ScalarID) (bool, bool) {
		found := false
		seen := make(map[ir.ScalarID]bool)
		var walk func(ir.ScalarID, uint32) bool
		walk = func(cur ir.ScalarID, d uint32) bool {
			if found || cur == 0 || d >= n.o.Limits.MaxDepth || !n.step(1) {
				return found && d < n.o.Limits.MaxDepth
			}
			if seen[cur] {
				return true
			}
			seen[cur] = true
			s, ok := n.b.Scalar(cur)
			if !ok {
				return false
			}
			if s.Kind == ir.Subquery {
				found = true
				return true
			}
			for j := range s.OperandCount() {
				child, _ := n.b.Operand(cur, j)
				if !walk(child, d+1) {
					return false
				}
			}
			return true
		}
		if !walk(sid, 0) {
			return false, false
		}
		return found, true
	}
	// A part's leaves and subqueries never change across prefixes, so
	// analyze each part once instead of recomputing it at every join.
	partOwners := make([]map[int]bool, len(parts))
	partSkipped := make([]bool, len(parts))
	for j, part := range parts {
		if part.n.Effects.Volatility != ir.Immutable {
			continue
		}
		owned, ok := slotLeaves(part.id)
		if !ok {
			n.err = ErrInvalid
			return relResult{}, true
		}
		partOwners[j] = owned
		sub, ok := hasSubquery(part.id)
		if !ok {
			n.err = ErrInvalid
			return relResult{}, true
		}
		partSkipped[j] = sub
	}
	attached := make([]bool, len(parts))
	attachedTotal := 0
	present := make([]bool, len(leaves))
	present[0] = true
	top := segments[0]
	top.kind = ir.CrossJoin
	for i := 1; i < len(segments); i++ {
		if n.err != nil {
			return relResult{}, true
		}
		present[i] = true
		var selected []scalarResult
		for j, part := range parts {
			if attached[j] || partSkipped[j] || part.n.Effects.Volatility != ir.Immutable {
				continue
			}
			ready := true
			for leaf := range partOwners[j] {
				if !present[leaf] {
					ready = false
					break
				}
			}
			if ready {
				selected = append(selected, part)
				attached[j] = true
				attachedTotal++
			}
		}
		var predID ir.ScalarID
		nullable := false
		if len(selected) == 1 {
			predID = selected[0].id
		} else if len(selected) > 1 {
			args := selected
			for _, s := range args {
				nullable = nullable || s.n.Nullable
			}
			meta := ir.Scalar{Kind: ir.Bool, Scope: source.Scope, Layout: pred.n.Layout, Nullable: nullable, BoolOp: ir.And}
			predID = n.addScalar(meta, args).id
			if n.err != nil {
				return relResult{}, true
			}
		}
		kind := ir.CrossJoin
		if predID != 0 {
			kind = ir.InnerJoin
		}
		outputs := append(slices.Clone(top.outputs), segments[i].outputs...)
		effects := top.effects.Union(segments[i].effects)
		for _, s := range selected {
			effects = effects.Union(s.n.Effects)
		}
		joinID, err := n.b.AddRel(n.ctx, ir.RelSpec{
			Rel:     ir.Rel{Kind: ir.Join, Scope: source.Scope, JoinKind: kind, Predicate: predID, Effects: effects},
			Inputs:  []ir.RelID{top.id, segments[i].id},
			Outputs: outputs,
		})
		if err != nil {
			n.err = err
			return relResult{}, true
		}
		top = segment{id: joinID, kind: kind, predicate: predID, outputs: outputs, effects: effects}
	}
	if attachedTotal == 0 {
		// Nothing moved into the joins; keep the bound filter-over-cross
		// shape instead of rebuilding an identical tree.
		return relResult{}, false
	}
	if !slices.Equal(top.outputs, spec.Outputs) {
		// Pruned or reordered outputs would need a projection the binder did
		// not declare; keep the bound shape instead of guessing one.
		return relResult{}, false
	}
	if n.err != nil {
		return relResult{}, true
	}
	current := relResult{id: top.id, n: ir.Rel{Kind: ir.Join, Scope: source.Scope, JoinKind: top.kind, Predicate: top.predicate, Effects: top.effects}, outputs: top.outputs}
	var residual []scalarResult
	for j, part := range parts {
		if !attached[j] {
			residual = append(residual, part)
		}
	}
	if len(residual) == 0 {
		return current, true
	}
	var residualID ir.ScalarID
	if len(residual) == 1 {
		residualID = residual[0].id
	} else {
		nullable := false
		for _, s := range residual {
			nullable = nullable || s.n.Nullable
		}
		meta := ir.Scalar{Kind: ir.Bool, Scope: source.Scope, Layout: pred.n.Layout, Nullable: nullable, BoolOp: ir.And}
		residualID = n.addScalar(meta, residual).id
		if n.err != nil {
			return relResult{}, true
		}
	}
	effects := top.effects
	if rs, ok := n.b.Scalar(residualID); ok {
		effects = effects.Union(rs.Effects)
	}
	filterID, err := n.b.AddRel(n.ctx, ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Filter, Scope: source.Scope, Predicate: residualID, Effects: effects},
		Inputs:  []ir.RelID{top.id},
		Outputs: slices.Clone(top.outputs),
	})
	if err != nil {
		n.err = err
		return relResult{}, true
	}
	current = relResult{id: filterID, n: ir.Rel{Kind: ir.Filter, Scope: source.Scope, Predicate: residualID, Effects: effects}, outputs: top.outputs}
	return current, true
}
