package exec

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type windowExpression struct {
	state    *expr.State
	builder  *columnar.Builder
	env      *Environment
	layout   model.TypeLayout
	input    columnar.Batch
	output   columnar.Batch
	closed   bool
	closeErr error
}

func (c *invocation) windowExpression(node physical.NodeSpec, graph *ir.Graph, root ir.ScalarID, slots []physical.Slot, layouts []model.TypeLayout) (*windowExpression, error) {
	p := c.portal
	if err := c.charge(uint64(unsafe.Sizeof(windowExpression{})) + uint64(len(slots)+1)*uint64(unsafe.Sizeof(columnar.Vector{}))); err != nil {
		return nil, err
	}
	scalar, _ := graph.Scalar(root)
	layout, err := c.layout(graph, scalar.Layout, 0)
	if err != nil {
		return nil, err
	}
	program, err := c.programIn(node.Logical.Graph, graph, []ir.ScalarID{root}, slots)
	if err != nil {
		return nil, err
	}
	state, err := c.state(program)
	if err != nil {
		return nil, err
	}
	value := &windowExpression{state: state, env: &p.env, layout: layout,
		input:  columnar.Batch{Columns: make([]columnar.Vector, 0, len(slots))},
		output: columnar.Batch{Columns: make([]columnar.Vector, 0, 1)},
	}
	if c.preparing {
		return value, nil
	}
	p.owned = append(p.owned, value)
	value.builder, err = columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: p.env.Limits.BatchBytes, Allocator: p.env.Query.Allocator(runtime.WorkMemory)})
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (e *windowExpression) Layout() model.TypeLayout { return e.layout }

func (e *windowExpression) Eval(ctx context.Context, row []model.Value) (model.Value, error) {
	if e.closed {
		return model.Value{}, ErrInvalid
	}
	e.input.Reset()
	e.output.Reset()
	e.builder.Reset()
	if err := e.builder.Append(ctx, row); err != nil {
		return model.Value{}, err
	}
	if err := e.builder.Publish(&e.input); err != nil {
		return model.Value{}, err
	}
	if err := e.state.Eval(ctx, &e.input, e.env.Parameters, &e.output, expr.Direct); err != nil {
		return model.Value{}, err
	}
	value, _, err := columnar.CopyValue(ctx, &e.output.Columns[0], 0, e.env.Window.MaxRecordBytes*8)
	return value, err
}

func (e *windowExpression) Close() error {
	if e.closed {
		return e.closeErr
	}
	e.closed = true
	e.input.Reset()
	e.output.Reset()
	e.closeErr = e.state.Close()
	e.state = nil
	if e.builder != nil {
		e.closeErr = errors.Join(e.closeErr, e.builder.Close())
		e.builder = nil
	}
	return e.closeErr
}
