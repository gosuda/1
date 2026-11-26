package physical

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (p *Plan) validateNode(ctx context.Context, id NodeID) error {
	n := p.nodes[id-1]
	g, logical, err := p.relation(n.Logical)
	if err != nil {
		return err
	}
	if n.Algorithm < TableScan || n.Algorithm > Sample {
		return invalid("unknown algorithm")
	}
	if !validChildCount(n.Algorithm, len(n.Children)) || len(n.Required) != len(n.Children) {
		return invalid("missing children or child requirements")
	}
	kind := logicalKind(n.Algorithm)
	if n.Algorithm == TopK && n.Limit.Scope == PartitionLimit {
		kind = 0
	}
	if n.Algorithm == Sort && p.nodes[n.Children[0]-1].Logical == n.Logical {
		kind = 0
	}
	if kind != 0 && logical.Kind != kind {
		return invalid("algorithm does not implement logical relation")
	}
	if n.Algorithm == UnionAll && (!logical.All || logical.SetKind != ir.Union) {
		return invalid("UnionAll requires logical UNION ALL")
	}
	if uint32(len(n.Output)) != logical.OutputCount() {
		return invalid("logical output width mismatch")
	}
	for i, slot := range n.Output {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := p.slot(slot); err != nil {
			return err
		}
		expected, _ := g.Output(n.Logical.Rel, uint32(i))
		if slot.Graph != n.Logical.Graph || slot.ID != expected {
			return invalid("logical output identity mismatch")
		}
	}
	for _, ref := range n.Expressions {
		graph, ok := p.Graph(ref.Graph)
		if !ok {
			return invalid("expression graph index")
		}
		if _, ok := graph.Scalar(ref.Scalar); !ok {
			return invalid("scalar expression index")
		}
	}
	if err := metadata(n); err != nil {
		return err
	}
	if n.Break != ExpectedBreak(n.Algorithm) {
		return invalid("pipeline breaker mismatch")
	}
	if n.Break != Streaming && n.Memory.PeakBytes == 0 {
		return invalid("pipeline breaker lacks memory estimate")
	}
	if (n.Algorithm == StreamAggregate || n.Algorithm == StreamDistinct) && n.Memory.PeakBytes == 0 {
		return invalid("streaming state lacks memory estimate")
	}
	if err := p.properties(n.Delivered, n.Output); err != nil {
		return err
	}
	for i, childID := range n.Children {
		child := p.nodes[childID-1]
		if err := p.properties(n.Required[i], child.Output); err != nil {
			return err
		}
		if !satisfies(child.Delivered, n.Required[i]) {
			return invalid("child does not deliver required properties")
		}
		if kind == 0 {
			if child.Logical != n.Logical || !slices.Equal(child.Output, n.Output) {
				return invalid("enforcer changes logical identity")
			}
		} else if (n.Algorithm == HashAggregate || n.Algorithm == StreamAggregate) && n.Aggregate.Phase == FinalAggregate {
			if child.Logical != n.Logical || !slices.Equal(child.Output, n.Output) {
				return invalid("final aggregate input is not its partial state")
			}
		} else {
			expected, ok := g.Input(n.Logical.Rel, uint32(i))
			if !ok || child.Logical != (RelRef{n.Logical.Graph, expected}) {
				return invalid("physical child does not implement logical input")
			}
		}
	}
	if err := p.validateSemanticOrdering(n, g, logical); err != nil {
		return err
	}
	if err := p.validatePayload(n); err != nil {
		return err
	}
	if err := p.validateParameters(n, g, logical); err != nil {
		return err
	}
	switch n.Algorithm {
	case TableScan, PrimaryKeyScan, SecondaryScan:
		return p.validateScan(n, logical)
	case CatalogScan:
		if n.Catalog.Relation == 0 || n.Catalog.Relation != logical.Catalog || len(n.Catalog.Columns) != len(n.Output) {
			return invalid("catalog scan identity or projection mismatch")
		}
		for i, column := range n.Catalog.Columns {
			slot, ok := g.Slot(n.Output[i].ID)
			if column == 0 || !ok || slot.Column != column || slot.Object != 0 {
				return invalid("catalog scan column identity mismatch")
			}
		}
	case LookupJoin, NestedLoopJoin, HashJoin, MergeJoin, Apply:
		return p.validateJoin(ctx, n, logical)
	case HashAggregate, StreamAggregate:
		return p.validateAggregate(n, g, logical)
	case LocalGather, LocalOrderedMerge, LocalHashRepartition:
		return p.validateExchange(n)
	case CTEScan, WorktableScan, CTEMaterialize, RecursiveUnion:
		if n.Worktable.Graph != n.Logical.Graph || n.Worktable.CTE != logical.CTE {
			return invalid("worktable logical identity mismatch")
		}
		cte, ok := g.CTE(n.Worktable.CTE)
		invalidFeedback := n.Algorithm == WorktableScan && (!cte.Recursive || !logical.RecursiveReference)
		invalidRecursion := n.Algorithm == RecursiveUnion && !cte.Recursive
		if !ok || invalidFeedback || invalidRecursion {
			return invalid("invalid worktable use")
		}
		if (n.Algorithm == CTEMaterialize || n.Algorithm == RecursiveUnion) && n.Worktable.Producer != 0 {
			return invalid("worktable producer cannot name another producer")
		}
	}
	return p.validateDelivery(ctx, n, logical)
}

