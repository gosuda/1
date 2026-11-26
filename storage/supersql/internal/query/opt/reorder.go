package opt

import (
	"errors"
	"math/bits"
	"slices"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type joinTree struct {
	left, right *joinTree
	leaf        ir.RelID
	mask        uint16
	rows, cost  float64
	rowBytes    float64
}
type joinPredicate struct {
	id          ir.ScalarID
	mask        uint16
	leaves      []int
	selectivity float64
	tree        *joinTree
}

func reorderAllocate[T any](o *optimizer, n uint64) []T {
	if o.err != nil || o.exhausted {
		return nil
	}
	var value T
	size := uint64(unsafe.Sizeof(value))
	if size != 0 && n > (uint64(o.options.MemoBytes)-o.memoBytes)/size {
		o.exhausted = true
		return nil
	}
	o.memoBytes += n * size
	return allocate[T](o, n)
}

func (o *optimizer) subsetJoin(leaves []ir.RelID, predicates []ir.ScalarID) (*joinTree, []joinPredicate) {
	facts := reorderAllocate[joinPredicate](o, uint64(len(predicates)))
	if o.err != nil || o.exhausted {
		return nil, nil
	}
	for i, predicate := range predicates {
		mask, safe := o.predicateMask(predicate, leaves, 0)
		if !safe {
			return nil, nil
		}
		facts[i] = joinPredicate{id: predicate, mask: mask, selectivity: o.selectivity(predicate, 0)}
	}
	count := 1 << len(leaves)
	trees := reorderAllocate[joinTree](o, uint64(count))
	if o.err != nil || o.exhausted {
		return nil, nil
	}
	for i, leaf := range leaves {
		m := 1 << i
		trees[m] = o.joinLeaf(leaf)
		trees[m].mask = uint16(m)
	}
	for size := 2; size <= len(leaves); size++ {
		for mask := 1; mask < count; mask++ {
			if bits.OnesCount(uint(mask)) != size {
				continue
			}
			if !o.exploreSubset(trees, facts, mask) {
				return nil, nil
			}
		}
	}
	return &trees[count-1], facts
}

func (o *optimizer) exploreSubset(trees []joinTree, facts []joinPredicate, mask int) bool {
	for left := (mask - 1) & mask; left > 0; left = (left - 1) & mask {
		if !o.search() {
			return false
		}
		right := mask ^ left
		selectivity := 1.0
		for _, predicate := range facts {
			if !o.step(1) {
				return false
			}
			crosses := predicate.mask&uint16(left) != predicate.mask && predicate.mask&uint16(right) != predicate.mask
			rootFilter := mask == len(trees)-1 && bits.OnesCount16(predicate.mask) < 2
			if predicate.mask&uint16(mask) == predicate.mask && (crosses || rootFilter) {
				selectivity *= predicate.selectivity
			}
		}
		candidate := o.joinCandidate(&trees[left], &trees[right], selectivity)
		candidate.mask = uint16(mask)
		o.expressions++
		if trees[mask].mask == 0 || candidate.cost < trees[mask].cost {
			trees[mask] = candidate
		}
	}
	return true
}

func (o *optimizer) joinLeaf(id ir.RelID) joinTree {
	node := &o.groups[id].seed.node
	return joinTree{leaf: id, rows: node.Estimate.Rows, rowBytes: node.Estimate.RowBytes, cost: node.Cost.Total}
}

func (o *optimizer) joinCandidate(left, right *joinTree, selectivity float64) joinTree {
	l := expression{node: physical.NodeSpec{Estimate: physical.Estimate{Rows: left.rows, RowBytes: left.rowBytes}, Cost: physical.Cost{Total: left.cost}}}
	r := expression{node: physical.NodeSpec{Estimate: physical.Estimate{Rows: right.rows, RowBytes: right.rowBytes}, Cost: physical.Cost{Total: right.cost}}}
	children := [2]*expression{&l, &r}
	rows := cappedMultiply(left.rows, right.rows) * selectivity
	width := left.rowBytes + right.rowBytes
	e := expression{node: physical.NodeSpec{Algorithm: physical.NestedLoopJoin, Estimate: physical.Estimate{Rows: rows, RowBytes: width}}, children: children[:]}
	o.cost(&e)
	return joinTree{left: left, right: right, rows: rows, rowBytes: width, cost: e.node.Cost.Total}
}

// Large regions retain only one prefix and one candidate per remaining leaf.
// Slot ownership uses indexed membership, never a native-word relation mask.
func (o *optimizer) greedyJoin(leaves []ir.RelID, predicates []ir.ScalarID) (*joinTree, []joinPredicate) {
	if len(leaves) >= int(o.options.IR.MaxDepth) || len(leaves) >= int(o.options.Plan.MaxDepth) {
		o.exhausted = true
		return nil, nil
	}
	facts := reorderAllocate[joinPredicate](o, uint64(len(predicates)))
	owners := reorderAllocate[int](o, uint64(o.graph.SlotCount())+1)
	marks := reorderAllocate[bool](o, uint64(len(leaves)))
	chosen := reorderAllocate[bool](o, uint64(len(leaves)))
	trees := reorderAllocate[joinTree](o, uint64(len(leaves))*2-1)
	if o.err != nil || o.exhausted {
		return nil, nil
	}
	start := 0
	for i, leaf := range leaves {
		if !o.search() {
			return nil, nil
		}
		trees[i] = o.joinLeaf(leaf)
		if trees[i].rows < trees[start].rows || (trees[i].rows == trees[start].rows && trees[i].cost < trees[start].cost) {
			start = i
		}
		r, _ := o.graph.Rel(leaf)
		for j := range r.OutputCount() {
			if !o.step(1) {
				return nil, nil
			}
			slot, _ := o.graph.Output(leaf, j)
			if owners[slot] != 0 {
				return nil, nil
			}
			owners[slot] = i + 1
		}
	}
	for i, id := range predicates {
		clear(marks)
		if !o.step(uint64(len(marks))) || !o.predicateLeaves(id, owners, marks, 0) {
			return nil, nil
		}
		count := 0
		for _, referenced := range marks {
			if referenced {
				count++
			}
		}
		refs := reorderAllocate[int](o, uint64(count))
		if o.err != nil || o.exhausted || !o.step(uint64(len(marks))) {
			return nil, nil
		}
		at := 0
		for leaf, referenced := range marks {
			if referenced {
				refs[at] = leaf
				at++
			}
		}
		facts[i] = joinPredicate{id: id, leaves: refs, selectivity: o.selectivity(id, 0)}
	}
	chosen[start] = true
	prefix := &trees[start]
	for size := 1; size < len(leaves); size++ {
		winner := -1
		connectedWinner := false
		var best joinTree
		for i := range leaves {
			if !o.search() {
				return nil, nil
			}
			if chosen[i] {
				continue
			}
			selectivity := 1.0
			connected := false
			for _, predicate := range facts {
				ready, crosses := o.greedyPredicate(predicate, chosen, i)
				if ready {
					selectivity *= predicate.selectivity
					connected = connected || crosses
				}
			}
			candidate := o.joinCandidate(prefix, &trees[i], selectivity)
			o.expressions++
			newConnection := connected && !connectedWinner
			betterCost := candidate.cost < best.cost || (candidate.cost == best.cost && candidate.rows < best.rows)
			betterConnectedPlan := connected == connectedWinner && betterCost
			if winner < 0 || newConnection || betterConnectedPlan {
				winner, connectedWinner, best = i, connected, candidate
			}
		}
		if !o.search() {
			return nil, nil
		}
		prefix = &trees[len(leaves)+size-1]
		*prefix = best
		for i := range facts {
			if ready, _ := o.greedyPredicate(facts[i], chosen, winner); ready {
				facts[i].tree = prefix
			}
		}
		chosen[winner] = true
	}
	if o.err != nil {
		return nil, nil
	}
	return prefix, facts
}

func (o *optimizer) greedyPredicate(predicate joinPredicate, chosen []bool, next int) (ready, connected bool) {
	if !o.step(1) || predicate.tree != nil {
		return false, false
	}
	left, right := false, false
	for _, leaf := range predicate.leaves {
		if !o.step(1) {
			return false, false
		}
		switch {
		case chosen[leaf]:
			left = true
		case leaf == next:
			right = true
		default:
			return false, false
		}
	}
	return true, left && right
}

func (o *optimizer) predicateLeaves(id ir.ScalarID, owners []int, marks []bool, depth uint32) bool {
	if !o.step(1) {
		return false
	}
	if depth >= o.options.IR.MaxDepth {
		o.err = ErrLimit
		return false
	}
	s, _ := o.graph.Scalar(id)
	if s.Effects.Volatility != ir.Immutable || s.Effects.MayError || s.Kind == ir.Subquery || s.Kind == ir.Once {
		return false
	}
	if s.Kind == ir.Slot {
		owner := owners[s.Slot]
		if owner == 0 {
			return false
		}
		marks[owner-1] = true
		return true
	}
	for i := range s.OperandCount() {
		child, _ := o.graph.Operand(id, i)
		if !o.predicateLeaves(child, owners, marks, depth+1) {
			return false
		}
	}
	return true
}

// Reordering is restricted to identity-preserving inner/cross join regions.
// Renamed outputs, captured scopes and effects remain original-order barriers.
func (o *optimizer) reorder(best *physical.Plan) (*physical.Plan, error) {
	root := o.graph.Root()
	region := root
	for o.step(1) {
		r, _ := o.graph.Rel(region)
		if r.SecurityBarrier || r.Effects.Volatility != ir.Immutable || r.Effects.MayError {
			return best, nil
		}
		if r.Kind == ir.Join {
			break
		}
		transparent := r.Kind == ir.Project || r.Kind == ir.Filter || r.Kind == ir.Sort
		if r.InputCount() != 1 || !transparent {
			return best, o.err
		}
		region, _ = o.graph.Input(region, 0)
	}
	if o.err != nil {
		return nil, o.err
	}
	var leaves []ir.RelID
	var predicates []ir.ScalarID
	var collect func(ir.RelID, uint32) bool
	collect = func(id ir.RelID, depth uint32) bool {
		if !o.search() {
			return false
		}
		if depth >= o.options.IR.MaxDepth {
			o.err = ErrLimit
			return false
		}
		r, _ := o.graph.Rel(id)
		reorderableKind := r.Kind == ir.Join && (r.JoinKind == ir.InnerJoin || r.JoinKind == ir.CrossJoin)
		if !reorderableKind || r.SecurityBarrier || r.Effects.Volatility != ir.Immutable || r.Effects.MayError || o.graph.CaptureCount(r.Scope) != 0 {
			if !o.charge(2, 4) {
				return false
			}
			leaves = append(leaves, id)
			return true
		}
		left, _ := o.graph.Input(id, 0)
		right, _ := o.graph.Input(id, 1)
		l, _ := o.graph.Rel(left)
		rr, _ := o.graph.Rel(right)
		if uint64(r.OutputCount()) != uint64(l.OutputCount())+uint64(rr.OutputCount()) {
			return false
		}
		for i := range r.OutputCount() {
			actual, _ := o.graph.Output(id, i)
			var expected model.SlotID
			if i < l.OutputCount() {
				expected, _ = o.graph.Output(left, i)
			} else {
				expected, _ = o.graph.Output(right, i-l.OutputCount())
			}
			if actual != expected {
				return false
			}
		}
		if l.Scope != r.Scope || rr.Scope != r.Scope {
			return false
		}
		if r.Predicate != 0 {
			if !o.charge(2, 4) {
				return false
			}
			predicates = append(predicates, r.Predicate)
		}
		return collect(left, depth+1) && collect(right, depth+1)
	}
	if !collect(region, 0) || len(leaves) < 2 {
		return best, o.err
	}
	var tree *joinTree
	var facts []joinPredicate
	if len(leaves) <= 8 {
		tree, facts = o.subsetJoin(leaves, predicates)
	} else {
		tree, facts = o.greedyJoin(leaves, predicates)
	}
	if tree == nil {
		return best, o.err
	}
	if !o.search() {
		return best, o.err
	}
	reserve := o.options.IR.MaxBytes + o.options.Plan.MaxBytes
	if reserve < o.options.IR.MaxBytes || reserve > o.options.MaxBytes-o.bytes {
		o.exhausted = true
		return best, nil
	}
	graph, err := o.rebuildJoin(region, tree, facts)
	if err != nil {
		if errors.Is(err, ir.ErrLimit) {
			o.exhausted = true
			return best, nil
		}
		return nil, err
	}
	if !o.search() {
		return best, o.err
	}
	remaining := o.options
	remaining.MaxBytes -= o.bytes
	remaining.MaxWork -= o.work
	remaining.SearchTime = max(time.Nanosecond, o.options.SearchTime-time.Since(o.searchStart))
	remaining.MemoExpressions = max(1, o.options.MemoExpressions-o.expressions)
	remaining.MemoBytes = max(1, o.options.MemoBytes-int64(o.memoBytes))
	candidate, err := optimizeOriginal(o.ctx, graph, remaining)
	if err != nil {
		if errors.Is(err, ErrLimit) || errors.Is(err, ir.ErrLimit) || errors.Is(err, physical.ErrLimit) {
			o.exhausted = true
			return best, nil
		}
		return nil, err
	}
	o.exhausted = o.exhausted || candidate.BudgetExhausted
	o.expressions += candidate.MemoExpressions
	o.memoBytes += candidate.MemoBytes
	oldRoot, _ := best.Node(best.Root())
	newRoot, _ := candidate.Plan.Node(candidate.Plan.Root())
	if newRoot.Metadata().Cost.Total < oldRoot.Metadata().Cost.Total {
		return candidate.Plan, nil
	}
	return best, nil
}
func (o *optimizer) predicateMask(id ir.ScalarID, leaves []ir.RelID, depth uint32) (uint16, bool) {
	if !o.step(1) {
		return 0, false
	}
	if depth >= o.options.IR.MaxDepth {
		o.err = ErrLimit
		return 0, false
	}
	s, _ := o.graph.Scalar(id)
	if s.Effects.Volatility != ir.Immutable || s.Effects.MayError || s.Kind == ir.Subquery || s.Kind == ir.Once {
		return 0, false
	}
	if s.Kind == ir.Slot {
		for i, leaf := range leaves {
			r, _ := o.graph.Rel(leaf)
			for j := range r.OutputCount() {
				slot, _ := o.graph.Output(leaf, j)
				if slot == s.Slot {
					return 1 << i, true
				}
			}
		}
		return 0, false
	}
	var mask uint16
	for i := range s.OperandCount() {
		child, _ := o.graph.Operand(id, i)
		m, safe := o.predicateMask(child, leaves, depth+1)
		if !safe {
			return 0, false
		}
		mask |= m
	}
	return mask, true
}
func (o *optimizer) rebuildJoin(region ir.RelID, tree *joinTree, predicates []joinPredicate) (*ir.Graph, error) {
	if !o.charge(o.options.IR.MaxBytes, 1) {
		return nil, o.err
	}
	b, err := ir.Extend(o.ctx, o.graph, o.options.IR)
	if err != nil {
		return nil, err
	}
	logical, _ := o.graph.Rel(region)
	type built struct {
		id    ir.RelID
		slots []model.SlotID
	}
	var build func(*joinTree, bool) (built, error)
	build = func(t *joinTree, root bool) (built, error) {
		if !o.step(1) {
			return built{}, o.err
		}
		if t.leaf != 0 {
			r, _ := o.graph.Rel(t.leaf)
			slots := allocate[model.SlotID](o, uint64(r.OutputCount()))
			if o.err != nil {
				return built{}, o.err
			}
			for i := range slots {
				slots[i], _ = o.graph.Output(t.leaf, uint32(i))
			}
			return built{id: t.leaf, slots: slots}, nil
		}
		left, err := build(t.left, false)
		if err != nil {
			return built{}, err
		}
		right, err := build(t.right, false)
		if err != nil {
			return built{}, err
		}
		outputs := allocate[model.SlotID](o, uint64(len(left.slots)+len(right.slots)))
		if o.err != nil {
			return built{}, o.err
		}
		copy(outputs, left.slots)
		copy(outputs[len(left.slots):], right.slots)
		var selected []ir.ScalarID
		for _, predicate := range predicates {
			crosses := predicate.mask&t.left.mask != predicate.mask && predicate.mask&t.right.mask != predicate.mask
			rootFilter := root && bits.OnesCount16(predicate.mask) < 2
			covered := t.mask != 0 && predicate.mask&t.mask == predicate.mask
			belongsHere := covered && (crosses || rootFilter)
			if predicate.tree == t || belongsHere {
				if !o.charge(2, 4) {
					return built{}, o.err
				}
				selected = append(selected, predicate.id)
			}
		}
		predicate := ir.ScalarID(0)
		if len(selected) == 1 {
			predicate = selected[0]
		}
		if len(selected) > 1 {
			s, _ := o.graph.Scalar(selected[0])
			predicate, err = b.AddScalar(o.ctx, ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, Scope: logical.Scope, Layout: s.Layout, Nullable: true, BoolOp: ir.And}, Operands: selected})
			if err != nil {
				return built{}, err
			}
		}
		kind := ir.CrossJoin
		if predicate != 0 {
			kind = ir.InnerJoin
		}
		id, err := b.AddRel(o.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: logical.Scope, JoinKind: kind, Predicate: predicate}, Inputs: []ir.RelID{left.id, right.id}, Outputs: outputs})
		return built{id: id, slots: outputs}, err
	}
	replacement, err := build(tree, true)
	if err != nil {
		return nil, err
	}
	original := allocate[model.SlotID](o, uint64(logical.OutputCount()))
	if o.err != nil {
		return nil, o.err
	}
	for i := range original {
		original[i], _ = o.graph.Output(region, uint32(i))
	}
	if !slices.Equal(original, replacement.slots) {
		bindings := allocate[ir.Binding](o, uint64(len(original)))
		if o.err != nil {
			return nil, o.err
		}
		for i, slot := range original {
			info, _ := o.graph.Slot(slot)
			scalar, err := b.AddScalar(o.ctx, ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: logical.Scope, Layout: info.Layout, Nullable: info.Nullable, Slot: slot}})
			if err != nil {
				return nil, err
			}
			bindings[i] = ir.Binding{Slot: slot, Expr: scalar}
		}
		replacement.id, err = b.AddRel(o.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: logical.Scope}, Inputs: []ir.RelID{replacement.id}, Outputs: original, Bindings: bindings})
		if err != nil {
			return nil, err
		}
	}
	var rebuild func(ir.RelID) (ir.RelID, error)
	rebuild = func(id ir.RelID) (ir.RelID, error) {
		if id == region {
			return replacement.id, nil
		}
		if !o.step(1) {
			return 0, o.err
		}
		spec, err := o.graph.CloneRelSpec(o.ctx, id, o.options.IR.MaxBytes)
		if err != nil {
			return 0, err
		}
		if len(spec.Inputs) != 1 {
			return 0, ErrInvalid
		}
		spec.Inputs[0], err = rebuild(spec.Inputs[0])
		if err != nil {
			return 0, err
		}
		return b.AddRel(o.ctx, spec)
	}
	root, err := rebuild(o.graph.Root())
	if err != nil {
		return nil, err
	}
	return b.Build(o.ctx, root)
}
