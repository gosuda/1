package exec

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	exapply "github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func scalarSubqueryID(owner physical.NodeID, port uint32) expr.SubqueryID {
	return expr.SubqueryID(uint64(owner)<<32 | uint64(port+1))
}

func (c *invocation) EvaluateScalar(ctx context.Context, id expr.SubqueryID, captures []model.Value, consume func(expr.SubqueryRow) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, port := physical.NodeID(uint64(id)>>32), uint32(id)
	if owner == 0 || int(owner) >= len(c.prepared.nodes) || port == 0 {
		return ErrInvalid
	}
	node := &c.prepared.nodes[owner]
	if node.domain != c.domain || int(port) > len(node.node.ScalarSubplans) {
		return ErrInvalid
	}
	scalarGraph, _ := c.plan.Graph(node.node.ScalarSubplans[port-1].Scalar.Graph)
	scalar, _ := scalarGraph.Scalar(node.node.ScalarSubplans[port-1].Scalar.Scalar)
	if scalar.SubqueryKind != ir.ScalarSubquery && scalar.SubqueryKind != ir.RowSubquery {
		return ErrInvalid
	}
	root := node.node.ScalarSubplans[port-1].Root
	body := &c.prepared.nodes[root]
	if len(body.layouts) == 0 || len(captures) != len(body.captureSlots) {
		return ErrInvalid
	}
	claim, err := c.portal.env.Query.Allocator(runtime.WorkMemory).Reserve(uint64(unsafe.Sizeof(applyFactory{})) + uint64(unsafe.Sizeof(scalarCacheFactory{})))
	if err != nil {
		return err
	}
	defer claim.Release()
	factory := applyFactory{owner: c, root: root, layouts: body.captureLayouts, columns: body.captureColumns}
	var source exapply.RightFactory = &factory
	if body.queryScalar {
		owner, err := c.scalarOwner(root)
		if err != nil {
			return err
		}
		source = &scalarCacheFactory{owner: owner, id: root}
	}
	env := &c.portal.env
	return exapply.ConsumeScalar(ctx, source, captures, exapply.ScalarOptions{Layouts: body.layouts, Query: env.Query, BatchRows: env.Limits.BatchRows, MaxRowBytes: env.Apply.MaxRowBytes, WorkBytes: env.Apply.WorkBytes}, func(values []model.Value, present bool) error {
		return consume(expr.SubqueryRow{Values: values, Present: present})
	})
}

func (c *invocation) EvaluateRows(ctx context.Context, id expr.SubqueryID, captures []model.Value, visit func(expr.SubqueryRow) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, port := physical.NodeID(uint64(id)>>32), uint32(id)
	if owner == 0 || int(owner) >= len(c.prepared.nodes) || port == 0 {
		return ErrInvalid
	}
	node := &c.prepared.nodes[owner]
	if node.domain != c.domain || int(port) > len(node.node.ScalarSubplans) {
		return ErrInvalid
	}
	subplan := node.node.ScalarSubplans[port-1]
	graph, _ := c.plan.Graph(subplan.Scalar.Graph)
	scalar, _ := graph.Scalar(subplan.Scalar.Scalar)
	if scalar.SubqueryKind == ir.ScalarSubquery || scalar.SubqueryKind == ir.RowSubquery {
		return ErrInvalid
	}
	body := &c.prepared.nodes[subplan.Root]
	if len(captures) != len(body.captureSlots) {
		return ErrInvalid
	}
	claim, err := c.portal.env.Query.Allocator(runtime.WorkMemory).Reserve(uint64(unsafe.Sizeof(applyFactory{})))
	if err != nil {
		return err
	}
	defer claim.Release()
	factory := applyFactory{owner: c, root: subplan.Root, layouts: body.captureLayouts, columns: body.captureColumns}
	env := &c.portal.env
	return exapply.ConsumeRows(ctx, &factory, captures, exapply.RowsOptions{Layouts: body.layouts, Query: env.Query, Exists: scalar.SubqueryKind == ir.ExistsSubquery, MaxRowBytes: env.Apply.MaxRowBytes, WorkBytes: env.Apply.WorkBytes}, func(values []model.Value) (bool, error) {
		return visit(expr.SubqueryRow{Values: values, Present: true})
	})
}
