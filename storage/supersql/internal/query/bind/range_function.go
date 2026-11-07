package bind

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type rangeFunctionCall struct {
	call        *zpgparse.FuncCall
	parts       []string
	args        []*zpgparse.Node
	definitions []*zpgparse.Node
}

func (b *binder) rangeFunction(node *zpgparse.RangeFunction, s *scope) (relation, error) {
	items, err := b.rangeFunctionCalls(node)
	if err != nil {
		return relation{}, err
	}
	name := items[0].parts[len(items[0].parts)-1]
	if len(items) == 1 {
		result, err := b.rangeFunctionCall(items[0], node.Alias, node.Ordinality, s)
		if err != nil {
			return relation{}, err
		}
		return b.aliasRelation(result, s, node.Alias, name)
	}
	inputs := make([]ir.RelID, len(items))
	var columns []column
	effects := ir.Effects{MayError: true}
	for i, item := range items {
		child, err := b.rangeFunctionCall(item, nil, false, s)
		if err != nil {
			return relation{}, err
		}
		count := uint64(len(columns)) + uint64(len(child.columns))
		if node.Ordinality {
			count++
		}
		const columnBytes = uint64(3*unsafe.Sizeof(column{}) + unsafe.Sizeof(model.SlotID(0)))
		if count > uint64(b.options.Limits.MaxOperands) || count > uint64(b.options.Limits.MaxSlots) || count > b.options.Limits.MaxBytes/columnBytes {
			return relation{}, bindError("54000", "ROWS FROM column limit exceeded", int(item.call.Location)+1)
		}
		for _, c := range child.columns {
			slot, err := b.slot(s, c.Layout, true, 0, 0)
			if err != nil {
				return relation{}, err
			}
			c.Slot, c.Nullable = slot, true
			columns = append(columns, c)
		}
		inputs[i] = child.id
		effects = effects.Union(child.effects)
	}
	if node.Ordinality {
		columns, err = b.functionOrdinality(s, columns)
		if err != nil {
			return relation{}, err
		}
	}
	columns, err = aliasOwnedColumns(columns, node.Alias, name)
	if err != nil {
		return relation{}, err
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.RowsFrom, Scope: s.id, Span: sourceSpan(int(items[0].call.Location) + 1), Effects: effects, RowsFromOrdinality: node.Ordinality}, Inputs: inputs, Outputs: outputSlots(columns)})
	if err != nil {
		return relation{}, err
	}
	return b.nameRelation(relation{id: id, columns: columns, effects: effects}, s, relationQualifier{name: columnsQualifier(node.Alias, name)}, nil)
}

func (b *binder) rangeFunctionCalls(node *zpgparse.RangeFunction) ([]rangeFunctionCall, error) {
	var count uint64
	for _, function := range node.Functions {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		item := function.GetList()
		if item == nil || len(item.Items) != 2 {
			return nil, bindError("42601", "invalid table function", 0)
		}
		call := item.Items[0].GetFuncCall()
		if call == nil {
			return nil, missing("table function expression", 0)
		}
		definitions := item.Items[1].GetList().GetItems()
		if len(definitions) != 0 && len(node.Coldeflist) != 0 {
			return nil, bindError("42601", "multiple column definition lists for one function", int(call.Location)+1)
		}
		if expandRangeUnnest(call, definitions) {
			count += uint64(len(call.Args))
		} else {
			count++
		}
	}
	if count == 0 {
		return nil, bindError("42601", "invalid table function", 0)
	}
	const itemBytes = uint64(unsafe.Sizeof(rangeFunctionCall{}) + unsafe.Sizeof(ir.RelID(0)))
	if count > uint64(b.options.Limits.MaxOperands) || count > uint64(b.options.Limits.MaxNodes) || count > b.options.Limits.MaxBytes/itemBytes {
		return nil, bindError("54000", "ROWS FROM function limit exceeded", 0)
	}
	if len(node.Coldeflist) != 0 && (count != 1 || node.Ordinality) {
		return nil, bindError("42601", "column definitions must be attached to an individual ROWS FROM function", 0)
	}
	items := make([]rangeFunctionCall, 0, int(count))
	for _, function := range node.Functions {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		item := function.GetList()
		call := item.Items[0].GetFuncCall()
		parts, err := names(call.Funcname)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 || len(parts) > 2 {
			return nil, bindError("42883", "function does not exist", int(call.Location)+1)
		}
		definitions := item.Items[1].GetList().GetItems()
		if expandRangeUnnest(call, definitions) {
			parts = []string{"pg_catalog", "unnest"}
			for i := range call.Args {
				if err := b.ctx.Err(); err != nil {
					return nil, err
				}
				items = append(items, rangeFunctionCall{call: call, parts: parts, args: call.Args[i : i+1]})
			}
			continue
		}
		if len(node.Coldeflist) != 0 {
			definitions = node.Coldeflist
		}
		items = append(items, rangeFunctionCall{call: call, parts: parts, args: call.Args, definitions: definitions})
	}
	return items, nil
}

