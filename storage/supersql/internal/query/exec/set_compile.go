package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exset "github.com/gosuda/supersql-alpha/internal/query/exec/setop"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) setOperation(id physical.NodeID, node physical.NodeSpec, layouts []model.TypeLayout, children []storage.BatchCursor) (storage.BatchCursor, error) {
	options := c.portal.env.Set
	options.Query = c.portal.env.Query
	options.Encoder = c.portal.env.Keys
	options.Layouts = layouts
	options.BatchRows = c.portal.env.Limits.BatchRows
	options.BatchBytes = c.portal.env.Limits.BatchBytes
	options.DistinctColumns = nil
	options.OrderedInput = nil
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	if node.Algorithm == physical.HashSetOp {
		options.Mode = exset.Binary
		options.Kind = relation.SetKind
		options.All = relation.All
		if c.preparing {
			return nil, nil
		}
		return exset.NewBinary(c.portal.ctx, children[0], children[1], options)
	}
	options.Kind = 0
	options.All = false
	options.Mode = exset.Distinct
	if node.Algorithm == physical.StreamDistinct {
		options.Mode = exset.DistinctOn
		count := len(node.Output)
		if relation.DistinctOn {
			count = int(relation.ExpressionCount())
		}
		if err := c.charge(uint64(count) * uint64(unsafe.Sizeof(int(0)))); err != nil {
			return nil, err
		}
		options.DistinctColumns = make([]int, count)
		for i := range options.DistinctColumns {
			column := i
			if relation.DistinctOn {
				scalar, _ := graph.Expression(node.Logical.Rel, uint32(i))
				column = aggregateColumn(graph, scalar, node.Output)
				if column < 0 {
					return nil, &RequiredExecutionError{Node: id, Algorithm: node.Algorithm, Reason: "computed DISTINCT ON key projection"}
				}
			}
			options.DistinctColumns[i] = column
		}
		input, _ := c.plan.Node(node.Children[0])
		ordering := input.Delivered()
		if err := c.charge(uint64(ordering.OrderingCount()) * uint64(unsafe.Sizeof(exset.OrderKey{}))); err != nil {
			return nil, err
		}
		options.OrderedInput = make([]exset.OrderKey, ordering.OrderingCount())
		for i := range options.OrderedInput {
			key, _ := ordering.Ordering(uint32(i))
			column := joinColumn(node.Output, key.Key.Slot)
			if column < 0 {
				return nil, ErrInvalid
			}
			options.OrderedInput[i] = exset.OrderKey{Column: column, Descending: key.Direction == ir.Descending, NullsFirst: key.Nulls == ir.NullsFirst}
		}
	}
	if c.preparing {
		return nil, nil
	}
	return exset.New(c.portal.ctx, children[0], options)
}
