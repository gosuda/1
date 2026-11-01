package exec

import (
	"fmt"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exunnest "github.com/gosuda/supersql-alpha/internal/query/exec/unnest"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) rowSource(node physical.NodeSpec, layouts []model.TypeLayout, graph *ir.Graph, relation ir.Rel) (storage.BatchCursor, error) {
	count := relation.ExpressionCount()
	if relation.JSONTableRoot != 0 {
		count = uint32(relation.JSONTableArguments)
	}
	if err := c.charge(uint64(count) * (uint64(unsafe.Sizeof(ir.ScalarID(0))) + uint64(unsafe.Sizeof(model.TypeLayout{})))); err != nil {
		return nil, err
	}
	roots := make([]ir.ScalarID, count)
	arguments := make([]model.TypeLayout, count)
	for i := range roots {
		roots[i], _ = graph.Expression(node.Logical.Rel, uint32(i))
		scalar, ok := graph.Scalar(roots[i])
		if !ok {
			return nil, fmt.Errorf("row source argument %d: %w", i, ErrInvalid)
		}
		var err error
		arguments[i], err = c.layout(graph, scalar.Layout, 0)
		if err != nil {
			return nil, err
		}
	}
	result, err := c.layout(graph, relation.RowResult, 0)
	if err != nil {
		return nil, err
	}
	env := &c.portal.env
	maximum := env.Limits.MetadataBytes - c.used
	var plan builtin.RowSourcePlan
	if relation.JSONTableRoot != 0 {
		plan, err = c.resolveJSONTable(node.Logical.Rel, graph, relation, arguments, result, maximum)
	} else {
		plan, err = c.resolveRowSource(relation.RowFunction, arguments, result, relation.RowCollation, relation.RowStrict, relation.RowVariadicArray, maximum)
	}
	if err != nil {
		return nil, err
	}
	if err = c.charge(plan.OwnedBytes()); err != nil {
		return nil, err
	}
	// A row source with no declared expressions carries no argument program;
	// its provider is opened with an empty argument row. COPY FROM is that
	// intrinsic source. expr.Compile rejects a zero-output program, so an empty
	// group must not be compiled at all.
	var program *expr.Program
	if len(roots) != 0 {
		program, err = c.program(node.Logical.Graph, roots, nil)
		if err != nil {
			return nil, err
		}
	}
	if c.preparing {
		return nil, nil
	}
	return exunnest.New(c.portal.ctx, exunnest.Options{
		Arguments: program, Element: result, Rows: plan, Output: layouts, Ordinality: relation.UnnestOrdinality,
		Parameters: env.Parameters, Captures: c.captures, Query: env.Query, Subqueries: c, Sequences: env.Sequences, Relations: env.Relations, Temporal: env.Temporal, Settings: env.Settings, TextSearch: env.TextSearch, Advisory: env.Advisory, Native: env.Native,
		SessionIntrospection: env.SessionIntrospection, CatalogMetadata: catalogMetadataRuntime(env.Catalog),
		BatchRows: env.Limits.BatchRows, BatchBytes: env.Limits.BatchBytes,
	})
}