func expandRangeUnnest(call *zpgparse.FuncCall, definitions []*zpgparse.Node) bool {
	// PostgreSQL's FROM-only rewrite forces the built-in, bypassing search_path.
	return len(call.Funcname) == 1 && call.Funcname[0].GetString_().GetSval() == "unnest" && len(call.Args) > 1 && len(definitions) == 0 &&
		len(call.AggOrder) == 0 && call.AggFilter == nil && call.Over == nil && !call.AggStar && !call.AggDistinct && !call.AggWithinGroup && !call.FuncVariadic
}

func (b *binder) rangeFunctionCall(item rangeFunctionCall, alias *zpgparse.Alias, ordinality bool, s *scope) (relation, error) {
	call, parts := item.call, item.parts
	pos := int(call.Location) + 1
	name := parts[len(parts)-1]
	aggregateSyntax := call.AggStar || call.AggDistinct || call.AggFilter != nil || len(call.AggOrder) != 0 || call.AggWithinGroup
	if call.Over != nil || aggregateSyntax {
		return relation{}, bindError("42809", "aggregate or window syntax in table function", pos)
	}
	allowAggregate, allowWindow := s.allowAggregate, s.allowWindow
	s.allowAggregate, s.allowWindow = false, false
	arguments, err := b.callArguments(item.args, s, pos)
	s.allowAggregate, s.allowWindow = allowAggregate, allowWindow
	if err != nil {
		return relation{}, err
	}
	args := arguments.values
	for _, schema := range b.functionPath(parts) {
		if schema != "pg_catalog" && schema != "information_schema" {
			if source, found, err := b.userRowFunction(schema, name, arguments, s, pos, call.FuncVariadic); err != nil {
				return relation{}, err
			} else if found {
				return b.userRangeFunction(item, alias, ordinality, s, source, name, pos)
			}
			continue
		}
		resolved, found, err := b.builtinFunction(name, arguments, s, pos, call.FuncVariadic)
		if err != nil {
			return relation{}, err
		}
		if !found {
			continue
		}
		_, informationFunction := b.options.CatalogRelations.InformationSchemaFunction(resolved.signature.ID)
		if informationFunction != (schema == "information_schema") {
			continue
		}
		signature := resolved.signature
		args = resolved.arguments
		if jsonRowsFunction(signature.ID) {
			return b.jsonRangeFunction(item, alias, ordinality, s, signature, args)
		}
		if regexpRowsFunction(signature.ID) {
			return b.regexpRangeFunction(item, alias, ordinality, s, signature, args)
		}
		if kind, ok := generatorKind(signature.ID); ok {
			return b.generatorRangeFunction(item, alias, ordinality, s, signature, args, kind)
		}
		if unnestKind(signature.ID) {
			return b.unnestRangeFunction(item, alias, ordinality, s, signature, args)
		}
		if b.nativeRowsFunction(signature.ID) {
			return b.nativeRangeFunction(item, alias, ordinality, s, signature, args, resolved.expandVariadic)
		}
		return relation{}, missing("table function implementation", pos)
	}
	return relation{}, bindError("42883", "function does not exist", pos)
}

