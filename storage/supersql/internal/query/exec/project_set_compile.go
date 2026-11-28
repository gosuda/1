package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/projectset"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) projectSet(node physical.NodeSpec, layouts []model.TypeLayout, input []physical.Slot, child storage.BatchCursor) (storage.BatchCursor, error) {
	graph, ok := c.plan.Graph(node.Logical.Graph)
	if !ok || (!c.preparing && child == nil) {
		return nil, ErrInvalid
	}
	relation, ok := graph.Rel(node.Logical.Rel)
	if !ok || relation.Kind != ir.ProjectSet || relation.BindingCount() != uint32(len(node.Output)) {
		return nil, ErrInvalid
	}
	if err := c.charge(uint64(len(node.Output))*uint64(unsafe.Sizeof(ir.ScalarID(0))) + uint64(len(node.Output))*uint64(unsafe.Sizeof(int(0)))); err != nil {
		return nil, err
	}
	roots, err := projectionRoots(graph, node)
	if err != nil {
		return nil, err
	}
	passthroughRoots := make([]ir.ScalarID, 0, len(roots))
	passthroughOutputs := make([]int, 0, len(roots))
	if err := c.charge(uint64(len(roots)) * uint64(unsafe.Sizeof(projectset.Source{}))); err != nil {
		return nil, err
	}
	sources := make([]projectset.Source, 0, len(roots))
	for output, root := range roots {
		scalar, ok := graph.Scalar(root)
		if !ok {
			return nil, ErrInvalid
		}
		if scalar.Kind == ir.Slot {
			passthroughRoots = append(passthroughRoots, root)
			passthroughOutputs = append(passthroughOutputs, output)
			continue
		}
		if scalar.Kind != ir.Call {
			return nil, ErrInvalid
		}
		signature, ok := graph.Signature(scalar.Signature)
		if !ok || !signature.SetReturning || !graph.SameLayout(signature.Result, scalar.Layout) {
			return nil, ErrInvalid
		}
		argumentRoots := make([]ir.ScalarID, scalar.OperandCount())
		argumentLayouts := make([]model.TypeLayout, scalar.OperandCount())
		if err := c.charge(uint64(len(argumentRoots)) * (uint64(unsafe.Sizeof(ir.ScalarID(0))) + uint64(unsafe.Sizeof(model.TypeLayout{})))); err != nil {
			return nil, err
		}
		for i := range scalar.OperandCount() {
			argumentRoots[i], ok = graph.Operand(root, i)
			if !ok {
				return nil, ErrInvalid
			}
			argument, ok := graph.Scalar(argumentRoots[i])
			if !ok {
				return nil, ErrInvalid
			}
			argumentLayouts[i], err = c.layout(graph, argument.Layout, 0)
			if err != nil {
				return nil, err
			}
		}
		result, err := c.layout(graph, scalar.Layout, 0)
		if err != nil {
			return nil, err
		}
		maximum := c.portal.env.Limits.MetadataBytes - c.used
		rows, err := c.resolveRowSource(scalar.Function, argumentLayouts, result, signature.InputCollation, signature.Strict, signature.VariadicArray, maximum)
		if err != nil {
			return nil, err
		}
		if err := c.charge(rows.OwnedBytes()); err != nil {
			return nil, err
		}
		var argumentsProgram = (*expr.Program)(nil)
		if len(argumentRoots) != 0 {
			argumentsProgram, err = c.program(node.Logical.Graph, argumentRoots, input)
			if err != nil {
				return nil, err
			}
		}
		sources = append(sources, projectset.Source{Arguments: argumentsProgram, Rows: rows, Result: result, Output: output, Strict: signature.Strict})
	}
	var passthrough *expr.Program
	if len(passthroughRoots) != 0 {
		passthrough, err = c.program(node.Logical.Graph, passthroughRoots, input)
		if err != nil {
			return nil, err
		}
	}
	if len(sources) == 0 {
		return nil, ErrInvalid
	}
	if c.preparing {
		return nil, nil
	}
	env := &c.portal.env
	return projectset.New(c.portal.ctx, projectset.Options{
		Input: child, Passthrough: passthrough, PassthroughOutputs: passthroughOutputs, Sources: sources, Output: layouts,
		Parameters: env.Parameters, Captures: c.captures, Subqueries: c, Sequences: env.Sequences, Relations: env.Relations,
		Temporal: env.Temporal, Settings: env.Settings, TextSearch: env.TextSearch, Advisory: env.Advisory, Native: env.Native, SessionIntrospection: env.SessionIntrospection,
		CatalogMetadata: catalogMetadataRuntime(env.Catalog), Query: env.Query,
		BatchRows: env.Limits.BatchRows, BatchBytes: env.Limits.BatchBytes,
	})
}
