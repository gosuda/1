package bind

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const storedGeneratedVersion = 1
const checkExpressionVersion = 1

// BoundColumnExpression owns its graph and private row-parameter layouts.
type BoundColumnExpression struct {
	Graph        *ir.Graph
	Root         ir.ScalarID
	Parameters   []IndexExpressionParameter
	Result       model.TypeLayout
	Builtin      builtin.ResolveOptions
	Dependencies []model.Dependency
}

// BindColumnDefault imports frozen scalar IR; invocation settings are explicit.
func BindColumnDefault(ctx context.Context, definition *model.ExpressionDefinition, target model.TypeLayout, snapshot CatalogSnapshot, options Options) (*BoundColumnExpression, error) {
	b, err := newExpressionBinder(ctx, snapshot, options)
	if err != nil {
		return nil, err
	}
	s, err := b.newScope(nil)
	if err != nil {
		return nil, err
	}
	e, err := b.routineDefault(definition, target, s, 0)
	if err != nil {
		return nil, err
	}
	e, err = b.coerceColumnAssignment(e, s, model.ColumnDescriptor{Layout: target})
	if err != nil {
		return nil, err
	}
	slot, err := b.slot(s, e.typ, e.nullable, 0, 0)
	if err != nil {
		return nil, err
	}
	rel, err := b.builder.AddRel(ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: s.id, Rows: 1, Effects: e.effects}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{e.id}})
	if err != nil {
		return nil, err
	}
	graph, err := b.builder.Build(ctx, rel)
	if err != nil {
		return nil, err
	}
	return &BoundColumnExpression{Graph: graph, Root: e.id, Result: e.typ, Builtin: options.Builtin, Dependencies: b.dependencies}, nil
}

// DefineStoredGenerated borrows a pending declaration, not a published table.
func DefineStoredGenerated(ctx context.Context, ast *zpgparse.Node, table *model.Descriptor, column model.ColumnID, target model.TypeLayout, snapshot CatalogSnapshot, options Options) (*model.ExpressionDefinition, error) {
	b, err := newExpressionBinder(ctx, snapshot, options)
	if err != nil {
		return nil, err
	}
	if table == nil || table.Kind() != model.ObjectTable {
		return nil, bindError("42809", "generated expression requires a table declaration", 0)
	}
	spec, err := b.descriptorSpec(table)
	if err != nil {
		return nil, err
	}
	if spec.Table == nil {
		return nil, bindError("42809", "generated expression requires table metadata", 0)
	}
	found := false
	for _, declared := range spec.Table.Columns {
		if declared.ID == column && column != 0 && sameType(declared.Layout, target) {
			found = true
			break
		}
	}
	if !found {
		return nil, bindError("42804", "generated target column identity or type differs from declaration", 0)
	}
	definition, _, err := defineRowExpression(ctx, ast, table, spec.Table.Columns, snapshot, options, rowStoredGenerated, column, &target, 0)
	return definition, err
}

func BindStoredGenerated(ctx context.Context, definition *model.ExpressionDefinition, column model.ColumnID, columns []storage.ColumnLayout, target model.TypeLayout, snapshot CatalogSnapshot, options Options) (*BoundColumnExpression, error) {
	bound, err := bindRowExpression(ctx, definition, column, columns, target, snapshot, options.Limits, rowStoredGenerated, 0)
	if err != nil {
		return nil, err
	}
	return &BoundColumnExpression{Graph: bound.Graph, Root: bound.Root, Parameters: bound.Parameters, Result: bound.Result, Builtin: bound.Builtin, Dependencies: bound.Dependencies}, nil
}

// DefineCheck freezes a boolean expression against an unpublished table declaration.
func DefineCheck(ctx context.Context, ast *zpgparse.Node, table *model.Descriptor, constraint model.ObjectID, snapshot CatalogSnapshot, options Options) (*model.ExpressionDefinition, error) {
	b, err := newExpressionBinder(ctx, snapshot, options)
	if err != nil {
		return nil, err
	}
	if table == nil || table.Kind() != model.ObjectTable || constraint == 0 {
		return nil, bindError("22023", "CHECK requires a table declaration and constraint identity", 0)
	}
	spec, err := b.descriptorSpec(table)
	if err != nil {
		return nil, err
	}
	if spec.Table == nil {
		return nil, bindError("42809", "CHECK requires table metadata", 0)
	}
	definition, _, err := defineRowExpression(ctx, ast, table, spec.Table.Columns, snapshot, options, rowCheck, 0, nil, constraint)
	return definition, err
}

