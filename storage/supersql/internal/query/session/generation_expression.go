package session

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
)

type GenerationExpressionOptions struct {
	Columns ColumnExpressionOptions
	Indexes IndexExpressionOptions
}

func (options GenerationExpressionOptions) ForSnapshot(ctx context.Context, snapshot *catalog.CatalogSnapshot, _ schema.JobDefinition) (schema.GenerationExpressions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	columns, err := NewColumnExpressionCompiler(snapshot, options.Columns)
	if err != nil {
		return nil, err
	}
	indexes, err := NewIndexExpressionCompiler(snapshot, options.Indexes)
	if err != nil {
		return nil, err
	}
	return &GenerationExpressionCompiler{columns: columns, transforms: indexes}, nil
}

// GenerationExpressionCompiler binds every program against the same immutable
// catalog snapshot. Returned programs own their metadata and retain no snapshot.
type GenerationExpressionCompiler struct {
	columns    *ColumnExpressionCompiler
	transforms *IndexExpressionCompiler
}

func (c *GenerationExpressionCompiler) CompileColumnDefault(ctx context.Context, definition *model.ExpressionDefinition, result model.TypeLayout) (txn.ValueExpression, error) {
	return c.columns.CompileColumnDefault(ctx, definition, result)
}

func (c *GenerationExpressionCompiler) CompileStoredGenerated(ctx context.Context, definition *model.ExpressionDefinition, column model.ColumnID, columns []storage.ColumnLayout, result model.TypeLayout) (txn.ValueExpression, error) {
	return c.columns.CompileStoredGenerated(ctx, definition, column, columns, result)
}

func (c *GenerationExpressionCompiler) CompileCheck(ctx context.Context, definition *model.ExpressionDefinition, constraint model.ObjectID, columns []storage.ColumnLayout) (txn.ValueExpression, error) {
	return c.columns.CompileCheck(ctx, definition, constraint, columns)
}

func (c *GenerationExpressionCompiler) CompileIdentityDefault(ctx context.Context, spec schema.IdentityDefaultSpec) (txn.ValueExpression, error) {
	return c.columns.CompileIdentityDefault(ctx, spec)
}

func (c *GenerationExpressionCompiler) CompileGenerationTransform(ctx context.Context, definition *model.ExpressionDefinition, target model.ColumnID, columns []storage.ColumnLayout, result model.TypeLayout) (txn.ValueExpression, error) {
	binding, err := bind.BindGenerationTransform(ctx, definition, target, columns, result, c.transforms.snapshot, c.transforms.options.Binding)
	if err != nil {
		return nil, err
	}
	program, parameters, err := c.transforms.compile(ctx, binding)
	if err != nil {
		return nil, err
	}
	positions := make([]int, len(parameters))
	for i, parameter := range binding.Parameters {
		positions[i] = -1
		for j, column := range columns {
			if column.ID == parameter.Column {
				positions[i] = j
				break
			}
		}
		if positions[i] < 0 {
			return nil, &model.Error{Code: "42703", Message: "generation transform column no longer exists"}
		}
	}
	return &indexValueExpression{program: program, parameters: parameters, positions: positions, width: len(columns), inputBytes: c.transforms.options.InputBytes, resultBytes: c.transforms.options.ResultBytes}, nil
}
