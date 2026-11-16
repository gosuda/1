package opt

import (
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (o *optimizer) inventory() {
	parents := allocate[uint32](o, uint64(o.graph.RelCount())+1)
	if o.err != nil {
		return
	}
	for id := ir.RelID(1); id <= ir.RelID(o.graph.RelCount()) && o.step(1); id++ {
		r, _ := o.graph.Rel(id)
		if r.OutputCount() > o.options.MaxOutputColumns || r.InputCount() > o.options.IR.MaxOperands || r.ExpressionCount() > o.options.IR.MaxOperands {
			o.err = ErrLimit
			return
		}
		for i := range r.InputCount() {
			child, _ := o.graph.Input(id, i)
			parents[child]++
		}
		var countScalar func(ir.ScalarID, uint32)
		countScalar = func(scalar ir.ScalarID, depth uint32) {
			if scalar == 0 || !o.step(1) {
				return
			}
			if depth >= o.options.IR.MaxDepth {
				o.err = ErrLimit
				return
			}
			s, _ := o.graph.Scalar(scalar)
			if s.Kind == ir.Subquery {
				parents[s.Relation]++
			}
			for j := range s.OperandCount() {
				child, _ := o.graph.Operand(scalar, j)
				countScalar(child, depth+1)
			}
		}
		o.relationScalars(&r, id, func(scalar ir.ScalarID) { countScalar(scalar, 0) })
	}
	for id := ir.RelID(1); id <= ir.RelID(o.graph.RelCount()) && o.step(1); id++ {
		r, _ := o.graph.Rel(id)
		if r.Kind != ir.Filter || r.SecurityBarrier || r.Effects.Volatility != ir.Immutable || r.Effects.MayError {
			continue
		}
		child, _ := o.graph.Input(id, 0)
		if parents[child] != 1 {
			continue
		}
		outputs := allocate[model.SlotID](o, uint64(r.OutputCount()))
		for i := range outputs {
			outputs[i], _ = o.graph.Output(id, uint32(i))
		}
		o.propagateScanPredicate(child, r.Predicate, outputs, parents, 0)
	}
}

func (o *optimizer) propagateScanPredicate(id ir.RelID, predicate ir.ScalarID, outputs []model.SlotID, parents []uint32, depth uint32) {
	if id == 0 || parents[id] != 1 || depth >= o.options.IR.MaxDepth || !o.step(1) {
		return
	}
	relation, _ := o.graph.Rel(id)
	if relation.SecurityBarrier || relation.Effects.Volatility != ir.Immutable || relation.Effects.MayError {
		return
	}
	if int(relation.OutputCount()) != len(outputs) {
		return
	}
	switch relation.Kind {
	case ir.Scan:
		o.predicates[id] = predicate
		o.predicateSlots[id] = outputs
	case ir.SetOp:
		for i := range relation.InputCount() {
			child, _ := o.graph.Input(id, i)
			o.propagateScanPredicate(child, predicate, outputs, parents, depth+1)
		}
	case ir.Project:
		child, _ := o.graph.Input(id, 0)
		input, _ := o.graph.Rel(child)
		mapped := allocate[model.SlotID](o, uint64(input.OutputCount()))
		for i := range outputs {
			output, _ := o.graph.Output(id, uint32(i))
			for j := range relation.BindingCount() {
				binding, _ := o.graph.Binding(id, j)
				if binding.Slot != output {
					continue
				}
				scalar, _ := o.graph.Scalar(binding.Expr)
				if scalar.Kind != ir.Slot {
					break
				}
				for k := range input.OutputCount() {
					slot, _ := o.graph.Output(child, k)
					if slot == scalar.Slot {
						mapped[k] = outputs[i]
						break
					}
				}
				break
			}
		}
		o.propagateScanPredicate(child, predicate, mapped, parents, depth+1)
	}
}
func (o *optimizer) base(id ir.RelID) physical.NodeSpec {
	r, _ := o.graph.Rel(id)
	n := physical.NodeSpec{Logical: physical.RelRef{Graph: 1, Rel: id}, Break: physical.Streaming}
	n.Output = allocate[physical.Slot](o, uint64(r.OutputCount()))
	n.Delivered = physical.Properties{Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1}, Rewindability: physical.OnePass}
	n.Delivered.Parameters = allocate[physical.Slot](o, uint64(o.graph.CaptureCount(r.Scope)))
	if o.err != nil {
		return n
	}
	for i := range n.Output {
		slot, _ := o.graph.Output(id, uint32(i))
		n.Output[i] = o.slot(slot)
	}
	for i := range n.Delivered.Parameters {
		slot, _ := o.graph.Capture(r.Scope, uint32(i))
		n.Delivered.Parameters[i] = o.slot(slot)
	}
	// References remain in their original graph. Every scalar-bearing relation
	// payload is still available through Logical; this list is not a new IR.
	o.relationScalars(&r, id, func(scalar ir.ScalarID) { o.addScalar(&n, scalar, 0) })
	return n
}
func (o *optimizer) addScalar(n *physical.NodeSpec, id ir.ScalarID, depth uint32) {
	if id == 0 || !o.step(1) {
		return
	}
	if depth >= o.options.IR.MaxDepth {
		o.err = ErrLimit
		return
	}
	s, _ := o.graph.Scalar(id)
	if s.Kind == ir.Subquery {
		ref := physical.ScalarRef{Graph: 1, Scalar: id}
		found := false
		for _, subplan := range n.ScalarSubplans {
			found = found || subplan.Scalar == ref
		}
		if !found {
			if !o.charge(2, uint64(unsafe.Sizeof(physical.ScalarSubplan{}))) {
				return
			}
			n.ScalarSubplans = append(n.ScalarSubplans, physical.ScalarSubplan{Scalar: ref})
		}
	}
	for i := range s.OperandCount() {
		child, _ := o.graph.Operand(id, i)
		o.addScalar(n, child, depth+1)
	}
	if depth != 0 {
		return
	}
	if !o.charge(2, uint64(unsafe.Sizeof(physical.ScalarRef{}))) {
		return
	}
	n.Expressions = append(n.Expressions, physical.ScalarRef{Graph: 1, Scalar: id})
}
func (o *optimizer) distinctOrdering(id ir.RelID, r ir.Rel) []physical.OrderKey {
	if order := o.order(id); len(order) != 0 {
		return order
	}
	order := allocate[physical.OrderKey](o, uint64(r.ExpressionCount()))
	if o.err != nil {
		return nil
	}
	for i := range order {
		scalar, _ := o.graph.Expression(id, uint32(i))
		expr, _ := o.graph.Scalar(scalar)
		if expr.Kind != ir.Slot {
			o.unsupported(id, "computed DISTINCT ON key requires a logical projection")
			return nil
		}
		key := o.key(expr.Slot)
		op := orderingOperator(key.Type)
		if op == 0 {
			o.unsupported(id, "DISTINCT ON type lacks ordering evidence")
			return nil
		}
		order[i] = physical.OrderKey{Key: key, Operator: op, Direction: ir.Ascending, Nulls: ir.NullsLast}
	}
	return order
}

