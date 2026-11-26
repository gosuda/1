package exec

import (
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type preparedAggregateProjection struct {
	program *expr.Program
	layouts []model.TypeLayout
	groups  []int
	calls   []exaggregate.Call
}

func (c *invocation) aggregateProject(node physical.NodeSpec, graph *ir.Graph, slots []physical.Slot, input storage.BatchCursor, options *exaggregate.Options) (storage.BatchCursor, error) {
	p := c.portal
	prepared := &c.prepared.nodes[c.current]
	if cached := prepared.aggregateProjection; cached != nil {
		c.programIndex++
		if len(options.Calls) != len(cached.calls) {
			return nil, ErrInvalid
		}
		for i := range options.Calls {
			options.Calls[i].Arguments = cached.calls[i].Arguments
			options.Calls[i].FilterColumn = cached.calls[i].FilterColumn
			options.Calls[i].OrderBy = cached.calls[i].OrderBy
		}
		options.GroupColumns = cached.groups
		return c.aggregateInput(cached.program, slots, input, options, cached.layouts)
	}
	count := uint64(len(options.GroupColumns))
	filtered := false
	for _, call := range options.Calls {
		count += uint64(len(call.Arguments)) + uint64(len(call.OrderBy))
		if call.FilterColumn != nil {
			count++
			filtered = true
		}
	}
	if count > uint64(p.env.Limits.Expressions.MaxRegisters) {
		return nil, expr.ErrLimit
	}
	bytes := count * (uint64(unsafe.Sizeof(ir.ScalarID(0))) + uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(unsafe.Sizeof(columnar.Vector{})))
	bytes += uint64(len(slots)) * 2 * uint64(unsafe.Sizeof(columnar.Vector{}))
	bytes += uint64(unsafe.Sizeof(expressionCursor{})) + 4096
	if err := c.charge(bytes); err != nil {
		return nil, err
	}
	roots := make([]ir.ScalarID, 0, int(count))
	var extension *ir.Builder
	if filtered {
		// Extension copies all graph arenas. Admit its hard byte ceiling before
		// cloning; the compiled program owns its own copy and separate claim.
		claim, err := p.env.Query.Allocator(runtime.WorkMemory).Reserve(0)
		if err != nil {
			return nil, err
		}
		defer claim.Release()
		handles := uint32(min(p.env.Limits.MetadataBytes/uint64(unsafe.Sizeof(ir.ScalarID(0))), uint64(math.MaxUint32-1)))
		extension, err = ir.Extend(p.ctx, graph, ir.Limits{
			MaxNodes: handles, MaxOperands: handles,
			MaxSlots: handles, MaxDepth: 512,
			MaxBytes: p.env.Limits.MetadataBytes, SourceBytes: math.MaxUint32,
		})
		if err != nil {
			return nil, err
		}
	}
	for i := range options.GroupColumns {
		root, _ := graph.Expression(node.Logical.Rel, uint32(i))
		options.GroupColumns[i] = len(roots)
		roots = append(roots, root)
	}
	for i := range options.Calls {
		call, _ := graph.Aggregate(node.Logical.Rel, uint32(i))
		bound := &options.Calls[i]
		var filter ir.ScalarID
		if call.Filter != 0 {
			scalar, _ := graph.Scalar(call.Filter)
			var err error
			filter, err = extension.AddScalar(p.ctx, ir.ScalarSpec{
				Scalar:   ir.Scalar{Kind: ir.Once, Scope: scalar.Scope, Layout: scalar.Layout, Nullable: scalar.Nullable, Effects: scalar.Effects},
				Operands: []ir.ScalarID{call.Filter},
			})
			if err != nil {
				return nil, err
			}
			*bound.FilterColumn = len(roots)
			roots = append(roots, filter)
		}
		for j := range bound.Arguments {
			root, _ := graph.AggregateArgument(node.Logical.Rel, uint32(i), uint32(j))
			root, err := c.aggregateMaskedRoot(graph, extension, slots, root, filter, call.Filter)
			if err != nil {
				return nil, err
			}
			bound.Arguments[j] = len(roots)
			roots = append(roots, root)
		}
		for j := range bound.OrderBy {
			order, _ := graph.AggregateOrder(node.Logical.Rel, uint32(i), uint32(j))
			column := -1
			for k := range bound.Arguments {
				argument, _ := graph.AggregateArgument(node.Logical.Rel, uint32(i), uint32(k))
				if order.Expr == argument {
					column = bound.Arguments[k]
					break
				}
			}
			for k := 0; column < 0 && k < j; k++ {
				previous, _ := graph.AggregateOrder(node.Logical.Rel, uint32(i), uint32(k))
				if order.Expr == previous.Expr {
					column = bound.OrderBy[k].Column
				}
			}
			if column < 0 {
				root, err := c.aggregateMaskedRoot(graph, extension, slots, order.Expr, filter, call.Filter)
				if err != nil {
					return nil, err
				}
				column = len(roots)
				roots = append(roots, root)
			}
			bound.OrderBy[j].Column = column
		}
	}
	if extension != nil {
		var err error
		graph, err = extension.Build(p.ctx, graph.Root())
		if err != nil {
			return nil, err
		}
	}
	layouts := make([]model.TypeLayout, len(roots))
	for i, root := range roots {
		scalar, _ := graph.Scalar(root)
		var err error
		layouts[i], err = c.layout(graph, scalar.Layout, 0)
		if err != nil {
			return nil, err
		}
	}
	program, err := c.programIn(node.Logical.Graph, graph, roots, slots)
	if err != nil {
		return nil, err
	}
	prepared.aggregateProjection = &preparedAggregateProjection{program: program, layouts: layouts, groups: options.GroupColumns, calls: options.Calls}
	return c.aggregateInput(program, slots, input, options, layouts)
}

func (c *invocation) aggregateInput(program *expr.Program, slots []physical.Slot, input storage.BatchCursor, options *exaggregate.Options, layouts []model.TypeLayout) (storage.BatchCursor, error) {
	p := c.portal
	state, err := c.state(program)
	if err != nil {
		return nil, err
	}
	projection := &expressionCursor{
		input: input, state: state, env: &p.env, layouts: options.InputLayouts,
		source:    columnar.Batch{Columns: make([]columnar.Vector, 0, len(slots))},
		evaluated: columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))},
		compact:   columnar.Batch{Columns: make([]columnar.Vector, 0, len(slots))},
	}
	if !c.preparing {
		p.owned = append(p.owned, projection)
	}
	options.InputLayouts = layouts
	return projection, nil
}

func (c *invocation) aggregateMaskedRoot(graph *ir.Graph, extension *ir.Builder, slots []physical.Slot, root, filter, predicate ir.ScalarID) (ir.ScalarID, error) {
	if filter == 0 || aggregateColumn(graph, root, slots) >= 0 {
		return root, nil
	}
	scalar, _ := graph.Scalar(root)
	typ, _ := graph.Type(scalar.Layout)
	null, err := extension.AddScalar(c.portal.ctx, ir.ScalarSpec{
		Scalar: ir.Scalar{Kind: ir.Const, Scope: scalar.Scope, Layout: scalar.Layout, Nullable: true},
		Value:  model.NullVal(typ.Storage),
	})
	if err != nil {
		return 0, err
	}
	condition, _ := graph.Scalar(predicate)
	return extension.AddScalar(c.portal.ctx, ir.ScalarSpec{
		Scalar:   ir.Scalar{Kind: ir.Case, Scope: scalar.Scope, Layout: scalar.Layout, Nullable: true, Effects: scalar.Effects.Union(condition.Effects)},
		Operands: []ir.ScalarID{filter, root, null},
	})
}
