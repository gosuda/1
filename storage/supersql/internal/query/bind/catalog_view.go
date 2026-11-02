package bind

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// BindCatalogViewDefinition freezes one canonical native view without creating
// catalog descriptors or allocating SQL identities.
func BindCatalogViewDefinition(ctx context.Context, source catalogrel.NativeViewSource, snapshot CatalogSnapshot, options Options) (catalogrel.BoundViewDefinition, error) {
	if err := ctx.Err(); err != nil {
		return catalogrel.BoundViewDefinition{}, err
	}
	registry := options.CatalogRelations
	if !registry.IdentityComplete() || registry.Complete() {
		return catalogrel.BoundViewDefinition{}, nativeViewDefinitionError(source, &model.Error{Code: "55000", Message: "catalog bootstrap registry is unavailable"})
	}
	parsed, err := zpgparse.Parse(source.SQL)
	if err != nil {
		return catalogrel.BoundViewDefinition{}, nativeViewDefinitionError(source, err)
	}
	if parsed == nil || len(parsed.Stmts) != 1 || parsed.Stmts[0] == nil || parsed.Stmts[0].Stmt == nil {
		return catalogrel.BoundViewDefinition{}, nativeViewDefinitionError(source, bindError("42601", "native view definition must contain one statement", 0))
	}
	path := make([]string, 0, len(options.SearchPath)+1)
	path = append(path, source.Namespace)
	for _, namespace := range options.SearchPath {
		if namespace != source.Namespace {
			path = append(path, namespace)
		}
	}
	options.SearchPath = path
	bound, metadata, err := bindViewDefinition(ctx, parsed.Stmts[0].Stmt, nil, zpgparse.ViewCheckOption_NO_CHECK_OPTION, snapshot, CreateDeclarationOptions{Bind: options})
	if err != nil {
		return catalogrel.BoundViewDefinition{}, nativeViewDefinitionError(source, err)
	}
	if len(bound.Warnings()) != 0 {
		return catalogrel.BoundViewDefinition{}, nativeViewDefinitionError(source, &model.Error{Code: "22023", Message: "native view definition produced binding warnings"})
	}
	columns := bound.Columns()
	outputs := make([]catalogrel.ViewOutputColumn, len(columns))
	for i, column := range columns {
		outputs[i] = catalogrel.ViewOutputColumn{Name: column.Name, Layout: column.Layout}
	}
	definition, err := registry.BoundView(source, metadata.Query, outputs)
	if err != nil {
		return catalogrel.BoundViewDefinition{}, nativeViewDefinitionError(source, err)
	}
	return definition, nil
}

func nativeViewDefinitionError(source catalogrel.NativeViewSource, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	qualified := source.Namespace + "." + source.Name
	var diagnostic *model.Error
	if !errors.As(err, &diagnostic) {
		code := "XX000"
		var state interface{ SQLState() string }
		if errors.As(err, &state) {
			code = state.SQLState()
		}
		return &model.Error{Code: code, Message: err.Error(), Detail: qualified, Cause: err}
	}
	detail := qualified
	if diagnostic.Detail != "" && diagnostic.Detail != qualified {
		detail += ": " + diagnostic.Detail
	}
	return &model.Error{
		Code: diagnostic.Code, Message: diagnostic.Message, Detail: detail, Hint: diagnostic.Hint,
		Column: diagnostic.Column, Constraint: diagnostic.Constraint, Datatype: diagnostic.Datatype,
		Table: diagnostic.Table, Schema: diagnostic.Schema, Context: diagnostic.Context,
		Position: diagnostic.Position, FeatureID: diagnostic.FeatureID, Cause: err,
	}
}
