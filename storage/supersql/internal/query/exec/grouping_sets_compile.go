package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/groupingset"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) groupingSets(node physical.NodeSpec, graph *ir.Graph, relation ir.Rel, input storage.BatchCursor, options aggregate.Options) (storage.BatchCursor, error) {
	prepared := &c.prepared.nodes[c.current]
	if c.preparing {
		count := relation.GroupingSetCount()
		if err := c.charge(uint64(count) * uint64(unsafe.Sizeof([]uint32{}))); err != nil {
			return nil, err
		}
		sets := make([][]uint32, count)
		for i := range sets {
			width, _ := graph.GroupingSetWidth(node.Logical.Rel, uint32(i))
			if err := c.charge(uint64(width) * uint64(unsafe.Sizeof(uint32(0)))); err != nil {
				return nil, err
			}
			sets[i] = make([]uint32, width)
			for j := range sets[i] {
				sets[i][j], _ = graph.GroupingKey(node.Logical.Rel, uint32(i), uint32(j))
			}
		}
		prepared.groupingSets = sets
		if relation.GroupingCount() != 0 {
			if err := c.charge(uint64(relation.GroupingCount()) * uint64(unsafe.Sizeof([]uint32{}))); err != nil {
				return nil, err
			}
			masks := make([][]uint32, relation.GroupingCount())
			for i := range masks {
				call, _ := graph.Grouping(node.Logical.Rel, uint32(i))
				output := node.Output[int(relation.ExpressionCount()+relation.AggregateCount())+i]
				if output.ID != call.Result || output.Graph != node.Logical.Graph {
					return nil, ErrInvalid
				}
				if err := c.charge(uint64(call.ArgumentCount()) * uint64(unsafe.Sizeof(uint32(0)))); err != nil {
					return nil, err
				}
				masks[i] = make([]uint32, call.ArgumentCount())
				for j := range masks[i] {
					masks[i][j], _ = graph.GroupingArgument(node.Logical.Rel, uint32(i), uint32(j))
				}
			}
			prepared.groupingMasks = masks
		}
	}
	if len(prepared.groupingSets) == 0 {
		return nil, ErrInvalid
	}
	options.OrderedGroups = false
	replay := groupingset.Options{Aggregate: options, Sets: prepared.groupingSets, Groupings: prepared.groupingMasks, Spool: c.portal.env.Spool}
	if node.Delivered.Distribution.Kind != physical.SingleNode {
		return c.partitionedGroupingSets(node, input, replay)
	}
	if c.preparing {
		return nil, groupingset.CheckSupport(c.portal.ctx, replay)
	}
	return groupingset.New(c.portal.ctx, input, replay)
}
