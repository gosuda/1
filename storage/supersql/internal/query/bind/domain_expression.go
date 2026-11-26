package bind

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const domainCheckExpressionVersion uint32 = 1

func DefineDomainCheck(ctx context.Context, ast *zpgparse.Node, domain *model.Descriptor, constraint model.ObjectID, base model.TypeLayout, snapshot CatalogSnapshot, options Options) (*model.ExpressionDefinition, []model.Dependency, error) {
	if domain == nil || domain.Kind() != model.ObjectType || constraint == 0 {
		return nil, nil, bindError("22023", "domain CHECK requires a type and constraint identity", 0)
	}
	spec, err := domain.SpecWithin(ctx, descriptorLimits(options.Limits))
	if err != nil {
		return nil, nil, err
	}
	if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindDomain || !sameType(spec.Type.BaseLayout, base) {
		return nil, nil, bindError("42809", "domain CHECK base type differs from its declaration", 0)
	}
	columns := []model.ColumnDescriptor{{ID: 1, Name: "value", State: model.StatePublic, Layout: base, Nullable: true}}
	definition, bound, err := defineRowExpression(ctx, ast, domain, columns, snapshot, options, rowDomainCheck, 0, nil, constraint)
	if err != nil {
		return nil, nil, err
	}
	return definition, bound.Dependencies, nil
}

func BindDomainCheck(ctx context.Context, definition *model.ExpressionDefinition, domain model.ObjectID, constraint model.ObjectID, base model.TypeLayout, snapshot CatalogSnapshot, limits ir.Limits) (*BoundColumnExpression, error) {
	if domain == 0 || constraint == 0 {
		return nil, bindError("22023", "domain CHECK identities are required", 0)
	}
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	columns := []storage.ColumnLayout{{ID: 1, Type: base.Type, Children: base.Children}}
	bound, err := bindRowExpression(ctx, definition, 0, columns, boolean, snapshot, limits, rowDomainCheck, constraint)
	if err != nil {
		return nil, err
	}
	encoding, _, err := decodeRowExpression(ctx, definition.Encoding, limits, "supersql.domain-check", domainCheckExpressionVersion)
	if err != nil {
		return nil, err
	}
	if encoding.Table != domain {
		return nil, bindError("42804", "domain CHECK identity changed", 0)
	}
	return &BoundColumnExpression{Graph: bound.Graph, Root: bound.Root, Parameters: bound.Parameters, Result: bound.Result, Builtin: bound.Builtin, Dependencies: bound.Dependencies}, nil
}