func (o *optimizer) seed(id ir.RelID, depth uint32) *expression {
	if !o.step(1) {
		return nil
	}
	if depth >= o.options.IR.MaxDepth {
		o.err = ErrLimit
		return nil
	}
	g := &o.groups[id]
	if g.seed != nil {
		return g.seed
	}
	if g.active {
		o.unsupported(id, "cyclic ordinary CTE dependency")
		return nil
	}
	g.active = true
	r, _ := o.graph.Rel(id)
	n := o.base(id)
	children := allocate[*expression](o, uint64(r.InputCount()))
	if o.err != nil {
		return nil
	}
	for i := range children {
		child, _ := o.graph.Input(id, uint32(i))
		children[i] = o.seed(child, depth+1)
		if o.err != nil {
			return nil
		}
	}
	for _, subplan := range n.ScalarSubplans {
		scalar, _ := o.graph.Scalar(subplan.Scalar.Scalar)
		o.seed(scalar.Relation, depth+1)
		if o.err != nil {
			return nil
		}
	}
	var producer *expression
	switch r.Kind {
	case ir.Scan:
		paths := o.scans(id, n)
		if o.err != nil {
			return nil
		}
		g.seed = paths[0]
	case ir.CatalogScan:
		n.Algorithm = physical.CatalogScan
		n.Catalog.Relation = r.Catalog
		n.Catalog.Columns = allocate[model.ColumnID](o, uint64(len(n.Output)))
		for i, output := range n.Output {
			slot, _ := o.graph.Slot(output.ID)
			n.Catalog.Columns[i] = slot.Column
		}
	case ir.Values:
		n.Algorithm = physical.Values
		if r.Effects.Volatility == ir.Immutable {
			n.Delivered.Rewindability = physical.Rewindable
		}
	case ir.GenerateSeries:
		n.Algorithm = physical.GenerateSeries
	case ir.GenerateSubscripts:
		n.Algorithm = physical.GenerateSubscripts
	case ir.Unnest:
		n.Algorithm = physical.Unnest
	case ir.RowsFrom:
		n.Algorithm = physical.RowsFrom
		for i, child := range children {
			required := child.node.Delivered
			required.Distribution = physical.Distribution{Kind: physical.SingleNode, Partitions: 1}
			required.Ordering = nil
			required.Rewindability = physical.OnePass
			children[i] = o.enforce(child, required)
			if o.err != nil {
				return nil
			}
		}
	case ir.Filter:
		n.Algorithm = physical.Filter
	case ir.Project:
		n.Algorithm = physical.Project
	case ir.ProjectSet:
		n.Algorithm = physical.ProjectSet
	case ir.Join:
		n.Algorithm = physical.NestedLoopJoin
		needed := children[1].node.Delivered
		needed.Rewindability = physical.Rewindable
		children[1] = o.enforce(children[1], needed)
		n.Join.MarkKind = r.MarkKind
		n.Join.NullAware = r.JoinKind == ir.MarkJoin && (r.MarkKind == ir.AnySubquery || r.MarkKind == ir.AllSubquery)
	case ir.Apply:
		n.Algorithm = physical.Apply
	case ir.Sort:
		n.Algorithm = physical.Sort
		n.Delivered.Ordering = o.order(id)
	case ir.Limit:
		n.Algorithm = physical.Limit
		n.Limit = o.limit(r)
		if r.WithTies {
			n.Algorithm = physical.TopK
			n.Delivered.Ordering = children[0].node.Delivered.Ordering
			if r.OrderCount() != 0 {
				n.Delivered.Ordering = o.order(id)
			}
		}
	case ir.Aggregate:
		n.Algorithm = physical.HashAggregate
		n.Aggregate.Phase = physical.CompleteAggregate
		n.Aggregate.GroupKeys = allocate[physical.Key](o, uint64(r.ExpressionCount()))
		if o.err != nil {
			return nil
		}
		for i := range n.Aggregate.GroupKeys {
			exprID, _ := o.graph.Expression(id, uint32(i))
			expr, _ := o.graph.Scalar(exprID)
			if expr.Kind != ir.Slot {
				o.unsupported(id, "computed grouping key requires an explicit logical projection")
				return nil
			}
			n.Aggregate.GroupKeys[i] = o.key(expr.Slot)
		}
	case ir.Distinct:
		n.Algorithm = physical.HashDistinct
		if r.DistinctOn {
			n.Algorithm = physical.StreamDistinct
			order := o.distinctOrdering(id, r)
			if o.err != nil {
				return nil
			}
			required := children[0].node.Delivered
			required.Ordering = order
			children[0] = o.enforce(children[0], required)
		}
	case ir.Window:
		n.Algorithm = physical.Window
		order := allocate[physical.OrderKey](o, uint64(r.PartitionCount()+r.OrderCount()))
		if o.err != nil {
			return nil
		}
		for i := uint32(0); i < r.PartitionCount(); i++ {
			scalar, _ := o.graph.Partition(id, i)
			expr, _ := o.graph.Scalar(scalar)
			if expr.Kind != ir.Slot {
				o.unsupported(id, "computed window partition key requires an explicit logical projection")
				return nil
			}
			key := o.key(expr.Slot)
			op := orderingOperator(key.Type)
			if op == 0 {
				o.unsupported(id, "window partition type lacks a resolved ordering operator")
				return nil
			}
			order[i] = physical.OrderKey{Key: key, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: op}
		}
		copy(order[r.PartitionCount():], o.order(id))
		required := children[0].node.Delivered
		required.Ordering = order
		children[0] = o.enforce(children[0], required)
	case ir.SetOp:
		n.Algorithm = physical.UnionAll
		if !r.All || r.SetKind != ir.Union {
			n.Algorithm = physical.HashSetOp
		}
	case ir.CTEProducer:
		n.Algorithm = physical.CTEMaterialize
		n.Worktable = physical.WorktableSpec{Graph: 1, CTE: r.CTE}
	case ir.CTEConsumer:
		n.Worktable = physical.WorktableSpec{Graph: 1, CTE: r.CTE}
		if r.RecursiveReference {
			n.Algorithm = physical.WorktableScan
		} else {
			n.Algorithm = physical.CTEScan
			for other := ir.RelID(1); other <= ir.RelID(o.graph.RelCount()) && o.step(1); other++ {
				candidate, _ := o.graph.Rel(other)
				if candidate.Kind == ir.CTEProducer && candidate.CTE == r.CTE {
					producer = o.seed(other, depth+1)
					break
				}
			}
			if producer == nil && o.err == nil {
				o.unsupported(id, "CTE consumer has no logical producer relation")
			}
		}
	case ir.RecursiveUnion:
		n.Algorithm = physical.RecursiveUnion
		n.Worktable = physical.WorktableSpec{Graph: 1, CTE: r.CTE}
	case ir.LockRows:
		n.Algorithm = physical.LockRows
	case ir.Sample:
		n.Algorithm = physical.Sample
	case ir.Modify:
		n.Algorithm = physical.Modify
	default:
		o.unsupported(id, "logical relation has no physical algorithm")
	}
	if o.err != nil {
		return nil
	}
	if g.seed == nil {
		g.seed = o.newExpression(n, children, producer)
	}
	if o.err != nil {
		return nil
	}
	g.logical = id
	g.properties = logicalProperties{output: g.seed.node.Output, parameters: g.seed.node.Delivered.Parameters, estimate: g.seed.node.Estimate}
	g.properties.keys = o.deriveKeys(id)
	g.alternatives = allocate[*expression](o, 1)
	if o.err != nil {
		return nil
	}
	g.alternatives[0] = g.seed
	g.active = false
	if r.Kind == ir.RecursiveUnion {
		o.producers[r.CTE] = g.seed
	}
	return g.seed
}
func (o *optimizer) order(id ir.RelID) []physical.OrderKey {
	r, _ := o.graph.Rel(id)
	result := allocate[physical.OrderKey](o, uint64(r.OrderCount()))
	if o.err != nil {
		return nil
	}
	for i := range result {
		key, _ := o.graph.Order(id, uint32(i))
		expr, _ := o.graph.Scalar(key.Expr)
		if expr.Kind != ir.Slot {
			o.unsupported(id, "computed ordering key requires an explicit logical projection")
			return nil
		}
		result[i] = physical.OrderKey{Key: o.key(expr.Slot), Direction: key.Direction, Nulls: key.Nulls, Operator: key.Operator}
	}
	return result
}
func orderingOperator(typ model.Type) model.OperatorID {
	var compatible model.OperatorID
	for operator := range builtin.OperatorsNamed("<") {
		if operator.Left != operator.Right || operator.Result != 16 {
			continue
		}
		if operator.Left == typ.ID {
			return operator.ID
		}
		if compatible == 0 && builtin.KeyOrderingEquivalent(typ, operator.Left) {
			compatible = operator.ID
		}
	}
	return compatible
}
func (o *optimizer) limit(r ir.Rel) physical.LimitSpec {
	offset, offsetOK := o.constantCount(r.Offset, true)
	count, countOK := o.constantCount(r.Count, false)
	if !offsetOK || !countOK {
		return physical.LimitSpec{Scope: physical.GlobalLimit, RuntimeBounds: true, WithTies: r.WithTies}
	}
	if count > ^uint64(0)-offset {
		return physical.LimitSpec{Scope: physical.GlobalLimit, RuntimeBounds: true, WithTies: r.WithTies}
	}
	return physical.LimitSpec{Scope: physical.GlobalLimit, Offset: offset, Count: count, WithTies: r.WithTies}
}
func (o *optimizer) constantCount(id ir.ScalarID, absent bool) (uint64, bool) {
	if id == 0 {
		return 0, absent
	}
	s, _ := o.graph.Scalar(id)
	if s.Kind != ir.Const {
		return 0, false
	}
	v, err := o.graph.CloneConstant(o.ctx, id, 1)
	if err != nil {
		o.err = err
		return 0, false
	}
	if v.IsNull || v.Type != model.TypeInt64 || v.IntVal < 0 {
		return 0, false
	}
	return uint64(v.IntVal), true
}
func outputOrder(order []physical.OrderKey, output []physical.Slot) []physical.OrderKey {
	for i, key := range order {
		if !slices.Contains(output, key.Key.Slot) {
			return order[:i]
		}
	}
	return order
}
