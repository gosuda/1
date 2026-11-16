package physical

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (p *Plan) validateJoin(ctx context.Context, n NodeSpec, logical ir.Rel) error {
	left, right := p.nodes[n.Children[0]-1], p.nodes[n.Children[1]-1]
	keys := n.Join.Keys
	keyed := n.Algorithm == HashJoin || n.Algorithm == MergeJoin || n.Algorithm == LookupJoin
	if keyed && len(keys) == 0 {
		return invalid("keyed join lacks equality keys")
	}
	for _, key := range keys {
		if err := p.key(key.Left, left.Output); err != nil {
			return err
		}
		if err := p.key(key.Right, right.Output); err != nil {
			return err
		}
		validNullEquality := key.Nulls == NullsUnequal || key.Nulls == NullsEqual
		if key.Left.Type != key.Right.Type || key.Left.Collation != key.Right.Collation || key.Equality == 0 || !validNullEquality {
			return invalid("incompatible join equality keys")
		}
		if err := joinEquality(n.Algorithm, key); err != nil {
			return err
		}
		if err := p.joinPredicate(ctx, n.Logical, logical.Predicate, key); err != nil {
			return err
		}
	}
	if logical.JoinKind == ir.CrossJoin && len(keys) != 0 {
		return invalid("cross join cannot prune by equality")
	}
	if logical.JoinKind == ir.MarkJoin {
		if n.Join.MarkKind != logical.MarkKind {
			return invalid("mark join subquery semantics differ")
		}
		if (logical.MarkKind == ir.AnySubquery || logical.MarkKind == ir.AllSubquery) && !n.Join.NullAware {
			return invalid("ANY/ALL mark requires three-valued null tracking")
		}
		if n.Join.NullAware && (left.Delivered.Distribution.Kind != SingleNode || right.Delivered.Distribution.Kind != SingleNode) {
			return invalid("three-valued marks require complete inputs, not equality partitions")
		}
	} else if n.Join.MarkKind != 0 {
		return invalid("mark semantics on non-mark join")
	}
	if n.Join.NullAware && logical.JoinKind != ir.MarkJoin {
		return invalid("null-aware NOT IN must retain its logical ANY/ALL mark")
	}
	if n.Algorithm == LookupJoin {
		if logical.JoinKind != ir.InnerJoin && logical.JoinKind != ir.LeftJoin && logical.JoinKind != ir.SemiJoin && logical.JoinKind != ir.AntiJoin {
			return invalid("lookup join cannot enumerate unmatched right rows")
		}
		if right.Algorithm != PrimaryKeyScan && right.Algorithm != SecondaryScan {
			return invalid("lookup right side must name a real keyed scan")
		}
		if n.Join.Lookup != right.Scan.Request.Target || n.Join.LookupIndex != right.Scan.Request.IndexID {
			return invalid("lookup object/index differs from scan")
		}
		if len(keys) > len(right.Scan.Request.KeyLayout) {
			return invalid("lookup exceeds index key width")
		}
		g, _ := p.Graph(right.Logical.Graph)
		for i, key := range keys {
			slot, _ := g.Slot(key.Right.Slot.ID)
			if right.Scan.PhysicalColumn(slot.Column) != right.Scan.Request.KeyLayout[i].ColumnID {
				return invalid("lookup keys are not an index prefix")
			}
		}
	} else if n.Join.Lookup.ObjectID != 0 || n.Join.LookupIndex != 0 {
		return invalid("lookup metadata on non-lookup join")
	}
	if n.Algorithm == NestedLoopJoin && right.Delivered.Rewindability != Rewindable {
		return invalid("nested loop requires rewindable inner input")
	}
	if n.Algorithm == Apply {
		for _, parameter := range right.Delivered.Parameters {
			if !containsSlot(left.Output, parameter) && !slices.Contains(n.Delivered.Parameters, parameter) {
				return invalid("Apply cannot bind inner parameter")
			}
		}
	} else if len(right.Delivered.Parameters) != len(n.Delivered.Parameters) {
		return invalid("correlated inner requires Apply")
	}
	if !joinDistribution(left.Delivered.Distribution, right.Delivered.Distribution, keys) {
		return invalid("join inputs are not colocated by typed keys and epoch")
	}
	preserved := left.Delivered.Distribution
	if logical.JoinKind == ir.RightJoin {
		preserved = right.Delivered.Distribution
	}
	if logical.JoinKind == ir.FullJoin {
		preserved = Distribution{Kind: SingleNode, Partitions: 1}
	}
	if !sameDistribution(n.Delivered.Distribution, preserved) {
		if logical.JoinKind == ir.FullJoin {
			return invalid("full join output must be a truthful single-node stream")
		}
		return invalid("join output distribution does not preserve its key side")
	}
	if n.Delivered.Rewindability != OnePass {
		return invalid("join output rewind requires a separate materialization")
	}
	if n.Algorithm == MergeJoin {
		if len(left.Delivered.Ordering) < len(keys) || len(right.Delivered.Ordering) < len(keys) {
			return invalid("merge join inputs lack key ordering")
		}
		for i, key := range keys {
			l, r := left.Delivered.Ordering[i], right.Delivered.Ordering[i]
			if l.Key != key.Left || r.Key != key.Right || l.Direction != r.Direction || l.Nulls != r.Nulls || l.Operator != r.Operator {
				return invalid("merge key ordering mismatch")
			}
		}
	}
	if len(n.Delivered.Ordering) != 0 {
		if n.Algorithm == HashJoin || logical.JoinKind == ir.RightJoin || logical.JoinKind == ir.FullJoin || !orderPrefix(left.Delivered.Ordering, n.Delivered.Ordering) {
			return invalid("join cannot preserve claimed ordering")
		}
	}
	return nil
}
func joinDistribution(left, right Distribution, keys []JoinKey) bool {
	if left.Kind == SingleNode && right.Kind == SingleNode {
		return true
	}
	partitioned := left.Kind == HashPartitioned || left.Kind == RangePartitioned
	samePartitioning := left.Kind == right.Kind && left.Partitions == right.Partitions && left.Function == right.Function && left.Epoch == right.Epoch
	if !partitioned || !samePartitioning || len(left.Keys) != len(right.Keys) {
		return false
	}
	for i, l := range left.Keys {
		found := false
		for _, pair := range keys {
			found = found || (pair.Left == l && pair.Right == right.Keys[i])
		}
		if !found {
			return false
		}
	}
	return true
}

