package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exseries "github.com/gosuda/supersql-alpha/internal/query/exec/series"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) series(node physical.NodeSpec, layouts []model.TypeLayout) (storage.BatchCursor, error) {
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	count := relation.ExpressionCount()
	invalidLayouts := len(layouts) < 1 || len(layouts) > 2
	invalidArguments := count < 2 || count > 4
	if invalidLayouts || invalidArguments {
		return nil, ErrInvalid
	}
	if count == 4 {
		explicitZone := node.Algorithm == physical.GenerateSeries && layouts[0].Type.ID == 1184
		if !explicitZone {
			return nil, ErrInvalid
		}
	}
	if err := c.charge(uint64(count) * uint64(unsafe.Sizeof(ir.ScalarID(0)))); err != nil {
		return nil, err
	}
	roots := make([]ir.ScalarID, count)
	for i := range roots {
		roots[i], _ = graph.Expression(node.Logical.Rel, uint32(i))
	}
	program, err := c.program(node.Logical.Graph, roots, nil)
	if err != nil || c.preparing {
		return nil, err
	}
	env := &c.portal.env
	options := exseries.Options{
		Arguments: program, Layout: layouts[0], Ordinality: len(layouts) == 2,
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
		WorkBytes: env.Limits.Expressions.MaxStateBytes, TimeZone: env.Builtin.TimeZone,
	}
	switch node.Algorithm {
	case physical.GenerateSeries:
		return exseries.New(c.portal.ctx, options)
	case physical.GenerateSubscripts:
		return exseries.NewSubscripts(c.portal.ctx, options)
	default:
		return nil, ErrInvalid
	}
}
