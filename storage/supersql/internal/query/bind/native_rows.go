package bind

import (
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) nativeRowsFunction(id model.FunctionID) bool {
	if operation, ok := b.options.CatalogRelations.InformationSchemaFunction(id); ok && operation == builtin.InformationExpandArray {
		return true
	}
	switch id {
	case 938, 939, 1066, 1067, 1068, 1069, 1191, 1192, 1293, 1686, 2084, 2289, 2331, 2510, 2511,
		2599, 2856, 3259, 3260, 3322, 3423, 3425, 3427, 3689, 3690, 3713, 3714, 3715, 3716,
		6159, 6160, 6161, 6183, 6184, 6274, 6401:
		return true
	default:
		return false
	}
}

func (b *binder) nativeRangeFunction(item rangeFunctionCall, alias *zpgparse.Alias, ordinality bool, s *scope, signature builtin.Signature, args []expression, expandVariadic bool) (relation, error) {
	position := int(item.call.Location) + 1
	if !signature.ReturnsSet {
		return relation{}, missing("table function implementation", position)
	}
	var value expression
	var err error
	if result, known, resultErr := b.setRowResult(signature.ID, args, position); resultErr != nil {
		return relation{}, resultErr
	} else if known {
		targets, targetErr := b.functionArguments(signature, args, position)
		if targetErr != nil {
			return relation{}, targetErr
		}
		value, err = b.invokeArguments(s, signature, 0, args, ir.Call, 0, position, result, targets[:len(args)], false)
	} else if signature.Variadic == 2276 {
		value, err = b.invokeVariadic(s, signature, args, item.call.FuncVariadic, position)
	} else if signature.Variadic != 0 {
		value, err = b.invokeTypedVariadic(s, signature, args, expandVariadic, position)
	} else {
		value, err = b.invoke(s, signature, 0, args, ir.Call, 0, position)
	}
	if err != nil {
		return relation{}, err
	}
	return b.rowSourceRelation(value, item.definitions, alias, item.parts[len(item.parts)-1], ordinality, s, position, true)
}

// rowSourceRelation builds the generic row-source relation a FROM-clause call
// becomes: each emitted value is one scalar column unless expandComposite says
// a composite result spreads its fields across output columns. PostgreSQL lets
// a single output parameter keep a composite value in one named column.
func (b *binder) rowSourceRelation(value expression, definitions []*zpgparse.Node, alias *zpgparse.Alias, name string, ordinality bool, s *scope, position int, expandComposite bool) (relation, error) {
	call, ok := b.builder.Scalar(value.id)
	if !ok || call.Kind != ir.Call {
		return relation{}, bindError("XX000", "row source call is missing", position)
	}
	signature, ok := b.builder.Signature(call.Signature)
	if !ok {
		return relation{}, bindError("XX000", "row source signature is missing", position)
	}
	result := value.typ
	var columns []column
	var err error
	if result.Type.Storage == model.TypeComposite && expandComposite {
		if len(definitions) != 0 {
			if result.Type.ID != 2249 {
				return relation{}, bindError("42601", "a column definition list is redundant for a function returning a named composite type", position)
			}
			if len(result.Children) != 0 {
				return relation{}, bindError("42601", "a column definition list is redundant for a function with OUT parameters", position)
			}
			columns, err = b.unnestRecordDefinitions(s, definitions, position)
			result.Children = make([]model.TypeLayout, len(columns))
			result.FieldNames = make([]string, len(columns))
			for i, c := range columns {
				result.Children[i], result.FieldNames[i] = c.Layout, c.Name
			}
		} else if result.Type.ID != 2249 {
			columns, err = b.unnestNamedColumns(s, result, position)
		} else if len(result.Children) != 0 && len(result.Children) == len(result.FieldNames) {
			columns, err = b.rowSourceColumns(s, result.FieldNames, result.Children, position)
		} else {
			return relation{}, bindError("42601", `a column definition list is required for functions returning "record"`, position)
		}
	} else {
		if len(definitions) != 0 {
			return relation{}, bindError("42601", `a column definition list is only allowed for functions returning "record"`, position)
		}
		columnName := name
		if alias != nil {
			columnName = alias.Aliasname
		}
		columns, err = b.rowSourceColumns(s, []string{columnName}, []model.TypeLayout{result}, position)
	}
	if err != nil {
		return relation{}, err
	}
	layout, err := b.layout(result)
	if err != nil {
		return relation{}, err
	}
	if ordinality {
		columns, err = b.functionOrdinality(s, columns)
		if err != nil {
			return relation{}, err
		}
	}
	expressions := make([]ir.ScalarID, call.OperandCount())
	for i := range call.OperandCount() {
		expressions[i], ok = b.builder.Operand(value.id, i)
		if !ok {
			return relation{}, bindError("XX000", "row source argument is missing", position)
		}
	}
	effects := value.effects
	effects.MayError = true
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{
		Kind: ir.Unnest, Scope: s.id, Span: sourceSpan(position), Effects: effects,
		RowFunction: call.Function, RowResult: layout, RowCollation: signature.InputCollation,
		RowStrict: signature.Strict, RowVariadicArray: signature.VariadicArray, UnnestOrdinality: ordinality,
	}, Outputs: outputSlots(columns), Expressions: expressions})
	return relation{id: id, columns: columns, effects: effects}, err
}

