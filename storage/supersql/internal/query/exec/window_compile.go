package exec

import (
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exwindow "github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) window(id physical.NodeID, node physical.NodeSpec, layouts []model.TypeLayout, children []storage.BatchCursor) (storage.BatchCursor, error) {
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	gap := func(reason string) error {
		return &RequiredExecutionError{Node: id, Algorithm: node.Algorithm, Reason: reason}
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
	options := c.portal.env.Window
	options.InputLayouts = inputLayouts
	options.OutputLayouts = layouts
	options.Query = c.portal.env.Query
	options.Encoder = c.portal.env.Keys
	options.Builtin = c.portal.env.Builtin
	options.Aggregates = c.portal.env.Aggregate.Aggregates
	options.BatchRows = c.portal.env.Limits.BatchRows
	options.BatchBytes = c.portal.env.Limits.BatchBytes
	forwarded := len(node.Output) - int(relation.WindowCount())
	if forwarded < 0 {
		return nil, ErrInvalid
	}
	if err := c.charge(uint64(forwarded+int(relation.PartitionCount()))*uint64(unsafe.Sizeof(int(0))) + uint64(relation.OrderCount())*uint64(unsafe.Sizeof(exwindow.OrderKey{})) + uint64(relation.WindowCount())*uint64(unsafe.Sizeof(exwindow.Call{}))); err != nil {
		return nil, err
	}
	options.OutputColumns = make([]int, forwarded)
	for i := range options.OutputColumns {
		column := joinColumn(slots, node.Output[i])
		if column < 0 {
			return nil, gap("computed window output projection")
		}
		options.OutputColumns[i] = column
	}
	options.PartitionColumns = make([]int, relation.PartitionCount())
	for i := range options.PartitionColumns {
		scalar, _ := graph.Partition(node.Logical.Rel, uint32(i))
		column := aggregateColumn(graph, scalar, slots)
		if column < 0 {
			return nil, gap("computed window partition projection")
		}
		options.PartitionColumns[i] = column
	}
	options.OrderBy = make([]exwindow.OrderKey, relation.OrderCount())
	for i := range options.OrderBy {
		order, _ := graph.Order(node.Logical.Rel, uint32(i))
		column := aggregateColumn(graph, order.Expr, slots)
		if column < 0 {
			return nil, gap("computed window order projection")
		}
		options.OrderBy[i] = exwindow.OrderKey{Column: column, Descending: order.Direction == ir.Descending, NullsFirst: order.Nulls == ir.NullsFirst, InputCollation: order.Collation}
	}
	options.Calls = make([]exwindow.Call, relation.WindowCount())
	for i := range options.Calls {
		call, _ := graph.Window(node.Logical.Rel, uint32(i))
		if call.Result != node.Output[forwarded+i].ID {
			return nil, gap("reordered window output projection")
		}
		compiled, err := c.windowCall(node, graph, uint32(i), slots, inputLayouts)
		if err != nil {
			return nil, err
		}
		options.Calls[i] = compiled
	}
	options.Frame = relation.Frame
	options.RangeSupport[0], err = c.windowRangeSupport(graph, relation.Frame.Start)
	if err != nil {
		return nil, err
	}
	options.RangeSupport[1], err = c.windowRangeSupport(graph, relation.Frame.End)
	if err != nil {
		return nil, err
	}
	options.Frame.Start.InRange, options.Frame.End.InRange = 0, 0
	options.StartOffset, err = c.windowOffset(node.Logical.Graph, relation.Frame.Start.Offset)
	if err != nil {
		return nil, err
	}
	options.EndOffset, err = c.windowOffset(node.Logical.Graph, relation.Frame.End.Offset)
	if err != nil {
		return nil, err
	}
	if c.preparing {
		return nil, exwindow.CheckSupport(c.portal.ctx, options)
	}
	return exwindow.New(c.portal.ctx, children[0], options)
}

func (c *invocation) windowCall(node physical.NodeSpec, graph *ir.Graph, index uint32, slots []physical.Slot, layouts []model.TypeLayout) (exwindow.Call, error) {
	call, _ := graph.Window(node.Logical.Rel, index)
	signature, _ := graph.Signature(call.Signature)
	result := exwindow.Call{Function: call.Function, InputCollation: signature.InputCollation}
	if err := c.charge(uint64(call.ArgumentCount()) * uint64(unsafe.Sizeof(int(0)))); err != nil {
		return result, err
	}
	result.Arguments = make([]int, call.ArgumentCount())
	for j := range result.Arguments {
		scalar, _ := graph.WindowArgument(node.Logical.Rel, index, uint32(j))
		column := aggregateColumn(graph, scalar, slots)
		result.Arguments[j] = column
		if column >= 0 {
			continue
		}
		if result.Expressions == nil {
			if err := c.charge(uint64(call.ArgumentCount()) * uint64(unsafe.Sizeof(exwindow.ArgumentExpression(nil)))); err != nil {
				return result, err
			}
			result.Expressions = make([]exwindow.ArgumentExpression, call.ArgumentCount())
		}
		expression, err := c.windowExpression(node, graph, scalar, slots, layouts)
		if err != nil {
			return result, err
		}
		result.Expressions[j] = expression
	}
	if call.Filter != 0 {
		filter, err := c.windowExpression(node, graph, call.Filter, slots, layouts)
		if err != nil {
			return result, err
		}
		result.Filter = filter
	}
	return result, nil
}

func (c *invocation) windowRangeSupport(graph *ir.Graph, bound ir.FrameBound) (exwindow.RangeSupport, error) {
	if bound.InRange == 0 {
		return exwindow.RangeSupport{}, nil
	}
	signature, exists := graph.Signature(bound.InRange)
	if !exists || signature.Function == 0 || signature.ParameterCount() != 5 {
		return exwindow.RangeSupport{}, ErrInvalid
	}
	inputID, inputExists := graph.SignatureParameter(bound.InRange, 0)
	offsetID, offsetExists := graph.SignatureParameter(bound.InRange, 2)
	if !inputExists || !offsetExists {
		return exwindow.RangeSupport{}, ErrInvalid
	}
	input, err := c.layout(graph, inputID, 0)
	if err != nil {
		return exwindow.RangeSupport{}, err
	}
	offset, err := c.layout(graph, offsetID, 0)
	if err != nil {
		return exwindow.RangeSupport{}, err
	}
	return exwindow.RangeSupport{Function: signature.Function, Input: input, Offset: offset, InputCollation: signature.InputCollation}, nil
}

func (c *invocation) windowOffset(graph physical.GraphID, root ir.ScalarID) (result *model.Value, err error) {
	if root == 0 {
		return nil, nil
	}
	state, err := c.compile(graph, []ir.ScalarID{root}, nil)
	if err != nil {
		return nil, err
	}
	if c.preparing {
		value := model.Int64Val(0)
		logical, _ := c.plan.Graph(graph)
		scalar, _ := logical.Scalar(root)
		typ, _ := logical.Type(scalar.Layout)
		switch typ.Storage {
		case model.TypeFloat64:
			value = model.Float64Val(0)
		case model.TypeNumeric:
			value = model.Value{Type: model.TypeNumeric, StrVal: "0"}
		case model.TypeInterval:
			value = model.Value{Type: model.TypeInterval}
		}
		return &value, nil
	}
	defer func() {
		err = errors.Join(err, state.Close())
		if err != nil {
			result = nil
		}
	}()
	input := columnar.Batch{Length: 1}
	var output columnar.Batch
	if err = state.Eval(c.portal.ctx, &input, c.portal.env.Parameters, &output, expr.Direct); err != nil {
		return nil, err
	}
	maximum := c.portal.env.Window.MaxRecordBytes * 8
	bytes, err := columnar.ValueBytes(c.portal.ctx, &output.Columns[0], 0, maximum)
	if err != nil {
		return nil, err
	}
	if err = c.charge(uint64(unsafe.Sizeof(model.Value{})) + bytes); err != nil {
		return nil, err
	}
	value, _, err := columnar.CopyValue(c.portal.ctx, &output.Columns[0], 0, maximum)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
