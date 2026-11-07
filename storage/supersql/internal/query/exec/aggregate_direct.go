package exec

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type aggregateDirectEvaluator struct {
	state   *expr.State
	env     *Environment
	builder *columnar.Builder
	batch   columnar.Batch
	row     []model.Value
}

func (e *aggregateDirectEvaluator) Evaluate(ctx context.Context, groups []model.Value, columns []int, out []model.Value) error {
	if e == nil || e.state == nil || e.builder == nil || len(columns) != len(e.row) {
		return ErrInvalid
	}
	for i, column := range columns {
		if column < 0 {
			e.row[i] = model.NullVal(e.row[i].Type)
			continue
		}
		if column >= len(groups) {
			return ErrInvalid
		}
		e.row[i] = groups[column]
	}
	e.builder.Reset()
	if err := e.builder.Append(ctx, e.row); err != nil {
		return err
	}
	e.batch.MaxRows = 1
	if err := e.builder.Publish(&e.batch); err != nil {
		return err
	}
	return e.state.EvalBorrowedRow(ctx, &e.batch, e.env.Parameters, out, expr.Direct)
}

func (e *aggregateDirectEvaluator) Close() error {
	if e == nil {
		return nil
	}
	var err error
	if e.state != nil {
		err = e.state.Close()
		e.state = nil
	}
	if e.builder != nil {
		err = errors.Join(err, e.builder.Close())
		e.builder = nil
	}
	e.batch = columnar.Batch{}
	e.row = nil
	e.env = nil
	return err
}

func (c *invocation) newAggregateDirect(node physical.NodeSpec, graph *ir.Graph, callIndex uint32, call ir.AggregateCall, groupLayouts []model.TypeLayout) ([]model.TypeLayout, exaggregate.DirectEvaluator, error) {
	count := int(call.DirectArgumentCount())
	if count == 0 {
		return nil, nil, nil
	}
	p := c.portal
	metadata := uint64(count)*uint64(unsafe.Sizeof(ir.ScalarID(0))+unsafe.Sizeof(model.TypeLayout{})) + uint64(len(groupLayouts))*uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(aggregateDirectEvaluator{}))
	if err := c.charge(metadata); err != nil {
		return nil, nil, err
	}
	roots := make([]ir.ScalarID, count)
	layouts := make([]model.TypeLayout, count)
	for i := range roots {
		roots[i], _ = graph.AggregateDirectArgument(node.Logical.Rel, callIndex, uint32(i))
		scalar, _ := graph.Scalar(roots[i])
		var err error
		layouts[i], err = c.layout(graph, scalar.Layout, 0)
		if err != nil {
			return nil, nil, err
		}
	}
	program, err := c.programIn(node.Logical.Graph, graph, roots, node.Output[:len(groupLayouts)])
	if err != nil {
		return nil, nil, err
	}
	state, err := c.state(program)
	if err != nil || c.preparing {
		return layouts, nil, err
	}
	evaluator := &aggregateDirectEvaluator{state: state, env: &p.env, row: make([]model.Value, len(groupLayouts)), batch: columnar.Batch{Columns: make([]columnar.Vector, 0, len(groupLayouts))}}
	for i, layout := range groupLayouts {
		evaluator.row[i] = model.NullVal(layout.Type.Storage)
	}
	evaluator.builder, err = columnar.NewBuilder(groupLayouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: p.env.Limits.BatchBytes, Allocator: p.env.Query.Allocator(runtime.WorkMemory)})
	if err != nil {
		return nil, nil, errors.Join(err, evaluator.Close())
	}
	p.owned = append(p.owned, evaluator)
	return layouts, evaluator, nil
}