func BindCheck(ctx context.Context, definition *model.ExpressionDefinition, constraint model.ObjectID, columns []storage.ColumnLayout, snapshot CatalogSnapshot, options Options) (*BoundColumnExpression, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if constraint == 0 {
		return nil, bindError("22023", "CHECK requires a constraint identity", 0)
	}
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	bound, err := bindRowExpression(ctx, definition, 0, columns, boolean, snapshot, options.Limits, rowCheck, constraint)
	if err != nil {
		return nil, err
	}
	return &BoundColumnExpression{Graph: bound.Graph, Root: bound.Root, Parameters: bound.Parameters, Result: bound.Result, Builtin: bound.Builtin, Dependencies: bound.Dependencies}, nil
}

func (b *binder) importColumnExpression(bound *BoundColumnExpression, inputs map[model.ColumnID]column, scope *scope, position int) (expression, error) {
	if bound == nil || bound.Graph == nil || bound.Root == 0 || scope == nil {
		return expression{}, model.ErrInvalidDescriptor
	}
	graph := bound.Graph
	layouts := make([]ir.LayoutID, graph.LayoutCount())
	layoutID := func(source ir.LayoutID) (ir.LayoutID, error) {
		if source == 0 || uint32(source) > graph.LayoutCount() {
			return 0, model.ErrInvalidDescriptor
		}
		if layouts[source-1] != 0 {
			return layouts[source-1], nil
		}
		layout, err := graph.CloneLayout(b.ctx, source, b.options.Limits.MaxBytes)
		if err != nil {
			return 0, err
		}
		layouts[source-1], err = b.layout(layout)
		return layouts[source-1], err
	}
	signatures := make([]ir.SignatureID, graph.SignatureCount())
	for i := range signatures {
		sourceID := ir.SignatureID(i + 1)
		source, ok := graph.Signature(sourceID)
		if !ok {
			return expression{}, model.ErrInvalidDescriptor
		}
		result, err := layoutID(source.Result)
		if err != nil {
			return expression{}, err
		}
		parameters := make([]ir.LayoutID, source.ParameterCount())
		for parameter := range parameters {
			id, ok := graph.SignatureParameter(sourceID, uint32(parameter))
			if !ok {
				return expression{}, model.ErrInvalidDescriptor
			}
			parameters[parameter], err = layoutID(id)
			if err != nil {
				return expression{}, err
			}
		}
		source.Result = result
		signatures[i], err = b.builder.AddSignature(b.ctx, ir.SignatureSpec{Signature: source.Metadata(), Parameters: parameters})
		if err != nil {
			return expression{}, err
		}
	}
	scalars := make([]ir.ScalarID, graph.ScalarCount())
	for i := range scalars {
		sourceID := ir.ScalarID(i + 1)
		spec, err := graph.CloneScalarSpec(b.ctx, sourceID, b.options.Limits.MaxBytes)
		if err != nil {
			return expression{}, err
		}
		if spec.Kind == ir.Param {
			if spec.Parameter == 0 || int(spec.Parameter) > len(bound.Parameters) || len(spec.Operands) != 0 || len(spec.Comparisons) != 0 {
				return expression{}, model.ErrInvalidDescriptor
			}
			parameter := bound.Parameters[spec.Parameter-1]
			input, ok := inputs[parameter.Column]
			if !ok || !sameType(input.Layout, parameter.Layout) {
				return expression{}, bindError("42703", "generated expression input column is unavailable", position)
			}
			reference, err := b.reference(scope, input, position)
			if err != nil {
				return expression{}, err
			}
			scalars[i] = reference.id
			continue
		}
		if spec.Kind == ir.Slot || spec.Kind == ir.Subquery || spec.Relation != 0 || spec.Slot != 0 || spec.Parameter != 0 {
			return expression{}, model.ErrInvalidDescriptor
		}
		spec.Scope = scope.id
		spec.Span = sourceSpan(position)
		spec.Layout, err = layoutID(spec.Layout)
		if err != nil {
			return expression{}, err
		}
		for operand, source := range spec.Operands {
			if source == 0 || int(source) > i {
				return expression{}, model.ErrInvalidDescriptor
			}
			spec.Operands[operand] = scalars[source-1]
		}
		if spec.Signature != 0 {
			if int(spec.Signature) > len(signatures) {
				return expression{}, model.ErrInvalidDescriptor
			}
			spec.Signature = signatures[spec.Signature-1]
		}
		for comparison, source := range spec.Comparisons {
			if source == 0 || int(source) > len(signatures) {
				return expression{}, model.ErrInvalidDescriptor
			}
			spec.Comparisons[comparison] = signatures[source-1]
		}
		scalars[i], err = b.builder.AddScalar(b.ctx, spec)
		if err != nil {
			return expression{}, err
		}
	}
	if uint32(bound.Root) > uint32(len(scalars)) {
		return expression{}, model.ErrInvalidDescriptor
	}
	root := scalars[bound.Root-1]
	metadata, ok := b.builder.Scalar(root)
	if !ok {
		return expression{}, model.ErrInvalidDescriptor
	}
	return expression{id: root, typ: bound.Result, nullable: metadata.Nullable, effects: metadata.Effects, position: position}, nil
}