func AggregateCombine(function model.FunctionID) model.FunctionID {
	switch function {
	case 2147, 2803:
		return 463
	case 2107, 2114:
		return 1724
	case 2108, 2109:
		return 463
	case 2110:
		return 204
	case 2111:
		return 218
	case 2113:
		return 1169
	default:
		return 0
	}
}

func (p *Plan) partialAggregateInput(id NodeID, logical RelRef) bool {
	for id != 0 {
		node := p.nodes[id-1]
		if node.Logical != logical {
			return false
		}
		if node.Algorithm == HashAggregate || node.Algorithm == StreamAggregate {
			return node.Aggregate.Phase == PartialAggregate
		}
		enforcer := logicalKind(node.Algorithm) == 0 || node.Algorithm == Sort
		if !enforcer || len(node.Children) != 1 {
			return false
		}
		id = node.Children[0]
	}
	return false
}

func (p *Plan) validateAggregate(n NodeSpec, g *ir.Graph, logical ir.Rel) error {
	input := p.nodes[n.Children[0]-1]
	a := n.Aggregate
	if a.Phase < CompleteAggregate || a.Phase > FinalAggregate {
		return invalid("aggregate phase missing")
	}
	if uint32(len(a.GroupKeys)) != logical.ExpressionCount() {
		return invalid("aggregate grouping key width differs")
	}
	for i, key := range a.GroupKeys {
		if err := p.key(key, input.Output); err != nil {
			return err
		}
		if a.Phase == FinalAggregate {
			if i >= len(input.Output) || key.Slot != input.Output[i] {
				return invalid("final aggregate grouping key is not its partial output")
			}
			continue
		}
		exprID, _ := g.Expression(n.Logical.Rel, uint32(i))
		expr, _ := g.Scalar(exprID)
		if expr.Kind != ir.Slot || expr.Slot != key.Slot.ID || key.Slot.Graph != n.Logical.Graph {
			return invalid("grouping expression must be projected into its declared key slot")
		}
	}
	if a.Phase != CompleteAggregate && uint32(len(a.Combine)) != logical.AggregateCount() {
		return invalid("partial/final aggregate lacks combine declarations")
	}
	if a.Phase == CompleteAggregate && len(a.Combine) != 0 {
		return invalid("complete aggregate has unused combine declarations")
	}
	if a.Phase == FinalAggregate && !p.partialAggregateInput(n.Children[0], n.Logical) {
		return invalid("final aggregate input is not rooted in a partial aggregate")
	}
	for i, combine := range a.Combine {
		call, _ := g.Aggregate(n.Logical.Rel, uint32(i))
		state, ok := p.Graph(combine.StateGraph)
		if !ok || combine.StateGraph != n.Logical.Graph || int(logical.ExpressionCount())+i >= len(n.Output) || combine.StateLayout != n.Output[int(logical.ExpressionCount())+i].Layout {
			return invalid("aggregate combine state incomplete")
		}
		stateType, ok := state.Type(combine.StateLayout)
		signature, signatureOK := builtin.LookupSignature(combine.Combine)
		validSignature := signatureOK && signature.Kind == 'f' && signature.Volatility == 'i' && signature.Strict &&
			signature.ArgumentCount == 2 && signature.Arguments[0] == stateType.ID &&
			signature.Arguments[1] == stateType.ID && signature.Result == stateType.ID
		modified := call.Distinct || call.Filter != 0 || call.OrderCount() != 0 || call.DirectArgumentCount() != 0
		if !ok || modified || combine.Aggregate != call.Function || combine.Combine != AggregateCombine(call.Function) || !validSignature {
			return invalid("aggregate combine contract is not executable")
		}
	}
	if n.Algorithm == StreamAggregate {
		if len(input.Delivered.Ordering) < len(a.GroupKeys) {
			return invalid("stream aggregation lacks grouping order")
		}
		for i, key := range a.GroupKeys {
			if input.Delivered.Ordering[i].Key != key {
				return invalid("stream aggregation grouping order mismatch")
			}
		}
	}
	if err := validateAggregateDistribution(n, input, g, logical); err != nil {
		return err
	}
	if err := validateAggregateOrdering(n, input); err != nil {
		return err
	}
	if n.Delivered.Rewindability == Rewindable && n.Algorithm != HashAggregate && input.Delivered.Rewindability != Rewindable {
		return invalid("stream aggregate cannot rewind one-pass input")
	}
	return nil
}

