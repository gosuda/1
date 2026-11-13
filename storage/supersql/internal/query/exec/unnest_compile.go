package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	exunnest "github.com/gosuda/supersql-alpha/internal/query/exec/unnest"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) unnest(node physical.NodeSpec, layouts []model.TypeLayout) (storage.BatchCursor, error) {
	graph, ok := c.plan.Graph(node.Logical.Graph)
	if !ok {
		return nil, ErrInvalid
	}
	relation, ok := graph.Rel(node.Logical.Rel)
	if !ok {
		return nil, ErrInvalid
	}
	if relation.RowFunction != 0 || relation.JSONTableRoot != 0 {
		return c.rowSource(node, layouts, graph, relation)
	}
	if relation.ExpressionCount() != 1 {
		return nil, ErrInvalid
	}
	root, _ := graph.Expression(node.Logical.Rel, 0)
	scalar, ok := graph.Scalar(root)
	if !ok {
		return nil, ErrInvalid
	}
	var element model.TypeLayout
	var multirange model.TypeID
	if relation.UnnestRange != (model.Type{}) {
		argument, ok := graph.Type(scalar.Layout)
		family, known := builtin.LookupRangeFamily(argument.ID)
		validInput := ok && known && argument.ID == family.Multirange && argument.Storage == model.TypeBytes
		validOutput := relation.UnnestRange.ID == family.Range && relation.UnnestRange.Storage == model.TypeBytes
		if !validInput || !validOutput || graph.LayoutChildCount(scalar.Layout) != 0 {
			return nil, ErrInvalid
		}
		multirange = argument.ID
		element = model.TypeLayout{Type: relation.UnnestRange}
	} else {
		if graph.LayoutChildCount(scalar.Layout) != 1 {
			return nil, ErrInvalid
		}
		child, _ := graph.LayoutChild(scalar.Layout, 0)
		var err error
		element, err = c.layout(graph, child, 0)
		if err != nil {
			return nil, err
		}
	}
	if err := c.charge(uint64(unsafe.Sizeof(ir.ScalarID(0)))); err != nil {
		return nil, err
	}
	program, err := c.program(node.Logical.Graph, []ir.ScalarID{root}, nil)
	if err != nil || c.preparing {
		return nil, err
	}
	env := &c.portal.env
	return exunnest.New(c.portal.ctx, exunnest.Options{
		Arguments: program, Element: element, Multirange: multirange, Output: layouts, Ordinality: relation.UnnestOrdinality,
		Parameters: env.Parameters, Captures: c.captures, Query: env.Query,
		Subqueries: c,
		Sequences:  env.Sequences,
		Relations:  env.Relations,
		Temporal:   env.Temporal,
		Settings:   env.Settings,
		TextSearch: env.TextSearch,
		Advisory:   env.Advisory,
		Native:     env.Native,
		BatchRows:  env.Limits.BatchRows, BatchBytes: env.Limits.BatchBytes,
	})
}