// generatorKind reports the dedicated IR relation for the integer series and
// array subscript generators. They keep their own kinds because their argument
// and output contracts are stricter than a generic row source: the outputs are
// non-null and the arguments are coerced in declaration order.
func generatorKind(id model.FunctionID) (ir.RelKind, bool) {
	switch id {
	case 938, 939, 1066, 1067, 1068, 1069, 3259, 3260, 6274:
		return ir.GenerateSeries, true
	case 1191, 1192:
		return ir.GenerateSubscripts, true
	default:
		return 0, false
	}
}

func (b *binder) generatorRangeFunction(item rangeFunctionCall, alias *zpgparse.Alias, ordinality bool, s *scope, signature builtin.Signature, args []expression, kind ir.RelKind) (relation, error) {
	pos := int(item.call.Location) + 1
	if len(item.definitions) != 0 {
		return relation{}, bindError("42601", "column definitions require a record-returning function", pos)
	}
	targets, err := b.functionArguments(signature, args, pos)
	if err != nil {
		return relation{}, err
	}
	expressions := make([]ir.ScalarID, len(args))
	effects := ir.Effects{MayError: true}
	for i, argument := range args {
		value, err := b.coerce(s, argument, targets[i], ir.ImplicitCast)
		if err != nil {
			return relation{}, err
		}
		expressions[i] = value.id
		effects = effects.Union(value.effects)
	}
	layout := b.primitive(signature.Result)
	name := item.parts[len(item.parts)-1]
	if alias != nil {
		name = alias.Aliasname
	}
	slot, err := b.slot(s, layout, false, 0, 0)
	if err != nil {
		return relation{}, err
	}
	columns := []column{{ResultColumn: ResultColumn{Name: name, Layout: layout, Slot: slot}}}
	if ordinality {
		columns, err = b.functionOrdinality(s, columns)
		if err != nil {
			return relation{}, err
		}
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: kind, Scope: s.id, Span: sourceSpan(pos), Effects: effects}, Outputs: outputSlots(columns), Expressions: expressions})
	return relation{id: id, columns: columns, effects: effects}, err
}

// unnestKind reports the built-in array and multirange expansions. They keep the
// dedicated Unnest relation because their element layout, and not a scalar
// function result, supplies the output columns.
func unnestKind(id model.FunctionID) bool {
	return id == 2331 || id == 1293
}

func (b *binder) unnestRangeFunction(item rangeFunctionCall, alias *zpgparse.Alias, ordinality bool, s *scope, signature builtin.Signature, args []expression) (relation, error) {
	pos := int(item.call.Location) + 1
	targets, err := b.functionArguments(signature, args, pos)
	if err != nil {
		return relation{}, err
	}
	expressions := make([]ir.ScalarID, len(args))
	effects := ir.Effects{MayError: true}
	for i, argument := range args {
		value, err := b.coerce(s, argument, targets[i], ir.ImplicitCast)
		if err != nil {
			return relation{}, err
		}
		expressions[i] = value.id
		effects = effects.Union(value.effects)
	}
	element, err := b.unnestElement(targets[0], pos)
	if err != nil {
		return relation{}, err
	}
	var unnestRange model.Type
	if signature.ID == 1293 {
		unnestRange = element.Type
	}
	columns, err := b.unnestColumns(s, element, item.definitions, alias, item.parts[len(item.parts)-1], pos)
	if err != nil {
		return relation{}, err
	}
	if ordinality {
		columns, err = b.functionOrdinality(s, columns)
		if err != nil {
			return relation{}, err
		}
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Unnest, Scope: s.id, Span: sourceSpan(pos), Effects: effects, UnnestOrdinality: ordinality, UnnestRange: unnestRange}, Outputs: outputSlots(columns), Expressions: expressions})
	return relation{id: id, columns: columns, effects: effects}, err
}