func (p *Plan) joinPredicate(ctx context.Context, relation RelRef, root ir.ScalarID, key JoinKey) error {
	if key.Predicate.Graph != relation.Graph || key.Predicate.Scalar == 0 {
		return invalid("join key lacks logical predicate identity")
	}
	g, _ := p.Graph(relation.Graph)
	call, ok := g.Scalar(key.Predicate.Scalar)
	changesNullEquality := call.Strict && key.Nulls == NullsEqual
	if !ok || call.Kind != ir.Call || call.Operator != key.Equality || call.OperandCount() != 2 || changesNullEquality {
		return invalid("join key does not name its comparison")
	}
	leftID, _ := g.Operand(key.Predicate.Scalar, 0)
	rightID, _ := g.Operand(key.Predicate.Scalar, 1)
	left, _ := g.Scalar(leftID)
	right, _ := g.Scalar(rightID)
	leftMatches := left.Kind == ir.Slot && key.Left.Slot.Graph == relation.Graph && left.Slot == key.Left.Slot.ID
	rightMatches := right.Kind == ir.Slot && key.Right.Slot.Graph == relation.Graph && right.Slot == key.Right.Slot.ID
	if !leftMatches || !rightMatches {
		return invalid("join comparison operands differ from physical keys")
	}
	remaining := uint64(g.ScalarCount())
	found, err := conjunctionContains(ctx, g, root, key.Predicate.Scalar, &remaining)
	if err != nil {
		return err
	}
	if !found {
		return invalid("join key is not a required logical conjunct")
	}
	return nil
}
func conjunctionContains(ctx context.Context, g *ir.Graph, root, target ir.ScalarID, remaining *uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if *remaining == 0 {
		return false, ErrLimit
	}
	*remaining--
	if root == target {
		return true, nil
	}
	node, ok := g.Scalar(root)
	if !ok || node.Kind != ir.Bool || node.BoolOp != ir.And {
		return false, nil
	}
	for i := range node.OperandCount() {
		child, _ := g.Operand(root, i)
		found, err := conjunctionContains(ctx, g, child, target, remaining)
		if found || err != nil {
			return found, err
		}
	}
	return false, nil
}

func joinEquality(algorithm Algorithm, key JoinKey) error {
	operator, ok := builtin.LookupOperator(key.Equality)
	if !ok || operator.Left != key.Left.Type.ID || operator.Right != key.Right.Type.ID || operator.Result != 16 {
		return invalid("join equality lacks a matching resolved operator")
	}
	if !operator.HashJoinable && !operator.MergeJoinable {
		return invalid("join key operator has no proven equality semantics")
	}
	if algorithm == HashJoin && !operator.HashJoinable {
		return invalid("join equality does not support hashing")
	}
	if algorithm == MergeJoin && !operator.MergeJoinable {
		return invalid("join equality does not support merging")
	}
	return nil
}
