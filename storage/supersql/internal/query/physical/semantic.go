package physical

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (p *Plan) validateWorktable(ctx context.Context, id NodeID) error {
	n := p.nodes[id-1]
	if n.Algorithm != CTEScan && n.Algorithm != WorktableScan {
		return nil
	}
	producerID := n.Worktable.Producer
	if producerID == 0 || uint64(producerID) > uint64(len(p.nodes)) || producerID == id {
		return invalid("worktable producer index missing")
	}
	producer := p.nodes[producerID-1]
	if producer.Worktable.Graph != n.Worktable.Graph || producer.Worktable.CTE != n.Worktable.CTE {
		return invalid("worktable producer identity mismatch")
	}
	if n.Algorithm == CTEScan && producer.Algorithm != CTEMaterialize {
		return invalid("CTE scan requires materialized producer")
	}
	if n.Algorithm == WorktableScan && (producer.Algorithm != RecursiveUnion || producerID < id) {
		return invalid("recursive scan requires its enclosing recursive union")
	}
	if n.Algorithm == CTEScan {
		return nil
	}
	return p.validateOrdinalLayouts(ctx, n.Output, producer.Output)
}

func (p *Plan) validateSemanticOrdering(n NodeSpec, g *ir.Graph, logical ir.Rel) error {
	semanticSort := n.Algorithm == Sort && p.nodes[n.Children[0]-1].Logical != n.Logical
	if semanticSort && logical.Kind == ir.Sort {
		return logicalOrder(g, n.Logical, n.Delivered.Ordering, 0)
	}
	if n.Algorithm == TopK && n.Limit.Scope == GlobalLimit && logical.OrderCount() != 0 {
		return logicalOrder(g, n.Logical, n.Delivered.Ordering, 0)
	}
	if n.Algorithm == TopK && n.Limit.Scope == GlobalLimit && logical.OrderCount() == 0 {
		input := p.nodes[n.Children[0]-1].Delivered.Ordering
		if !orderPrefix(n.Delivered.Ordering, input) {
			return invalid("top-K changes input ordering before applying LIMIT")
		}
	}
	if n.Algorithm != Window {
		return nil
	}
	input := p.nodes[n.Children[0]-1].Delivered.Ordering
	if uint32(len(input)) < logical.PartitionCount()+logical.OrderCount() {
		return invalid("window requires partition and frame ordering")
	}
	for i := range logical.PartitionCount() {
		id, _ := g.Partition(n.Logical.Rel, i)
		expr, _ := g.Scalar(id)
		if expr.Kind != ir.Slot || input[i].Key.Slot.ID != expr.Slot || input[i].Key.Slot.Graph != n.Logical.Graph {
			return invalid("window partition expression is not an ordered input slot")
		}
	}
	return logicalOrder(g, n.Logical, input, logical.PartitionCount())
}
func logicalOrder(g *ir.Graph, ref RelRef, physical []OrderKey, offset uint32) error {
	relation, _ := g.Rel(ref.Rel)
	if uint64(len(physical)) < uint64(offset)+uint64(relation.OrderCount()) {
		return invalid("logical ordering not delivered")
	}
	for i := range relation.OrderCount() {
		order, _ := g.Order(ref.Rel, i)
		expr, _ := g.Scalar(order.Expr)
		key := physical[offset+i]
		slotMatches := expr.Kind == ir.Slot && key.Key.Slot.Graph == ref.Graph && key.Key.Slot.ID == expr.Slot
		orderMatches := key.Direction == order.Direction && key.Nulls == order.Nulls && key.Operator == order.Operator && key.Key.Collation == order.Collation
		if !slotMatches || !orderMatches {
			return invalid("physical ordering differs from logical SQL ordering")
		}
	}
	return nil
}