func (p *Plan) validatePayload(n NodeSpec) error {
	if n.Algorithm == RowsFrom && len(n.Expressions) != 0 {
		return invalid("ROWS FROM cannot carry expressions")
	}
	scan := n.Algorithm >= TableScan && n.Algorithm <= SecondaryScan
	if !scan && !emptyScan(n.Scan) {
		return invalid("scan payload on non-scan")
	}
	catalogScan := n.Algorithm == CatalogScan
	if !catalogScan && (n.Catalog.Relation != 0 || len(n.Catalog.Columns) != 0) {
		return invalid("catalog payload on non-catalog scan")
	}
	join := n.Algorithm == LookupJoin || n.Algorithm == NestedLoopJoin || n.Algorithm == HashJoin || n.Algorithm == MergeJoin || n.Algorithm == Apply
	hasJoin := len(n.Join.Keys) != 0 || n.Join.NullAware || n.Join.MarkKind != 0 || n.Join.Lookup.ObjectID != 0 || n.Join.LookupIndex != 0
	if !join && hasJoin {
		return invalid("join payload on non-join")
	}
	aggregate := n.Algorithm == HashAggregate || n.Algorithm == StreamAggregate
	hasAggregate := n.Aggregate.Phase != 0 || len(n.Aggregate.GroupKeys) != 0 || len(n.Aggregate.Combine) != 0
	if !aggregate && hasAggregate {
		return invalid("aggregate payload on non-aggregate")
	}
	if n.Algorithm != Limit && n.Algorithm != TopK && n.Limit != (LimitSpec{}) {
		return invalid("limit payload on non-limit")
	}
	exchange := n.Algorithm >= LocalGather && n.Algorithm <= LocalHashRepartition
	if !exchange && n.Exchange != (ExchangeSpec{}) {
		return invalid("exchange payload on non-exchange")
	}
	worktable := n.Algorithm == CTEScan || n.Algorithm == WorktableScan || n.Algorithm == CTEMaterialize || n.Algorithm == RecursiveUnion
	if !worktable && n.Worktable != (WorktableSpec{}) {
		return invalid("worktable payload on unrelated algorithm")
	}
	return nil
}

func (p *Plan) validateParameters(n NodeSpec, g *ir.Graph, logical ir.Rel) error {
	// Captures are typed invocation inputs; they are not VM registers or values.
	if uint32(len(n.Delivered.Parameters)) != g.CaptureCount(logical.Scope) {
		return invalid("parameterization differs from logical scope")
	}
	for i, slot := range n.Delivered.Parameters {
		expected, ok := g.Capture(logical.Scope, uint32(i))
		if !ok || slot.Graph != n.Logical.Graph || slot.ID != expected {
			return invalid("parameter identity mismatch")
		}
	}
	return nil
}

