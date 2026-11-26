package opt

import (
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (o *optimizer) register(id ir.RelID, e *expression) {
	if e == nil || !o.search() {
		return
	}
	g := &o.groups[id]
	for _, existing := range g.alternatives {
		if !o.step(1) {
			return
		}
		if equivalent(existing, e) {
			return
		}
	}
	size := uint64(unsafe.Sizeof(expression{})) + uint64(len(e.node.Output))*uint64(unsafe.Sizeof(physical.Slot{})) + uint64(len(e.children))*16 + uint64(len(e.scalars))*(8+uint64(unsafe.Sizeof(physical.ScalarSubplan{}))) + uint64(len(e.node.Delivered.Ordering))*uint64(unsafe.Sizeof(physical.OrderKey{})) + uint64(len(e.node.Join.Keys))*uint64(unsafe.Sizeof(physical.JoinKey{})) + uint64(len(e.node.Catalog.Columns))*uint64(unsafe.Sizeof(model.ColumnID(0)))
	size += uint64(len(e.node.Aggregate.GroupKeys))*uint64(unsafe.Sizeof(physical.Key{})) + uint64(len(e.node.Aggregate.Combine))*uint64(unsafe.Sizeof(physical.Combine{}))
	if size > uint64(o.options.MemoBytes)-o.memoBytes {
		o.exhausted = true
		return
	}
	if !o.charge(2, uint64(unsafe.Sizeof(e))) {
		return
	}
	g.alternatives = append(g.alternatives, e)
	o.expressions++
	o.memoBytes += size
}
func equivalent(a, b *expression) bool {
	sameShape := a.node.Algorithm == b.node.Algorithm && slices.Equal(a.children, b.children) && a.producer == b.producer && slices.Equal(a.scalars, b.scalars) && slices.Equal(a.node.ScalarSubplans, b.node.ScalarSubplans)
	sameProperties := delivers(a.node.Delivered, b.node.Delivered) && delivers(b.node.Delivered, a.node.Delivered)
	if !sameShape || !sameProperties || !slices.Equal(a.node.Join.Keys, b.node.Join.Keys) {
		return false
	}
	if a.node.Algorithm >= physical.TableScan && a.node.Algorithm <= physical.SecondaryScan {
		return a.node.Scan.Source == b.node.Scan.Source && a.node.Scan.Request.Target == b.node.Scan.Request.Target && slices.Equal(a.node.Scan.ColumnMap, b.node.Scan.ColumnMap) && a.node.Scan.Request.IndexID == b.node.Scan.Request.IndexID && a.node.Scan.Request.Direction == b.node.Scan.Request.Direction
	}
	if a.node.Algorithm == physical.CatalogScan {
		return a.node.Catalog.Relation == b.node.Catalog.Relation && slices.Equal(a.node.Catalog.Columns, b.node.Catalog.Columns)
	}
	return true
}
func (o *optimizer) winner(id ir.RelID, required physical.Properties) *expression {
	var winner *expression
	for _, alternative := range o.groups[id].alternatives {
		if !o.step(1) {
			return nil
		}
		candidate := alternative
		if !delivers(candidate.node.Delivered, required) {
			candidate = o.enforce(candidate, required)
		}
		if o.err != nil {
			return nil
		}
		if winner == nil || candidate.node.Cost.Total < winner.node.Cost.Total {
			winner = candidate
		}
	}
	return winner
}
func (o *optimizer) explore(id ir.RelID) {
	seed := o.groups[id].seed
	r, _ := o.graph.Rel(id)
	if r.Kind == ir.Scan {
		for _, path := range o.access[id].paths {
			o.register(id, path)
		}
		return
	}
	if r.Kind == ir.CTEConsumer || r.Kind == ir.CTEProducer || r.Kind == ir.RecursiveUnion {
		return
	}
	children := allocate[*expression](o, uint64(len(seed.children)))
	if o.err != nil {
		return
	}
	for i, original := range seed.children {
		children[i] = o.winner(original.node.Logical.Rel, seed.node.Required[i])
		if o.err != nil {
			return
		}
	}
	n := seed.node
	candidate := o.newExpression(n, children, seed.producer)
	o.register(id, candidate)
	if !o.search() {
		return
	}
	switch r.Kind {
	case ir.Filter, ir.Project, ir.Sample, ir.LockRows:
		o.exploreOrderPreserving(id, r, seed)
	case ir.Sort:
		o.exploreSort(id, r, seed)
	case ir.Join:
		o.exploreJoin(id, r, seed)
	case ir.Aggregate:
		width, _ := o.graph.GroupingSetWidth(id, 0)
		if r.GroupingSetCount() > 1 || width != r.ExpressionCount() || r.GroupingCount() != 0 {
			return
		}
		for i := range r.AggregateCount() {
			call, _ := o.graph.Aggregate(id, i)
			if call.OrderCount() != 0 {
				return
			}
		}
		if r.AggregateCount() != 0 {
			o.explorePartialAggregate(id, r, seed, children[0])
			if o.err != nil {
				return
			}
		}
		n := seed.node
		n.Algorithm = physical.StreamAggregate
		required := children[0].node.Delivered
		required.Ordering = allocate[physical.OrderKey](o, uint64(len(n.Aggregate.GroupKeys)))
		if o.err != nil {
			return
		}
		for i, key := range n.Aggregate.GroupKeys {
			op := orderingOperator(key.Type)
			if op == 0 {
				return
			}
			required.Ordering[i] = physical.OrderKey{Key: key, Operator: op, Direction: ir.Ascending, Nulls: ir.NullsLast}
		}
		n.Delivered.Ordering = allocate[physical.OrderKey](o, uint64(len(required.Ordering)))
		if o.err != nil {
			return
		}
		for i, order := range required.Ordering {
			order.Key.Slot = n.Output[i]
			n.Delivered.Ordering[i] = order
		}
		child := o.winner(children[0].node.Logical.Rel, required)
		o.register(id, o.newExpression(n, []*expression{child}, nil))
	case ir.Distinct:
		n := seed.node
		n.Algorithm = physical.StreamDistinct
		required := children[0].node.Delivered
		required.Ordering = allocate[physical.OrderKey](o, uint64(len(n.Output)))
		if o.err != nil {
			return
		}
		for i, slot := range n.Output {
			key := o.key(slot.ID)
			op := orderingOperator(key.Type)
			if op == 0 {
				return
			}
			required.Ordering[i] = physical.OrderKey{Key: key, Operator: op, Direction: ir.Ascending, Nulls: ir.NullsLast}
		}
		child := o.winner(children[0].node.Logical.Rel, required)
		o.register(id, o.newExpression(n, []*expression{child}, nil))
	case ir.Limit:
		o.exploreOrderPreserving(id, r, seed)
		if r.WithTies || seed.node.Limit.RuntimeBounds || len(children[0].node.Delivered.Ordering) == 0 {
			return
		}
		n := seed.node
		n.Algorithm = physical.TopK
		n.Delivered.Ordering = children[0].node.Delivered.Ordering
		o.register(id, o.newExpression(n, children, nil))
	}
}

// exploreOrderPreserving registers unary operator alternatives over every
// ordered child alternative (e.g., forward and reverse index scans) so parent
// Sort/Limit/Window/StreamDistinct nodes can exploit existing index order.
// For Project, direct ir.Slot copy bindings also expose aliased order keys.
func (o *optimizer) exploreOrderPreserving(id ir.RelID, r ir.Rel, seed *expression) {
	if len(seed.children) != 1 || len(seed.node.Required) != 1 {
		return
	}
	childRel := seed.children[0].node.Logical.Rel
	required := seed.node.Required[0]
	if r.Kind != ir.Limit || !r.WithTies {
		required.Ordering = nil
	}
	for _, alt := range o.groups[childRel].alternatives {
		if !o.search() {
			return
		}
		if len(alt.node.Delivered.Ordering) == 0 || !delivers(alt.node.Delivered, required) {
			continue
		}
		n := seed.node
		n.Delivered.Ordering = nil
		expr := o.newExpression(n, []*expression{alt}, seed.producer)
		if o.err != nil {
			return
		}
		o.register(id, expr)
		if r.Kind != ir.Project {
			continue
		}
		if aliased := o.projectAliasedOrder(id, r, alt.node.Delivered.Ordering); len(aliased) != 0 {
			an := seed.node
			an.Delivered.Ordering = aliased
			aExpr := o.newExpression(an, []*expression{alt}, seed.producer)
			if o.err != nil {
				return
			}
			o.register(id, aExpr)
		}
	}
}

func (o *optimizer) projectAliasedOrder(id ir.RelID, r ir.Rel, inputOrder []physical.OrderKey) []physical.OrderKey {
	if len(inputOrder) == 0 {
		return nil
	}
	aliased := make([]physical.OrderKey, 0, len(inputOrder))
	changed := false
	for _, key := range inputOrder {
		mapped := key
		foundAlias := false
		for j := range r.BindingCount() {
			binding, _ := o.graph.Binding(id, j)
			if binding.Slot == key.Key.Slot.ID {
				continue
			}
			scalar, ok := o.graph.Scalar(binding.Expr)
			if !ok || scalar.Kind != ir.Slot || scalar.Slot != key.Key.Slot.ID {
				continue
			}
			aliasKey := o.key(binding.Slot)
			if aliasKey.Type != key.Key.Type || aliasKey.Collation != key.Key.Collation {
				continue
			}
			mapped.Key = aliasKey
			foundAlias = true
			changed = true
			break
		}
		if !foundAlias && !slices.Contains(o.groups[id].seed.node.Output, key.Key.Slot) {
			break
		}
		aliased = append(aliased, mapped)
	}
	if !changed || len(aliased) == 0 {
		return nil
	}
	return aliased
}

// exploreSort eliminates an explicit physical.Sort whenever a child alternative
// already delivers the exact SQL ordering and outputs (e.g., a forward or
// reverse PK/index scan propagated through Filter/Project).
func (o *optimizer) exploreSort(id ir.RelID, r ir.Rel, seed *expression) {
	if len(seed.children) != 1 || len(seed.node.Required) != 1 {
		return
	}
	wantOrder := seed.node.Delivered.Ordering
	if len(wantOrder) == 0 {
		return
	}
	childRel := seed.children[0].node.Logical.Rel
	required := seed.node.Required[0]
	required.Ordering = nil
	for _, alt := range o.groups[childRel].alternatives {
		if !o.search() {
			return
		}
		if !delivers(alt.node.Delivered, required) || !ordered(alt.node.Delivered.Ordering, wantOrder) {
			continue
		}
		if !slices.Equal(alt.node.Output, seed.node.Output) {
			continue
		}
		o.register(id, alt)
	}
}

func (o *optimizer) explorePartialAggregate(id ir.RelID, relation ir.Rel, seed *expression, child *expression) {
	combines := allocate[physical.Combine](o, uint64(relation.AggregateCount()))
	if o.err != nil {
		return
	}
	for i := range combines {
		call, _ := o.graph.Aggregate(id, uint32(i))
		combine := physical.AggregateCombine(call.Function)
		modified := call.Distinct || call.Filter != 0 || call.OrderCount() != 0 || call.DirectArgumentCount() != 0
		if combine == 0 || modified {
			return
		}
		output := seed.node.Output[int(relation.ExpressionCount())+i]
		combines[i] = physical.Combine{Aggregate: call.Function, Combine: combine, StateGraph: seed.node.Logical.Graph, StateLayout: output.Layout}
	}
	partialNode := seed.node
	partialNode.Aggregate.Phase = physical.PartialAggregate
	partialNode.Aggregate.Combine = combines
	partialNode.Delivered = child.node.Delivered
	partialNode.Delivered.Distribution.Keys = allocate[physical.Key](o, uint64(len(child.node.Delivered.Distribution.Keys)))
	if o.err != nil {
		return
	}
	copy(partialNode.Delivered.Distribution.Keys, child.node.Delivered.Distribution.Keys)
	partialNode.Delivered.Ordering = nil
	partialNode.Delivered.Rewindability = physical.OnePass
	if partialNode.Delivered.Distribution.Kind != physical.SingleNode {
		for i, key := range partialNode.Delivered.Distribution.Keys {
			group := slices.Index(partialNode.Aggregate.GroupKeys, key)
			if group < 0 {
				return
			}
			partialNode.Delivered.Distribution.Keys[i].Slot = partialNode.Output[group]
		}
	}
	partial := o.newExpression(partialNode, []*expression{child}, nil)
	if partial == nil || o.err != nil {
		return
	}
	required := partial.node.Delivered
	required.Distribution = physical.Distribution{Kind: physical.SingleNode, Partitions: 1}
	required.Ordering = nil
	partial = o.enforce(partial, required)
	if partial == nil || o.err != nil {
		return
	}
	finalNode := seed.node
	finalNode.Aggregate.Phase = physical.FinalAggregate
	finalNode.Aggregate.Combine = combines
	finalNode.Aggregate.GroupKeys = allocate[physical.Key](o, uint64(relation.ExpressionCount()))
	if o.err != nil {
		return
	}
	for i := range finalNode.Aggregate.GroupKeys {
		key := o.key(finalNode.Output[i].ID)
		key.Slot = finalNode.Output[i]
		finalNode.Aggregate.GroupKeys[i] = key
	}
	finalNode.Delivered = required
	o.register(id, o.newExpression(finalNode, []*expression{partial}, nil))
}

func (o *optimizer) exploreJoin(id ir.RelID, r ir.Rel, seed *expression) {
	// Hash, merge and lookup join candidates only reorder fully-consumed sides
	// and recheck every candidate pair against the original predicate, so a
	// join over stable inputs returns the same rows as the nested loop: both
	// sides share one statement snapshot, and a stable input scanned once
	// reads exactly what rescanning it would read. Volatile inputs stay on the
	// nested loop because materializing them once would change their values.
	// Error behavior is pinned by the keys, not the whole relation: joinKeys
	// only admits strict immutable non-erroring equalities, and the residual
	// rechecks those same conjuncts on every candidate pair. A relation whose
	// key conjuncts are clean therefore raises exactly when the nested loop
	// would: on the same pair, on the same conjunct.
	if r.Effects.Volatility == ir.Volatile || r.JoinKind == ir.CrossJoin || r.JoinKind == ir.MarkJoin {
		return
	}
	keys := o.joinKeys(id, seed.children[0].node.Output, seed.children[1].node.Output)
	if len(keys) == 0 || o.err != nil {
		return
	}
	for _, algorithm := range []physical.Algorithm{physical.HashJoin, physical.MergeJoin, physical.LookupJoin} {
		if !o.search() {
			return
		}
		if algorithm == physical.LookupJoin && r.JoinKind != ir.InnerJoin && r.JoinKind != ir.LeftJoin && r.JoinKind != ir.SemiJoin && r.JoinKind != ir.AntiJoin {
			continue
		}
		legal := true
		for _, key := range keys {
			op, _ := builtin.LookupOperator(key.Equality)
			hashUnsupported := algorithm == physical.HashJoin && !op.HashJoinable
			mergeUnsupported := algorithm == physical.MergeJoin && !op.MergeJoinable
			if hashUnsupported || mergeUnsupported {
				legal = false
			}
		}
		if !legal {
			continue
		}
		pair := allocate[*expression](o, 2)
		if o.err != nil {
			return
		}
		for side := range pair {
			required, ordered := o.joinRequirement(algorithm, side, seed.children[side].node.Delivered, keys)
			if o.err != nil {
				return
			}
			legal = ordered
			if !legal {
				break
			}
			pair[side] = o.winner(seed.children[side].node.Logical.Rel, required)
			if o.err != nil {
				return
			}
		}
		if !legal {
			continue
		}
		n := seed.node
		n.Algorithm = algorithm
		n.Join.Keys = keys
		n.Delivered.Ordering = nil
		if algorithm == physical.LookupJoin {
			o.exploreLookups(id, seed.children[1].node.Logical.Rel, n, pair[0], keys)
			continue
		}
		o.register(id, o.newExpression(n, pair, nil))
	}
}

func (o *optimizer) joinRequirement(algorithm physical.Algorithm, side int, required physical.Properties, keys []physical.JoinKey) (physical.Properties, bool) {
	required.Rewindability = physical.OnePass
	required.Ordering = nil
	if algorithm != physical.MergeJoin {
		return required, true
	}
	required.Ordering = allocate[physical.OrderKey](o, uint64(len(keys)))
	if o.err != nil {
		return required, false
	}
	for i, key := range keys {
		k := key.Left
		if side == 1 {
			k = key.Right
		}
		op := orderingOperator(k.Type)
		if op == 0 {
			return required, false
		}
		required.Ordering[i] = physical.OrderKey{Key: k, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: op}
	}
	return required, true
}

func (o *optimizer) exploreLookups(id, rightID ir.RelID, node physical.NodeSpec, left *expression, keys []physical.JoinKey) {
	for _, right := range o.groups[rightID].alternatives {
		if !o.search() {
			return
		}
		if !lookupMatches(o, right, keys) {
			continue
		}
		children := allocate[*expression](o, 2)
		if o.err != nil {
			return
		}
		children[0], children[1] = left, right
		node.Join.Lookup, node.Join.LookupIndex = right.node.Scan.Request.Target, right.node.Scan.Request.IndexID
		o.register(id, o.newExpression(node, children, nil))
	}
}
func lookupMatches(o *optimizer, right *expression, keys []physical.JoinKey) bool {
	if (right.node.Algorithm != physical.PrimaryKeyScan && right.node.Algorithm != physical.SecondaryScan) || right.node.Scan.GIN.Enabled {
		return false
	}
	limited := right.node.Scan.Request.SafeLimit.Enabled || right.node.Scan.AppliedLimit.Enabled || right.node.Scan.LimitOwner != 0
	if limited {
		return false
	}
	layout := right.node.Scan.Request.KeyLayout
	if len(keys) > len(layout) {
		return false
	}
	for i, key := range keys {
		slot, _ := o.graph.Slot(key.Right.Slot.ID)
		if right.node.Scan.PhysicalColumn(slot.Column) != layout[i].ColumnID {
			return false
		}
	}
	return true
}
func (o *optimizer) joinKeys(id ir.RelID, left, right []physical.Slot) []physical.JoinKey {
	r, _ := o.graph.Rel(id)
	var keys []physical.JoinKey
	var visit func(ir.ScalarID, uint32)
	visit = func(id ir.ScalarID, depth uint32) {
		if !o.step(1) {
			return
		}
		if depth >= o.options.IR.MaxDepth {
			o.err = ErrLimit
			return
		}
		s, _ := o.graph.Scalar(id)
		if s.Kind == ir.Bool && s.BoolOp == ir.And {
			for i := range s.OperandCount() {
				child, _ := o.graph.Operand(id, i)
				visit(child, depth+1)
			}
			return
		}
		lhs, _ := o.graph.Operand(id, 0)
		rhs, _ := o.graph.Operand(id, 1)
		lo, _ := o.graph.Scalar(lhs)
		ro, _ := o.graph.Scalar(rhs)
		l, ok := o.graph.UnwrapRelabel(lhs)
		if !ok {
			return
		}
		rr, ok := o.graph.UnwrapRelabel(rhs)
		if !ok {
			return
		}
		if l.Kind != ir.Slot || rr.Kind != ir.Slot {
			return
		}
		lk, rk := o.key(l.Slot), o.key(rr.Slot)
		// The key types come from the predicate operands, not the slots: the
		// binder coerced each side to the resolved operator's argument type,
		// so a relabeled slot carries its coerced type here. Verification
		// (ir/verify_scalar.go) guarantees a Relabel preserves its operand's
		// runtime representation, therefore hashing the underlying slot bytes
		// is consistent with the resolved equality, and the residual
		// rechecks every candidate pair with the original predicate.
		lType, lOK := o.graph.Type(lo.Layout)
		rType, rOK := o.graph.Type(ro.Layout)
		if !lOK || !rOK {
			return
		}
		lk.Type, rk.Type = lType, rType
		lk.Collation, rk.Collation = lType.Collation, rType.Collation
		if lk.Type != rk.Type || lk.Collation != rk.Collation {
			return
		}
		operator := s.Operator
		if slices.Contains(left, lk.Slot) && slices.Contains(right, rk.Slot) {
			// Canonical (left, right) orientation.
		} else if slices.Contains(right, lk.Slot) && slices.Contains(left, rk.Slot) {
			// Reversed orientation is normalized into canonical IR by
			// normalizeJoinPredicates before planning so physical.Plan can verify
			// Operand(0) == Left and Operand(1) == Right; do not emit an
			// unnormalized predicate ref if normalization was skipped under budget.
			return
		} else {
			return
		}
		key := physical.JoinKey{Left: lk, Right: rk, Equality: operator, Nulls: physical.NullsUnequal, Predicate: physical.ScalarRef{Graph: 1, Scalar: id}}
		if !o.charge(2, uint64(unsafe.Sizeof(key))) {
			return
		}
		keys = append(keys, key)
	}
	visit(r.Predicate, 0)
	return keys
}

func (o *optimizer) plan(root *expression) (*physical.Plan, error) {
	if root == nil || o.err != nil {
		return nil, o.err
	}
	if !o.charge(uint64(o.options.Plan.MaxNodes), uint64(unsafe.Sizeof(physical.NodeSpec{}))+64) {
		return nil, o.err
	}
	nodes := make([]physical.NodeSpec, 0, o.options.Plan.MaxNodes)
	ids := make(map[*expression]physical.NodeID)
	worktables := make(map[ir.CTEID]physical.NodeID)
	var emit func(*expression, uint32) physical.NodeID
	emit = func(e *expression, depth uint32) physical.NodeID {
		if !o.step(1) {
			return 0
		}
		if id, exists := ids[e]; exists {
			return id
		}
		if depth >= o.options.Plan.MaxDepth || len(nodes) >= int(o.options.Plan.MaxNodes) {
			o.err = ErrLimit
			return 0
		}
		n := e.node
		n.Children = allocate[physical.NodeID](o, uint64(len(e.children)))
		if o.err != nil {
			return 0
		}
		for i, child := range e.children {
			n.Children[i] = emit(child, depth+1)
			if o.err != nil {
				return 0
			}
		}
		n.ScalarSubplans = allocate[physical.ScalarSubplan](o, uint64(len(e.scalars)))
		if o.err != nil {
			return 0
		}
		for i, body := range e.scalars {
			n.ScalarSubplans[i] = physical.ScalarSubplan{Scalar: e.node.ScalarSubplans[i].Scalar, Root: emit(body, depth+1)}
			if o.err != nil {
				return 0
			}
		}
		if e.producer != nil {
			n.Worktable.Producer = emit(e.producer, depth+1)
			if o.err != nil {
				return 0
			}
		}
		if len(nodes) >= int(o.options.Plan.MaxNodes) {
			o.err = ErrLimit
			return 0
		}
		id := physical.NodeID(len(nodes) + 1)
		if n.Algorithm == physical.RecursiveUnion {
			worktables[n.Worktable.CTE] = id
		}
		nodes = append(nodes, n)
		ids[e] = id
		return id
	}
	dependencies := allocate[physical.NodeID](o, uint64(o.graph.CTECount()))
	dependencyCount := 0
	for relation := ir.RelID(1); relation <= ir.RelID(o.graph.RelCount()) && o.step(1); relation++ {
		r, _ := o.graph.Rel(relation)
		if r.Kind != ir.CTEProducer {
			continue
		}
		cte, _ := o.graph.CTE(r.CTE)
		body, _ := o.graph.Rel(cte.Body)
		if body.Kind == ir.Modify {
			dependencies[dependencyCount] = emit(o.groups[relation].seed, 1)
			dependencyCount++
		}
	}
	slices.Sort(dependencies[:dependencyCount])
	id := emit(root, 0)
	if o.err != nil {
		return nil, o.err
	}
	nodes[id-1].StatementDependencies = dependencies[:dependencyCount]
	for i := range nodes {
		if nodes[i].Algorithm == physical.WorktableScan {
			nodes[i].Worktable.Producer = worktables[nodes[i].Worktable.CTE]
		}
	}
	// Reserve room for one fully materialized plan object: Plan.MaxBytes is that
	// size, and this runs once per Optimize, so it is a size charge and not a
	// per-scan item count. NewPlan re-enforces the same bound on the real nodes.
	if !o.charge(o.options.Plan.MaxBytes, 1) {
		return nil, o.err
	}
	return physical.NewPlan(o.ctx, physical.Spec{Graphs: []*ir.Graph{o.graph}, Nodes: nodes, Root: id}, o.options.Plan)
}
