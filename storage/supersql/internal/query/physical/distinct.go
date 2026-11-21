package physical

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (p *Plan) validateDistinctOrder(ctx context.Context, n NodeSpec, logical ir.Rel) error {
	input := p.nodes[n.Children[0]-1].Delivered
	if !logical.DistinctOn {
		if len(input.Ordering) < len(n.Output) {
			return invalid("stream distinct requires all output keys ordered")
		}
		for _, slot := range n.Output {
			if err := ctx.Err(); err != nil {
				return err
			}
			found := false
			for _, order := range input.Ordering[:len(n.Output)] {
				found = found || order.Key.Slot == slot
			}
			if !found {
				return invalid("stream distinct ordering misses output key")
			}
		}
		return nil
	}
	if input.Distribution.Kind != SingleNode {
		return invalid("global DISTINCT ON requires gathered input")
	}
	if uint32(len(input.Ordering)) < logical.ExpressionCount() {
		return invalid("DISTINCT ON requires ordered distinct keys")
	}
	graph, _ := p.Graph(n.Logical.Graph)
	for i := range logical.ExpressionCount() {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, _ := graph.Expression(n.Logical.Rel, i)
		scalar, _ := graph.Scalar(id)
		key := input.Ordering[i].Key.Slot
		if scalar.Kind != ir.Slot || key.Graph != n.Logical.Graph || key.ID != scalar.Slot {
			return invalid("DISTINCT ON input ordering differs from distinct keys")
		}
	}
	return logicalOrder(graph, n.Logical, input.Ordering, 0)
}
