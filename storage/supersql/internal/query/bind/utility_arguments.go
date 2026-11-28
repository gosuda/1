package bind

import (
	"context"
	"fmt"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// ResolveParameterTypes resolves PREPARE's declared type names against one
// catalog snapshot. Returned layouts are owned by the caller.
func ResolveParameterTypes(ctx context.Context, names []*zpgparse.Node, snapshot CatalogSnapshot, options Options) ([]model.TypeLayout, error) {
	if uint64(len(names)) > uint64(options.Limits.MaxOperands) {
		return nil, bindError("54000", "prepared parameter limit exceeded", 0)
	}
	b, err := newExpressionBinder(ctx, snapshot, options)
	if err != nil {
		return nil, err
	}
	layouts := make([]model.TypeLayout, len(names))
	remaining := options.Limits.MaxBytes
	for i, node := range names {
		if node == nil || node.GetTypeName() == nil {
			return nil, bindError("42601", "invalid prepared parameter type", 0)
		}
		layout, err := b.typeName(node.GetTypeName())
		if err != nil {
			return nil, err
		}
		layout, used, err := model.CopyTypeLayout(ctx, layout, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= used
		layouts[i] = layout
	}
	return layouts, nil
}

// BindUtilityArguments binds EXECUTE arguments to the prepared parameter
// layouts. The result is an immutable one-row plan and contains no SQL text.
func BindUtilityArguments(ctx context.Context, arguments []*zpgparse.Node, targets []model.TypeLayout, snapshot CatalogSnapshot, options Options) (*BoundStatement, error) {
	if len(arguments) != len(targets) {
		return nil, bindError("42601", fmt.Sprintf("wrong number of parameters for prepared statement: expected %d, got %d", len(targets), len(arguments)), 0)
	}
	if uint64(len(arguments)) > uint64(options.Limits.MaxOperands) {
		return nil, bindError("54000", "prepared argument limit exceeded", 0)
	}
	b, err := newExpressionBinder(ctx, snapshot, options)
	if err != nil {
		return nil, err
	}
	scope, err := b.newScope(nil)
	if err != nil {
		return nil, err
	}
	scope.allowAggregate, scope.allowWindow = false, false
	outputs := make([]model.SlotID, len(arguments))
	expressions := make([]ir.ScalarID, len(arguments))
	columns := make([]ResultColumn, len(arguments))
	var effects ir.Effects
	remaining, payload := options.Limits.MaxNodes, options.Limits.MaxBytes
	for i, argument := range arguments {
		if argument == nil {
			return nil, bindError("42601", "missing prepared statement argument", 0)
		}
		if err := b.checkAST(argument.ProtoReflect(), 0, &remaining, &payload); err != nil {
			return nil, err
		}
		if err := validateUtilityArgument(ctx, argument); err != nil {
			return nil, err
		}
		value, err := b.scalar(argument, scope)
		if err != nil {
			return nil, err
		}
		value, err = b.coerce(scope, value, targets[i], ir.AssignmentCast)
		if err != nil {
			return nil, err
		}
		slot, err := b.slot(scope, value.typ, value.nullable, 0, 0)
		if err != nil {
			return nil, err
		}
		outputs[i], expressions[i] = slot, value.id
		columns[i] = ResultColumn{Name: fmt.Sprintf("parameter%d", i+1), Layout: value.typ, Nullable: value.nullable, Slot: slot}
		effects = effects.Union(value.effects)
	}
	root, err := b.builder.AddRel(ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope.id, Rows: 1, Effects: effects}, Outputs: outputs, Expressions: expressions})
	if err != nil {
		return nil, err
	}
	graph, err := b.builder.Build(ctx, root)
	if err != nil {
		return nil, err
	}
	statement := &BoundStatement{graph: graph, columns: columns, dependencies: b.dependencies, provisional: b.provisional, effects: effects, warnings: b.warnings}
	if snapshot != nil {
		statement.timestamp = snapshot.Timestamp()
	}
	return statement, nil
}

func validateUtilityArgument(ctx context.Context, expression *zpgparse.Node) error {
	return rewriteIndexAST(ctx, expression.ProtoReflect(), func(node *zpgparse.Node) error {
		if node.GetParamRef() != nil {
			return bindError("42P02", "EXECUTE arguments cannot reference external parameters", 0)
		}
		if node.GetColumnRef() != nil {
			return bindError("42703", "EXECUTE arguments cannot reference columns", 0)
		}
		if node.GetSubLink() != nil {
			return bindError("0A000", "EXECUTE arguments cannot contain subqueries", 0)
		}
		return nil
	})
}