// userRangeFunction binds a FROM-clause call to a user routine into the shared
// row-source relation: a set-returning routine emits each produced row, and a
// non-set routine evaluates once into its single result row. Execution reaches
// the routine through the statement-pinned resolver's RowSourceResolver.
func (b *binder) userRangeFunction(item rangeFunctionCall, alias *zpgparse.Alias, ordinality bool, s *scope, source userRowSource, name string, position int) (relation, error) {
	if len(item.definitions) != 0 {
		switch {
		case source.result.Type.Storage == model.TypeComposite && source.result.Type.ID != 2249:
			return relation{}, bindError("42601", "a column definition list is redundant for a function returning a named composite type", position)
		case len(source.outputs) != 0:
			return relation{}, bindError("42601", "a column definition list is redundant for a function with OUT parameters", position)
		case source.result.Type.Storage != model.TypeComposite:
			return relation{}, bindError("42601", `a column definition list is only allowed for functions returning "record"`, position)
		}
	}
	if source.result.Type.ID == 2249 && len(source.result.FieldNames) == 0 && len(source.outputs) != 0 && len(source.outputs) == len(source.result.Children) {
		// An OUT/TABLE parameter list is the record result's column
		// definition list; reload stripped it off the result layout.
		source.result.FieldNames = source.outputs
	}
	if len(source.outputs) == 1 {
		// A single output parameter names the routine's result column.
		name = source.outputs[0]
	}
	parameters := make([]ir.LayoutID, len(source.arguments))
	for i, argument := range source.arguments {
		var err error
		parameters[i], err = b.layout(argument.typ)
		if err != nil {
			return relation{}, err
		}
	}
	result, err := b.layout(source.result)
	if err != nil {
		return relation{}, err
	}
	signature, err := b.builder.AddSignature(b.ctx, ir.SignatureSpec{Signature: ir.Signature{Function: source.function, Result: result, InputCollation: source.collation, Nullable: true, SetReturning: source.set, VariadicArray: source.variadicArray, Strict: source.strict, Effects: source.effects}, Parameters: parameters})
	if err != nil {
		return relation{}, err
	}
	value, err := b.addScalar(s, expression{typ: source.result, nullable: true, effects: source.effects, position: position}, ir.Scalar{Kind: ir.Call, Function: source.function, Signature: signature, Strict: source.strict}, source.arguments)
	if err != nil {
		return relation{}, err
	}
	return b.rowSourceRelation(value, item.definitions, alias, name, ordinality, s, position, len(source.outputs) != 1)
}

func (b *binder) functionOrdinality(s *scope, columns []column) ([]column, error) {
	ordinal := b.primitive(20)
	slot, err := b.slot(s, ordinal, false, 0, 0)
	if err != nil {
		return nil, err
	}
	return append(columns, column{ResultColumn: ResultColumn{Name: "ordinality", Layout: ordinal, Slot: slot}}), nil
}

func lateralSource(node *zpgparse.Node) *zpgparse.RangeSubselect {
	if sub := node.GetRangeSubselect(); sub != nil && sub.Lateral {
		return sub
	}
	function := node.GetRangeFunction()
	if function == nil {
		return nil
	}
	name := ""
	if function.Alias != nil {
		name = function.Alias.Aliasname
	} else if len(function.Functions) != 0 {
		item := function.Functions[0].GetList()
		if item != nil && len(item.Items) != 0 {
			call := item.Items[0].GetFuncCall()
			if call != nil && len(call.Funcname) != 0 {
				name = call.Funcname[len(call.Funcname)-1].GetString_().GetSval()
			}
		}
	}
	star := &zpgparse.Node{Node: &zpgparse.Node_ColumnRef{ColumnRef: &zpgparse.ColumnRef{Fields: []*zpgparse.Node{{Node: &zpgparse.Node_AStar{AStar: &zpgparse.A_Star{}}}}, Location: -1}}}
	target := &zpgparse.Node{Node: &zpgparse.Node_ResTarget{ResTarget: &zpgparse.ResTarget{Val: star, Location: -1}}}
	selectNode := &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: &zpgparse.SelectStmt{TargetList: []*zpgparse.Node{target}, FromClause: []*zpgparse.Node{node}}}}
	return &zpgparse.RangeSubselect{Lateral: true, Subquery: selectNode, Alias: &zpgparse.Alias{Aliasname: name}}
}