func (p *Plan) validateDelivery(ctx context.Context, n NodeSpec, logical ir.Rel) error {
	d := n.Delivered
	if n.Algorithm == RowsFrom {
		if d.Distribution.Kind != SingleNode || len(d.Ordering) != 0 || d.Rewindability != OnePass {
			return invalid("ROWS FROM requires unordered single-node one-pass output")
		}
		for i, childID := range n.Children {
			if err := ctx.Err(); err != nil {
				return err
			}
			if p.nodes[childID-1].Delivered.Distribution.Kind != SingleNode || n.Required[i].Distribution.Kind != SingleNode {
				return invalid("ROWS FROM requires single-node inputs")
			}
		}
		return nil
	}
	if n.Algorithm == CTEScan {
		return p.validateCTEDelivery(ctx, n)
	}
	if n.Algorithm == CTEMaterialize {
		return p.validateOrdinalDelivery(ctx, n, p.nodes[n.Children[0]-1])
	}
	if len(n.Children) == 0 {
		if d.Distribution.Kind != SingleNode || len(d.Ordering) != 0 {
			return invalid("source cannot supply claimed distribution or ordering")
		}
		if n.Algorithm != Values && d.Rewindability != OnePass {
			return invalid("source rewindability is not represented")
		}
		if d.Rewindability == Rewindable && logical.Effects.Volatility != ir.Immutable {
			return invalid("volatile values cannot be rewound by reevaluation")
		}
		return nil
	}
	input := p.nodes[n.Children[0]-1].Delivered
	if !sameDistribution(d.Distribution, input.Distribution) {
		return invalid("operator changes distribution without exchange")
	}
	if len(n.Children) == 2 && p.nodes[n.Children[1]-1].Delivered.Distribution.Kind != SingleNode {
		return invalid("binary materialization requires single-node inputs")
	}
	if len(n.Children) == 2 && d.Distribution.Kind != SingleNode {
		return invalid("binary materialization requires single-node output")
	}
	switch n.Algorithm {
	case Sort, TopK:
		if len(d.Ordering) == 0 {
			return invalid("ordering enforcer has no ordering")
		}
		if n.Algorithm == TopK {
			if err := p.validateLimit(ctx, n); err != nil {
				return err
			}
		}
	case Limit:
		if !orderPrefix(input.Ordering, d.Ordering) {
			return invalid("limit invents ordering")
		}
		if err := p.validateLimit(ctx, n); err != nil {
			return err
		}
	case HashDistinct, HashSetOp, RecursiveUnion, UnionAll, Modify:
		if len(d.Ordering) != 0 {
			return invalid("unordered stateful operator claims ordering")
		}
		if n.Algorithm == HashDistinct && logical.DistinctOn {
			return invalid("DISTINCT ON needs ordered first-row selection")
		}
	case StreamDistinct:
		if err := p.validateDistinctOrder(ctx, n, logical); err != nil {
			return err
		}
		if !orderPrefix(input.Ordering, d.Ordering) {
			return invalid("stream distinct invents ordering")
		}
	default:
		if !orderPrefix(input.Ordering, d.Ordering) {
			return invalid("operator invents ordering")
		}
	}
	if d.Rewindability == Rewindable && input.Rewindability != Rewindable && n.Break != MaterializeInput && n.Break != MaterializeBoth {
		return invalid("rewind requires materialization or rewindable input")
	}
	return nil
}

func (p *Plan) validateExchange(n NodeSpec) error {
	input := p.nodes[n.Children[0]-1].Delivered
	d := n.Delivered
	if n.Exchange.Inputs != input.Distribution.Partitions || n.Exchange.Inputs < 1 || n.Exchange.QueueBytes == 0 || n.Exchange.QueueBytes > n.Memory.PeakBytes {
		return invalid("exchange lacks bounded input queues")
	}
	if d.Rewindability != OnePass {
		return invalid("exchange queues cannot rewind")
	}
	switch n.Algorithm {
	case LocalGather:
		if d.Distribution.Kind != SingleNode || len(d.Ordering) != 0 || input.Distribution.Kind == Replicated {
			return invalid("gather cannot preserve order or deduplicate replicas")
		}
	case LocalOrderedMerge:
		if d.Distribution.Kind != SingleNode || len(d.Ordering) == 0 || !orderPrefix(input.Ordering, d.Ordering) || input.Distribution.Kind == Replicated {
			return invalid("ordered merge requires ordered, nonreplicated inputs")
		}
	case LocalHashRepartition:
		if d.Distribution.Kind != HashPartitioned || len(d.Ordering) != 0 || input.Distribution.Kind == Replicated {
			return invalid("hash exchange only supplies unordered hash partitions")
		}
	}
	return nil
}