func (b *binder) introspectionRowLayout(id model.FunctionID) (model.TypeLayout, bool) {
	text := b.primitive(25)
	boolean := b.primitive(16)
	int4 := b.primitive(23)
	int8 := b.primitive(20)
	regclass := b.primitive(2205)
	interval := b.primitive(1186)
	var names []string
	var children []model.TypeLayout
	switch id {
	case 1686:
		names = []string{"word", "catcode", "barelabel", "catdesc", "baredesc"}
		children = []model.TypeLayout{text, b.primitive(18), boolean, text, text}
	case 2084:
		names = []string{"name", "setting", "unit", "category", "short_desc", "extra_desc", "context", "vartype", "source", "min_val", "max_val", "enumvals", "boot_val", "reset_val", "sourcefile", "sourceline", "pending_restart"}
		children = []model.TypeLayout{text, text, text, text, text, text, text, text, text, text, text, b.primitive(1009), text, text, text, int4, boolean}
	case 2289:
		names = []string{"option_name", "option_value"}
		children = []model.TypeLayout{text, text}
	case 2510:
		names = []string{"name", "statement", "prepare_time", "parameter_types", "result_types", "from_sql", "generic_plans", "custom_plans"}
		children = []model.TypeLayout{text, text, b.primitive(1184), b.primitive(2211), b.primitive(2211), boolean, int8, int8}
	case 2511:
		names = []string{"name", "statement", "is_holdable", "is_binary", "is_scrollable", "creation_time"}
		children = []model.TypeLayout{text, text, boolean, boolean, boolean, b.primitive(1184)}
	case 2599, 6401:
		names = []string{"abbrev", "utc_offset", "is_dst"}
		children = []model.TypeLayout{text, interval, boolean}
	case 2856:
		names = []string{"name", "abbrev", "utc_offset", "is_dst"}
		children = []model.TypeLayout{text, text, interval, boolean}
	case 3423:
		names = []string{"relid", "parentrelid", "isleaf", "level"}
		children = []model.TypeLayout{regclass, regclass, boolean, int4}
	case 3425:
		return regclass, true
	case 3427:
		names = []string{"index", "values", "nulls", "frequency", "base_frequency"}
		children = []model.TypeLayout{int4, b.primitive(1009), b.primitive(1000), b.primitive(701), b.primitive(701)}
	case 6159:
		names = []string{"fktable", "fkcols", "pktable", "pkcols", "is_array", "is_opt"}
		children = []model.TypeLayout{regclass, b.primitive(1009), regclass, b.primitive(1009), boolean, boolean}
	default:
		return model.TypeLayout{}, false
	}
	result := b.primitive(2249)
	result.FieldNames, result.Children = names, children
	return result, true
}

func (b *binder) fullTextRowLayout(id model.FunctionID) (model.TypeLayout, bool) {
	text := b.primitive(25)
	int4 := b.primitive(23)
	var names []string
	var children []model.TypeLayout
	switch id {
	case 3322:
		names = []string{"lexeme", "positions", "weights"}
		children = []model.TypeLayout{text, b.primitive(1005), b.primitive(1009)}
	case 3689, 3690:
		names = []string{"word", "ndoc", "nentry"}
		children = []model.TypeLayout{text, int4, int4}
	case 3713, 3714:
		names = []string{"tokid", "alias", "description"}
		children = []model.TypeLayout{int4, text, text}
	case 3715, 3716:
		names = []string{"tokid", "token"}
		children = []model.TypeLayout{int4, text}
	case 6183, 6184:
		names = []string{"alias", "description", "token", "dictionaries", "dictionary", "lexemes"}
		children = []model.TypeLayout{text, text, text, b.primitive(3770), b.primitive(3769), b.primitive(1009)}
	default:
		return model.TypeLayout{}, false
	}
	result := b.primitive(2249)
	result.FieldNames, result.Children = names, children
	return result, true
}

func (b *binder) setRowResult(id model.FunctionID, args []expression, position int) (model.TypeLayout, bool, error) {
	if operation, ok := b.options.CatalogRelations.InformationSchemaFunction(id); ok && operation == builtin.InformationExpandArray {
		if len(args) != 1 || args[0].typ.Type.Storage != model.TypeArray || len(args[0].typ.Children) != 1 {
			return model.TypeLayout{}, false, bindError("42804", "_pg_expandarray requires a concrete array", position)
		}
		result := b.primitive(2249)
		result.FieldNames = []string{"x", "n"}
		result.Children = []model.TypeLayout{args[0].typ.Children[0], b.primitive(23)}
		return result, true, nil
	}
	if result, known := b.introspectionRowLayout(id); known {
		return result, true, nil
	}
	if result, known := b.fullTextRowLayout(id); known {
		return result, true, nil
	}
	text := b.primitive(25)
	switch id {
	case 3208:
		result := b.primitive(2249)
		result.FieldNames = []string{"key", "value"}
		result.Children = []model.TypeLayout{text, b.primitive(3802)}
		return result, true, nil
	case 3932, 3959:
		result := b.primitive(2249)
		result.FieldNames = []string{"key", "value"}
		result.Children = []model.TypeLayout{text, text}
		return result, true, nil
	case 3958:
		result := b.primitive(2249)
		result.FieldNames = []string{"key", "value"}
		result.Children = []model.TypeLayout{text, b.primitive(114)}
		return result, true, nil
	case 3475, 3961:
		if len(args) == 0 || args[0].typ.Type.Storage != model.TypeComposite {
			return model.TypeLayout{}, false, bindError("42804", "first argument must be a row type", position)
		}
		return args[0].typ, true, nil
	case 3205, 3491:
		return model.TypeLayout{}, false, bindError("0A000", "could not determine row type for set-returning function", position)
	default:
		return model.TypeLayout{}, false, nil
	}
}
