package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) aggregate(id physical.NodeID, node physical.NodeSpec, layouts []model.TypeLayout, children []storage.BatchCursor) (storage.BatchCursor, error) {
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	gap := func(reason string) error {
		return &RequiredExecutionError{Node: id, Algorithm: node.Algorithm, Reason: reason}
	}
	groups := int(relation.ExpressionCount())
	width, ok := graph.GroupingSetWidth(node.Logical.Rel, 0)
	if !ok {
		return nil, ErrInvalid
	}
	multipleSets := relation.GroupingSetCount() != 1 || int(width) != groups || relation.GroupingCount() != 0
	if multipleSets && node.Aggregate.Phase != physical.CompleteAggregate {
		return nil, gap("partial aggregation with grouping sets")
	}
	partitioned := node.Delivered.Distribution.Kind != physical.SingleNode
	if multipleSets && node.Algorithm == physical.StreamAggregate {
		return nil, gap("stream ordering for grouping sets")
	}
	if len(layouts) != groups+int(relation.AggregateCount())+int(relation.GroupingCount()) {
		return nil, gap("noncanonical aggregate output projection")
	}
	child, _ := c.plan.Node(node.Children[0])
	if err := c.charge(uint64(child.OutputCount()) * uint64(unsafe.Sizeof(physical.Slot{}))); err != nil {
		return nil, err
	}
	slots := make([]physical.Slot, child.OutputCount())
	for i := range slots {
		slots[i], _ = child.Output(uint32(i))
	}
	inputLayouts, err := c.layouts(slots)
	if err != nil {
		return nil, err
	}
	options := c.portal.env.Aggregate
	switch node.Aggregate.Phase {
	case physical.CompleteAggregate:
		options.Phase = exaggregate.Complete
	case physical.PartialAggregate:
		options.Phase = exaggregate.Partial
	case physical.FinalAggregate:
		options.Phase = exaggregate.Final
	default:
		return nil, ErrInvalid
	}
	options.Query = c.portal.env.Query
	options.Encoder = c.portal.env.Keys
	options.Builtin = c.portal.env.Builtin
	options.InputLayouts = inputLayouts
	options.OutputLayouts = layouts
	options.BatchRows = c.portal.env.Limits.BatchRows
	options.BatchBytes = c.portal.env.Limits.BatchBytes
	options.OrderedGroups = node.Algorithm == physical.StreamAggregate
	if err := c.charge(uint64(groups)*uint64(unsafe.Sizeof(int(0))) + uint64(relation.AggregateCount())*uint64(unsafe.Sizeof(exaggregate.Call{}))); err != nil {
		return nil, err
	}
	options.GroupColumns = make([]int, groups)
	options.DirectGroupCount = groups
	options.OwnDirectEvaluators = true
	if groups != 0 {
		options.DirectGroupColumns = make([]int, groups)
		for i := range options.DirectGroupColumns {
			options.DirectGroupColumns[i] = i
		}
	}
	project := false
	for i := range options.GroupColumns {
		if node.Aggregate.Phase == physical.FinalAggregate {
			if i >= len(slots) || slots[i] != node.Output[i] {
				return nil, gap("final aggregate grouping state projection")
			}
			options.GroupColumns[i] = i
			continue
		}
		scalar, _ := graph.Expression(node.Logical.Rel, uint32(i))
		column := aggregateColumn(graph, scalar, slots)
		matched := false
		for j := range relation.BindingCount() {
			binding, _ := graph.Binding(node.Logical.Rel, j)
			if binding.Slot == node.Output[i].ID && binding.Expr == scalar {
				matched = true
				break
			}
		}
		if !matched || node.Output[i].Graph != node.Logical.Graph {
			return nil, gap("reordered grouping output projection")
		}
		project = project || column < 0
		options.GroupColumns[i] = column
	}
	options.Calls = make([]exaggregate.Call, relation.AggregateCount())
	for i := range options.Calls {
		call, _ := graph.Aggregate(node.Logical.Rel, uint32(i))
		if call.Result != node.Output[groups+i].ID || node.Output[groups+i].Graph != node.Logical.Graph {
			return nil, gap("reordered aggregate output projection")
		}
		if node.Aggregate.Phase == physical.FinalAggregate {
			if groups+i >= len(slots) || slots[groups+i] != node.Output[groups+i] {
				return nil, gap("final aggregate transition state projection")
			}
			if err := c.charge(uint64(unsafe.Sizeof(int(0)))); err != nil {
				return nil, err
			}
			signature, _ := graph.Signature(call.Signature)
			options.Calls[i] = exaggregate.Call{
				Function: call.Function, Combine: node.Aggregate.Combine[i].Combine,
				InputCollation: signature.InputCollation, Arguments: []int{groups + i},
			}
			continue
		}
		bytes := uint64(call.ArgumentCount()) * uint64(unsafe.Sizeof(int(0)))
		bytes += uint64(call.OrderCount()) * uint64(unsafe.Sizeof(exaggregate.OrderKey{}))
		if err := c.charge(bytes); err != nil {
			return nil, err
		}
		directLayouts, directEvaluator, err := c.newAggregateDirect(node, graph, uint32(i), call, layouts[:groups])
		if err != nil {
			return nil, err
		}
		signature, _ := graph.Signature(call.Signature)
		bound := exaggregate.Call{Function: call.Function, InputCollation: signature.InputCollation, Distinct: call.Distinct, Arguments: make([]int, call.ArgumentCount()), DirectLayouts: directLayouts, DirectEvaluator: directEvaluator, OrderBy: make([]exaggregate.OrderKey, call.OrderCount())}
		if node.Aggregate.Phase == physical.PartialAggregate {
			bound.Combine = node.Aggregate.Combine[i].Combine
		}
		for j := range bound.Arguments {
			scalar, _ := graph.AggregateArgument(node.Logical.Rel, uint32(i), uint32(j))
			column := aggregateColumn(graph, scalar, slots)
			project = project || column < 0
			bound.Arguments[j] = column
		}
		for j := range bound.OrderBy {
			order, _ := graph.AggregateOrder(node.Logical.Rel, uint32(i), uint32(j))
			scalar, _ := graph.Scalar(order.Expr)
			typ, _ := graph.Type(scalar.Layout)
			if !canonicalOrderingOperator(c.portal.env.Keys, order.Operator, typ, order.Direction) {
				return nil, gap("aggregate ordering operator lacks canonical SQL key semantics")
			}
			column := aggregateColumn(graph, order.Expr, slots)
			project = project || column < 0
			bound.OrderBy[j] = exaggregate.OrderKey{Column: column, Descending: order.Direction == ir.Descending, NullsFirst: order.Nulls == ir.NullsFirst}
		}
		if call.Filter != 0 {
			column := aggregateColumn(graph, call.Filter, slots)
			project = project || column < 0
			if err := c.charge(uint64(unsafe.Sizeof(column))); err != nil {
				return nil, err
			}
			bound.FilterColumn = &column
		}
		options.Calls[i] = bound
	}
	input := children[0]
	if project {
		input, err = c.aggregateProject(node, graph, slots, input, &options)
		if err != nil {
			return nil, err
		}
		if partitioned && !c.preparing {
			input, err = c.preservePartition(node, children, input)
			if err != nil {
				return nil, err
			}
		}
	}
	if multipleSets {
		return c.groupingSets(node, graph, relation, input, options)
	}
	if partitioned {
		return c.partitionedAggregate(node, input, options)
	}
	if c.preparing {
		return nil, exaggregate.CheckSupport(c.portal.ctx, options)
	}
	return exaggregate.New(c.portal.ctx, input, options)
}

func aggregateColumn(graph *ir.Graph, id ir.ScalarID, slots []physical.Slot) int {
	scalar, ok := graph.Scalar(id)
	if !ok || scalar.Kind != ir.Slot {
		return -1
	}
	for i, slot := range slots {
		if slot.ID == scalar.Slot {
			return i
		}
	}
	return -1
}