func (c *invocation) resolveJSONTable(id ir.RelID, graph *ir.Graph, relation ir.Rel, arguments []model.TypeLayout, result model.TypeLayout, maximum uint64) (builtin.RowSourcePlan, error) {
	path := func(ordinal uint32) ([]byte, error) {
		scalar, ok := graph.Expression(id, ordinal-1)
		if !ok {
			return nil, fmt.Errorf("json_table path expression %d: %w", ordinal, ErrInvalid)
		}
		value, err := graph.CloneConstant(c.portal.ctx, scalar, maximum)
		if err != nil {
			return nil, err
		}
		if value.Type != model.TypeBytes || value.IsNull {
			return nil, fmt.Errorf("json_table path expression %d type %v: %w", ordinal, value.Type, ErrInvalid)
		}
		return value.BytesVal, nil
	}
	root, err := path(relation.JSONTableRoot)
	if err != nil {
		return nil, err
	}
	spec := builtin.JSONTableSpec{
		Root: root, InputFormat: builtin.SQLJSONInputFormat(relation.JSONTableFormat),
		InputEncoding: builtin.SQLJSONEncoding(relation.JSONTableEncoding), Passing: int(relation.JSONTablePassing),
		EmptyOnError: relation.JSONTableFlags&ir.SQLJSONCaptureError != 0, UseTZ: relation.JSONTableFlags&ir.SQLJSONUseTZ != 0,
		Columns: make([]builtin.JSONTableColumnSpec, relation.JSONTableColumnCount()),
	}
	for i := range relation.JSONTableColumnCount() {
		column, ok := graph.JSONTableColumn(id, i)
		if !ok {
			return nil, ErrInvalid
		}
		spec.Columns[i] = builtin.JSONTableColumnSpec{
			Kind: builtin.JSONTableColumnKind(column.Kind), Parent: int(column.Parent), Output: int(column.Output),
			OutputFormat: builtin.SQLJSONInputFormat(column.JSONFormat), OutputEncoding: builtin.SQLJSONEncoding(column.JSONEncoding),
			Wrapper: builtin.SQLJSONWrapper(column.JSONWrapper), Quotes: builtin.SQLJSONQuotes(column.JSONQuotes),
			CaptureEmpty: column.JSONFlags&ir.SQLJSONCaptureEmpty != 0, CaptureError: column.JSONFlags&ir.SQLJSONCaptureError != 0,
		}
		if column.Path != 0 {
			spec.Columns[i].Path, err = path(column.Path)
			if err != nil {
				return nil, err
			}
		}
	}
	return builtin.ResolveJSONTable(c.portal.ctx, spec, arguments, result, c.portal.env.Builtin, maximum)
}

func (c *invocation) resolveRowSource(id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, inputCollation model.CollationID, strict, variadicArray bool, maximum uint64) (builtin.RowSourcePlan, error) {
	env := &c.portal.env
	options := env.Builtin
	options.Collation = inputCollation
	if id == qcopy.RowSourceFunction {
		if len(arguments) != 0 || env.CopySource == nil {
			return nil, ErrInvalid
		}
		return env.CopySource, nil
	}
	if options.InformationSchemaFunctions != nil {
		if function, ok := options.InformationSchemaFunctions.ResolveInformationSchemaFunction(id); ok && function.Operation == builtin.InformationExpandArray {
			return builtin.ResolveExpandArrayRows(c.portal.ctx, id, arguments, result, options, maximum)
		}
	}
	switch id {
	case 938, 939, 1066, 1067, 1068, 1069, 1191, 1192, 1293, 2331, 3259, 3260, 6160, 6161, 6274:
		return builtin.ResolveNativeRows(c.portal.ctx, id, arguments, result, options, maximum)
	case 2763, 2764, 2765, 2766:
		return builtin.ResolveRegexpRows(c.portal.ctx, id, arguments, result, options, maximum)
	case 1179, 4006:
		return builtin.ResolveJSONPathRows(c.portal.ctx, id, arguments, result, options, maximum)
	case 3208, 3219, 3465, 3931, 3932, 3955, 3957, 3958, 3959, 3969:
		return builtin.ResolveJSONCollectionRows(c.portal.ctx, id, arguments, result, options, maximum)
	case 3205, 3491, 3475, 3961:
		return builtin.ResolveJSONRecordRows(c.portal.ctx, id, arguments, result, options, maximum)
	case 3322, 3689, 3690, 3713, 3714, 3715, 3716, 6183, 6184:
		return builtin.ResolveFullTextRows(c.portal.ctx, id, arguments, result, options, maximum)
	case 1686, 2084, 2289, 2510, 2511, 2599, 2856, 3423, 3425, 3427, 6159, 6401:
		return builtin.ResolveIntrospectionRows(c.portal.ctx, id, arguments, result, options, maximum)
	}
	resolver, ok := env.Functions.(expr.RowSourceResolver)
	if !ok {
		return nil, &builtin.UnknownFunctionError{ID: id}
	}
	return resolver.ResolveRows(expr.RowSourceFunction{
		ID: id, Arguments: arguments, Result: result, Strict: strict,
		VariadicArray: variadicArray, InputCollation: inputCollation, Context: c.portal.ctx, MaximumBytes: maximum,
	})
}

func catalogMetadataRuntime(source any) model.CatalogMetadataIntrospection {
	runtime, _ := source.(model.CatalogMetadataIntrospection)
	return runtime
}
