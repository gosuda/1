package exec

import (
	"context"
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
	case 3035:
		return builtin.ResolveNotifyRows(c.portal.ctx, id, arguments, result, options, maximum)
	}
	if sig, ok := builtin.LookupSignature(id); ok && !sig.ReturnsSet {
		callable, err := builtin.ResolveTyped(c.portal.ctx, id, arguments, result, options, maximum)
		if err != nil {
			return nil, err
		}
		return &scalarRowSourcePlan{
			callable:  callable,
			result:    result,
			strict:    strict,
			sequences: env.Sequences,
			advisory:  env.Advisory,
		}, nil
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

type scalarRowSourcePlan struct {
	callable  builtin.Callable
	result    model.TypeLayout
	strict    bool
	sequences model.SequenceRuntime
	advisory  model.AdvisoryLockRuntime
}

func (p *scalarRowSourcePlan) OwnedBytes() uint64 {
	return uint64(unsafe.Sizeof(*p)) + p.callable.OwnedBytes()
}

func (p *scalarRowSourcePlan) Open(ctx context.Context, arguments []model.Value, options builtin.RowSourceOptions) (builtin.RowSourceIterator, error) {
	expandComposite := p.result.Type.Storage == model.TypeComposite && len(p.result.Children) != 0
	emitNullRow := func() []model.Value {
		if expandComposite {
			row := make([]model.Value, len(p.result.Children))
			for i, child := range p.result.Children {
				row[i] = model.NullVal(child.Type.Storage)
			}
			return row
		}
		return []model.Value{model.NullVal(p.result.Type.Storage)}
	}
	if p.strict {
		for _, arg := range arguments {
			if arg.IsNull {
				return &scalarRowSourceIterator{row: emitNullRow()}, nil
			}
		}
	}
	outputLimit := int64(options.MaxRowBytes)
	if outputLimit <= 0 {
		outputLimit = 1 << 20
	}
	budget := builtin.CallBudget{
		OutputBytes: outputLimit,
		MemoryBytes: options.MaxMemoryBytes,
		Temporal:    options.Temporal,
		Relations:   options.Relations,
		Settings:    options.Settings,
		TextSearch:  options.TextSearch,
		Sequences:   p.sequences,
		Advisory:    p.advisory,
		Native:      options.Native,
	}
	val, err := p.callable.Call(ctx, arguments, budget)
	if err != nil {
		return nil, err
	}
	var row []model.Value
	if expandComposite {
		if val.IsNull {
			row = emitNullRow()
		} else {
			if len(val.ArrayVal) != len(p.result.Children) {
				return nil, ErrInvalid
			}
			row = val.ArrayVal
		}
	} else {
		row = []model.Value{val}
	}
	return &scalarRowSourceIterator{row: row}, nil
}

type scalarRowSourceIterator struct {
	row     []model.Value
	emitted bool
}

func (it *scalarRowSourceIterator) Next(_ context.Context) ([]model.Value, bool, error) {
	if it.emitted {
		return nil, false, nil
	}
	it.emitted = true
	return it.row, true, nil
}

func (it *scalarRowSourceIterator) Close() error {
	it.row = nil
	it.emitted = true
	return nil
}
