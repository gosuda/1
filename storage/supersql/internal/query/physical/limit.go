package physical

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (p *Plan) validateLimit(ctx context.Context, n NodeSpec) error {
	l := n.Limit
	if l.Scope != GlobalLimit && l.Scope != PartitionLimit {
		return invalid("limit scope missing")
	}
	if l.Count > math.MaxUint64-l.Offset {
		return invalid("limit offset/count overflow")
	}
	if l.WithTies && (n.Algorithm != TopK || len(n.Delivered.Ordering) == 0) {
		return invalid("WITH TIES requires an ordered top-K")
	}
	if l.Scope == PartitionLimit {
		if n.Algorithm != TopK || l.Global == 0 || l.Offset != 0 || l.WithTies {
			return invalid("partition cap requires a global top-K consumer and zero offset")
		}
		return nil
	}
	if l.Global != 0 || n.Delivered.Distribution.Kind != SingleNode {
		return invalid("global limit requires one merged stream")
	}
	g, logical, err := p.relation(n.Logical)
	if err != nil {
		return err
	}
	if logical.WithTies != l.WithTies {
		return invalid("limit tie semantics differ from logical relation")
	}
	if l.RuntimeBounds {
		if l.Offset != 0 || l.Count != 0 {
			return invalid("runtime bounds cannot carry static limit estimates")
		}
		return nil
	}
	offset, ok := constantCount(ctx, g, logical.Offset, true)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ok || offset != l.Offset {
		return invalid("physical offset requires the matching bound integer constant")
	}
	count, ok := constantCount(ctx, g, logical.Count, false)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ok || count != l.Count {
		return invalid("physical limit requires the matching bound integer constant")
	}
	return nil
}

func constantCount(ctx context.Context, g *ir.Graph, id ir.ScalarID, absentZero bool) (uint64, bool) {
	if id == 0 {
		return 0, absentZero
	}
	scalar, ok := g.Scalar(id)
	if !ok || scalar.Kind != ir.Const {
		return 0, false
	}
	// IR constants are immutable; use its bounded copy API rather than retaining
	// a borrowed datum. Count values cannot have variable-width payloads.
	value, err := g.CloneConstant(ctx, id, 1)
	if err != nil || value.IsNull || value.Type != model.TypeInt64 || value.IntVal < 0 {
		return 0, false
	}
	return uint64(value.IntVal), true
}

func (p *Plan) validateGlobalLimit(ctx context.Context, id NodeID, consumers []uint32) error {
	n := p.nodes[id-1]
	owner := NodeID(0)
	if n.Limit.Scope == PartitionLimit {
		owner = n.Limit.Global
	}
	if n.Scan.AppliedLimit.Enabled {
		owner = n.Scan.LimitOwner
	}
	if owner == 0 {
		return nil
	}
	if owner <= id || uint64(owner) > uint64(len(p.nodes)) {
		return invalid("limit owner is not an ancestor")
	}
	global := p.nodes[owner-1]
	if global.Limit.RuntimeBounds {
		return invalid("runtime limit cannot justify compile-time pruning")
	}
	limitAlgorithm := global.Algorithm == TopK || global.Algorithm == Limit
	if global.Limit.Scope != GlobalLimit || !limitAlgorithm || global.Limit.WithTies {
		return invalid("local limit lacks a compatible global consumer")
	}
	if n.Limit.Scope == PartitionLimit {
		if global.Algorithm != TopK || n.Limit.Count != global.Limit.Offset+global.Limit.Count || !orderPrefix(n.Delivered.Ordering, global.Delivered.Ordering) || len(n.Delivered.Ordering) != len(global.Delivered.Ordering) {
			return invalid("partition top-K does not preserve global top-K")
		}
	} else {
		if n.Scan.AppliedLimit.Count != global.Limit.Offset+global.Limit.Count {
			return invalid("scan cap differs from global demand")
		}
		if global.Algorithm == TopK && !orderPrefix(n.Delivered.Ordering, global.Delivered.Ordering) {
			return invalid("unordered scan cannot apply ordered global limit")
		}
	}
	current := owner
	for current != id {
		if err := ctx.Err(); err != nil {
			return err
		}
		parent := p.nodes[current-1]
		if len(parent.Children) != 1 {
			return invalid("local limit crosses a row-multiplying operator")
		}
		child := parent.Children[0]
		if child != id && p.nodes[child-1].Algorithm != LocalGather && p.nodes[child-1].Algorithm != LocalOrderedMerge {
			return invalid("local limit crosses a row-removing or reordering operator")
		}
		if child < id {
			return invalid("limit owner is not an ancestor")
		}
		if consumers[child-1] != 1 {
			return invalid("locally capped input is shared outside its global limit")
		}
		current = child
	}
	return nil
}
