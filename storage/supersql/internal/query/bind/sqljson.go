package bind

import (
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func sqlJSONFormat(format *zpgparse.JsonFormat) (ir.SQLJSONFormat, ir.SQLJSONEncoding, error) {
	result := ir.SQLJSONDefaultFormat
	encoding := ir.SQLJSONDefaultEncoding
	if format == nil {
		return result, encoding, nil
	}
	switch format.FormatType {
	case zpgparse.JsonFormatType_JSON_FORMAT_TYPE_UNDEFINED, zpgparse.JsonFormatType_JS_FORMAT_DEFAULT:
	case zpgparse.JsonFormatType_JS_FORMAT_JSON:
		result = ir.SQLJSONTextFormat
	case zpgparse.JsonFormatType_JS_FORMAT_JSONB:
		result = ir.SQLJSONBinaryFormat
	default:
		return 0, 0, bindError("42601", "invalid JSON format", int(format.Location)+1)
	}
	switch format.Encoding {
	case zpgparse.JsonEncoding_JSON_ENCODING_UNDEFINED, zpgparse.JsonEncoding_JS_ENC_DEFAULT:
	case zpgparse.JsonEncoding_JS_ENC_UTF8:
		encoding = ir.SQLJSONUTF8
	case zpgparse.JsonEncoding_JS_ENC_UTF16:
		encoding = ir.SQLJSONUTF16
	case zpgparse.JsonEncoding_JS_ENC_UTF32:
		encoding = ir.SQLJSONUTF32
	default:
		return 0, 0, bindError("42601", "invalid JSON encoding", int(format.Location)+1)
	}
	return result, encoding, nil
}

func sqlJSONWrapper(wrapper zpgparse.JsonWrapper) (ir.SQLJSONWrapper, error) {
	switch wrapper {
	case zpgparse.JsonWrapper_JSON_WRAPPER_UNDEFINED, zpgparse.JsonWrapper_JSW_UNSPEC:
		return ir.SQLJSONUnspecifiedWrapper, nil
	case zpgparse.JsonWrapper_JSW_NONE:
		return ir.SQLJSONWithoutWrapper, nil
	case zpgparse.JsonWrapper_JSW_CONDITIONAL:
		return ir.SQLJSONConditionalWrapper, nil
	case zpgparse.JsonWrapper_JSW_UNCONDITIONAL:
		return ir.SQLJSONUnconditionalWrapper, nil
	default:
		return 0, bindError("42601", "invalid JSON wrapper", 0)
	}
}

func sqlJSONQuotes(quotes zpgparse.JsonQuotes) (ir.SQLJSONQuotes, error) {
	switch quotes {
	case zpgparse.JsonQuotes_JSON_QUOTES_UNDEFINED, zpgparse.JsonQuotes_JS_QUOTES_UNSPEC:
		return ir.SQLJSONUnspecifiedQuotes, nil
	case zpgparse.JsonQuotes_JS_QUOTES_KEEP:
		return ir.SQLJSONKeepQuotes, nil
	case zpgparse.JsonQuotes_JS_QUOTES_OMIT:
		return ir.SQLJSONOmitQuotes, nil
	default:
		return 0, bindError("42601", "invalid JSON quotes clause", 0)
	}
}

func sqlJSONItemType(item zpgparse.JsonValueType) (ir.SQLJSONItemType, error) {
	switch item {
	case zpgparse.JsonValueType_JSON_VALUE_TYPE_UNDEFINED, zpgparse.JsonValueType_JS_TYPE_ANY:
		return ir.SQLJSONAnyItem, nil
	case zpgparse.JsonValueType_JS_TYPE_OBJECT:
		return ir.SQLJSONObjectItem, nil
	case zpgparse.JsonValueType_JS_TYPE_ARRAY:
		return ir.SQLJSONArrayItem, nil
	case zpgparse.JsonValueType_JS_TYPE_SCALAR:
		return ir.SQLJSONScalarItem, nil
	default:
		return 0, bindError("42601", "invalid JSON item type", 0)
	}
}

func sqlJSONPayload(operation ir.SQLJSONOperation) ir.Scalar {
	return ir.Scalar{
		Kind:               ir.SQLJSON,
		JSONOperation:      operation,
		JSONFormat:         ir.SQLJSONDefaultFormat,
		JSONEncoding:       ir.SQLJSONDefaultEncoding,
		JSONOutputFormat:   ir.SQLJSONDefaultFormat,
		JSONOutputEncoding: ir.SQLJSONDefaultEncoding,
		JSONWrapper:        ir.SQLJSONUnspecifiedWrapper,
		JSONQuotes:         ir.SQLJSONUnspecifiedQuotes,
		JSONItemType:       ir.SQLJSONAnyItem,
	}
}

func (b *binder) addSQLJSON(s *scope, result model.TypeLayout, nullable bool, position int, payload ir.Scalar, args []expression) (expression, error) {
	effects := ir.Effects{MayError: true}
	if payload.JSONOperation == ir.SQLJSONScalar || payload.JSONOperation == ir.SQLJSONArray || payload.JSONOperation == ir.SQLJSONObject {
		effects.Volatility = b.sqlJSONConversionVolatility(args)
	}
	return b.addScalar(s, expression{typ: result, nullable: nullable, effects: effects, position: position}, payload, args)
}

func (b *binder) sqlJSONConversionVolatility(args []expression) ir.Volatility {
	volatility := ir.Immutable
	for _, argument := range args {
		if argument.typ.Type.Storage == model.TypeArray || argument.typ.Type.Storage == model.TypeComposite ||
			argument.typ.Type.ID == 1082 || argument.typ.Type.ID == 1114 || argument.typ.Type.ID == 1184 {
			volatility = max(volatility, ir.Stable)
			continue
		}
		info, ok := b.lookupTypeIO(argument.typ)
		if !ok || info.Output == 0 {
			continue
		}
		signature, ok := builtin.LookupSignature(info.Output)
		if !ok {
			volatility = max(volatility, ir.Stable)
			continue
		}
		switch signature.Volatility {
		case 's':
			volatility = max(volatility, ir.Stable)
		case 'v':
			return ir.Volatile
		}
	}
	return volatility
}

func (b *binder) jsonValueInput(value *zpgparse.JsonValueExpr, s *scope, position int) (expression, ir.SQLJSONFormat, ir.SQLJSONEncoding, error) {
	if value == nil {
		return expression{}, 0, 0, bindError("42601", "missing JSON value expression", position)
	}
	node := value.RawExpr
	if node == nil {
		node = value.FormattedExpr
	}
	if node == nil {
		return expression{}, 0, 0, bindError("42601", "missing JSON value expression", position)
	}
	input, err := b.scalar(node, s)
	if err != nil {
		return expression{}, 0, 0, err
	}
	input, err = b.defaultType(s, input)
	if err != nil {
		return expression{}, 0, 0, err
	}
	format, encoding, err := sqlJSONFormat(value.Format)
	if err != nil {
		return expression{}, 0, 0, err
	}
	if encoding != ir.SQLJSONDefaultEncoding && input.typ.Type.ID != 17 {
		return expression{}, 0, 0, bindError("42804", "JSON encoding is only allowed for bytea input", position)
	}
	return input, format, encoding, nil
}

func (b *binder) sqlJSONDocumentInput(input expression, format ir.SQLJSONFormat, s *scope, position int, construct string) (expression, ir.SQLJSONFormat, error) {
	switch input.typ.Type.ID {
	case 17:
		if format == ir.SQLJSONDefaultFormat {
			format = ir.SQLJSONTextFormat
		}
	case 114, 3802:
	default:
		if b.typeCategory(input.typ) != 'S' {
			return expression{}, 0, bindError("42804", construct+" input must be character, bytea, json, or jsonb", position)
		}
		var err error
		input, err = b.coerce(s, input, b.primitive(25), ir.ImplicitCast)
		if err != nil {
			return expression{}, 0, err
		}
	}
	return input, format, nil
}

func (b *binder) jsonFormattedValue(value *zpgparse.JsonValueExpr, s *scope, position int) (expression, error) {
	input, format, encoding, err := b.jsonValueInput(value, s, position)
	if err != nil {
		return expression{}, err
	}
	if format == ir.SQLJSONDefaultFormat && encoding == ir.SQLJSONDefaultEncoding {
		return input, nil
	}
	payload := sqlJSONPayload(ir.SQLJSONParse)
	payload.JSONFormat = format
	payload.JSONEncoding = encoding
	return b.addSQLJSON(s, b.primitive(114), input.nullable, position, payload, []expression{input})
}

func (b *binder) sqlJSONOutput(output *zpgparse.JsonOutput, fallback model.TypeID, position int) (model.TypeLayout, ir.SQLJSONFormat, ir.SQLJSONEncoding, error) {
	target := b.primitive(fallback)
	format := ir.SQLJSONDefaultFormat
	encoding := ir.SQLJSONDefaultEncoding
	if output == nil {
		return target, format, encoding, nil
	}
	var err error
	returning := output.Returning
	if returning != nil {
		format, encoding, err = sqlJSONFormat(returning.Format)
		if err != nil {
			return model.TypeLayout{}, 0, 0, err
		}
	}
	if output.TypeName != nil {
		target, err = b.typeName(output.TypeName)
		if err != nil {
			return model.TypeLayout{}, 0, 0, err
		}
	} else if returning != nil && returning.Typid != 0 {
		var ok bool
		target, ok = b.typeLayoutByID(model.TypeID(returning.Typid))
		if !ok {
			return model.TypeLayout{}, 0, 0, missing("physical layout for JSON RETURNING type", position)
		}
		target.Type.Modifier = returning.Typmod
	}
	if encoding != ir.SQLJSONDefaultEncoding && target.Type.ID != 17 {
		return model.TypeLayout{}, 0, 0, bindError("22023", "JSON encoding is only allowed when RETURNING bytea", position)
	}
	return target, format, encoding, nil
}

func (b *binder) sqlJSONDocumentOutput(target model.TypeLayout) bool {
	return target.Type.ID == 17 || target.Type.ID == 114 || target.Type.ID == 3802 || b.typeCategory(target) == 'S'
}


func (b *binder) jsonParse(n *zpgparse.JsonParseExpr, s *scope) (expression, error) {
	position := int(n.Location) + 1
	input, format, encoding, err := b.jsonValueInput(n.Expr, s, position)
	if err != nil {
		return expression{}, err
	}
	input, format, err = b.sqlJSONDocumentInput(input, format, s, position, "JSON()")
	if err != nil {
		return expression{}, err
	}
	if n.UniqueKeys && input.typ.Type.ID != 25 && input.typ.Type.ID != 17 {
		return expression{}, bindError("42804", "WITH UNIQUE KEYS requires character or bytea input", position)
	}
	target, outputFormat, outputEncoding, err := b.sqlJSONOutput(n.Output, 114, position)
	if err != nil {
		return expression{}, err
	}
	if target.Type.ID != 114 && target.Type.ID != 3802 {
		return expression{}, bindError("42804", "JSON() RETURNING type must be json or jsonb", position)
	}
	payload := sqlJSONPayload(ir.SQLJSONParse)
	payload.JSONFormat, payload.JSONEncoding = format, encoding
	payload.JSONOutputFormat, payload.JSONOutputEncoding = outputFormat, outputEncoding
	if n.UniqueKeys {
		payload.JSONFlags |= ir.SQLJSONUniqueKeys
	}
	return b.addSQLJSON(s, target, input.nullable, position, payload, []expression{input})
}

func (b *binder) jsonScalar(n *zpgparse.JsonScalarExpr, s *scope) (expression, error) {
	position := int(n.Location) + 1
	input, err := b.scalar(n.Expr, s)
	if err != nil {
		return expression{}, err
	}
	input, err = b.defaultType(s, input)
	if err != nil {
		return expression{}, err
	}
	if input.typ.Type.Storage == model.TypeArray || input.typ.Type.Storage == model.TypeComposite || input.typ.Type.ID == 114 || input.typ.Type.ID == 3802 {
		return expression{}, bindError("42804", "JSON_SCALAR argument must be a scalar SQL value", position)
	}
	target, format, encoding, err := b.sqlJSONOutput(n.Output, 114, position)
	if err != nil {
		return expression{}, err
	}
	if target.Type.ID != 114 && target.Type.ID != 3802 {
		return expression{}, bindError("42804", "JSON_SCALAR RETURNING type must be json or jsonb", position)
	}
	payload := sqlJSONPayload(ir.SQLJSONScalar)
	payload.JSONOutputFormat, payload.JSONOutputEncoding = format, encoding
	return b.addSQLJSON(s, target, false, position, payload, []expression{input})
}
func (b *binder) jsonSerialize(n *zpgparse.JsonSerializeExpr, s *scope) (expression, error) {
	position := int(n.Location) + 1
	input, format, encoding, err := b.jsonValueInput(n.Expr, s, position)
	if err != nil {
		return expression{}, err
	}
	input, format, err = b.sqlJSONDocumentInput(input, format, s, position, "JSON_SERIALIZE()")
	if err != nil {
		return expression{}, err
	}
	target, outputFormat, outputEncoding, err := b.sqlJSONOutput(n.Output, 25, position)
	if err != nil {
		return expression{}, err
	}
	if target.Type.ID != 17 && b.typeCategory(target) != 'S' {
		return expression{}, bindError("42804", "JSON_SERIALIZE RETURNING type must be character or bytea", position)
	}
	payload := sqlJSONPayload(ir.SQLJSONSerialize)
	payload.JSONFormat, payload.JSONEncoding = format, encoding
	payload.JSONOutputFormat, payload.JSONOutputEncoding = outputFormat, outputEncoding
	return b.addSQLJSON(s, target, input.nullable, position, payload, []expression{input})
}

func (b *binder) jsonArrayConstructor(n *zpgparse.JsonArrayConstructor, s *scope) (expression, error) {
	position := int(n.Location) + 1
	args := make([]expression, len(n.Exprs))
	for i, node := range n.Exprs {
		var err error
		args[i], err = b.jsonFormattedValue(node.GetJsonValueExpr(), s, position)
		if err != nil {
			return expression{}, err
		}
	}
	target, format, encoding, err := b.sqlJSONOutput(n.Output, 114, position)
	if err != nil {
		return expression{}, err
	}
	if !b.sqlJSONDocumentOutput(target) {
		return expression{}, bindError("42804", "JSON_ARRAY RETURNING type must be json, jsonb, character, or bytea", position)
	}
	payload := sqlJSONPayload(ir.SQLJSONArray)
	payload.JSONOutputFormat, payload.JSONOutputEncoding = format, encoding
	if n.AbsentOnNull {
		payload.JSONFlags |= ir.SQLJSONAbsentOnNull
	}
	return b.addSQLJSON(s, target, false, position, payload, args)
}

func (b *binder) jsonObjectConstructor(n *zpgparse.JsonObjectConstructor, s *scope) (expression, error) {
	position := int(n.Location) + 1
	args := make([]expression, 0, len(n.Exprs)*2)
	for _, node := range n.Exprs {
		pair := node.GetJsonKeyValue()
		if pair == nil {
			return expression{}, bindError("42601", "invalid JSON_OBJECT member", position)
		}
		key, err := b.scalar(pair.Key, s)
		if err != nil {
			return expression{}, err
		}
		key, err = b.coerce(s, key, b.primitive(25), ir.AssignmentCast)
		if err != nil {
			return expression{}, err
		}
		value, err := b.jsonFormattedValue(pair.Value, s, position)
		if err != nil {
			return expression{}, err
		}
		args = append(args, key, value)
	}
	target, format, encoding, err := b.sqlJSONOutput(n.Output, 114, position)
	if err != nil {
		return expression{}, err
	}
	if !b.sqlJSONDocumentOutput(target) {
		return expression{}, bindError("42804", "JSON_OBJECT RETURNING type must be json, jsonb, character, or bytea", position)
	}
	payload := sqlJSONPayload(ir.SQLJSONObject)
	payload.JSONOutputFormat, payload.JSONOutputEncoding = format, encoding
	if n.AbsentOnNull {
		payload.JSONFlags |= ir.SQLJSONAbsentOnNull
	}
	if n.Unique {
		payload.JSONFlags |= ir.SQLJSONUniqueKeys
	}
	return b.addSQLJSON(s, target, false, position, payload, args)
}

func (b *binder) jsonArrayQueryConstructor(n *zpgparse.JsonArrayQueryConstructor, s *scope) (expression, error) {
	position := int(n.Location) + 1
	if n.Query == nil || n.Query.GetSelectStmt() == nil {
		return expression{}, bindError("42601", "JSON_ARRAY query must be SELECT", position)
	}
	target, outputFormat, outputEncoding, err := b.sqlJSONOutput(n.Output, 114, position)
	if err != nil {
		return expression{}, err
	}
	if !b.sqlJSONDocumentOutput(target) {
		return expression{}, bindError("42804", "JSON_ARRAY RETURNING type must be json, jsonb, character, or bytea", position)
	}
	child, err := b.newScope(s)
	if err != nil {
		return expression{}, err
	}
	input, err := b.selectStatement(n.Query.GetSelectStmt(), child, nil)
	if err != nil {
		return expression{}, err
	}
	if len(input.columns) != 1 {
		return expression{}, bindError("42601", "JSON_ARRAY query must return only one column", position)
	}
	value, err := b.reference(child, input.columns[0], position)
	if err != nil {
		return expression{}, err
	}
	format, encoding, err := sqlJSONFormat(n.Format)
	if err != nil {
		return expression{}, err
	}
	if encoding != ir.SQLJSONDefaultEncoding && value.typ.Type.ID != 17 {
		return expression{}, bindError("22023", "JSON encoding is only allowed for bytea input", position)
	}
	if format != ir.SQLJSONDefaultFormat || encoding != ir.SQLJSONDefaultEncoding {
		payload := sqlJSONPayload(ir.SQLJSONParse)
		payload.JSONFormat, payload.JSONEncoding = format, encoding
		value, err = b.addSQLJSON(child, b.primitive(114), value.nullable, position, payload, []expression{value})
		if err != nil {
			return expression{}, err
		}
	}
	function := model.FunctionID(3175)
	if n.AbsentOnNull {
		function = 6276
	}
	if target.Type.ID == 3802 {
		function = 3267
		if n.AbsentOnNull {
			function = 6284
		}
	}
	signature, ok := builtin.LookupSignature(function)
	if !ok {
		return expression{}, bindError("XX000", "SQL/JSON aggregate signature is unavailable", position)
	}
	result, err := b.functionResult(signature, []expression{value})
	if err != nil {
		return expression{}, err
	}
	arguments, err := b.functionArguments(signature, []expression{value}, position)
	if err != nil {
		return expression{}, err
	}
	value, err = b.coerce(child, value, arguments[0], ir.ImplicitCast)
	if err != nil {
		return expression{}, err
	}
	collation, err := b.commonCollation([]expression{value})
	if err != nil {
		return expression{}, err
	}
	result, resultState := b.resultCollation(result, collation)
	resolvedSignature, err := b.signature(signature, 0, []model.TypeLayout{value.typ}, result, collation.id, false)
	if err != nil {
		return expression{}, err
	}
	slot, err := b.slot(child, result, true, 0, 0)
	if err != nil {
		return expression{}, err
	}
	callEffects := value.effects.Union(signatureEffects(signature))
	effects := input.effects.Union(callEffects)
	call := ir.AggregateSpec{AggregateCall: ir.AggregateCall{Signature: resolvedSignature, Function: function, Result: slot, Effects: callEffects}, Args: []ir.ScalarID{value.id}}
	aggregateID, err := b.builder.AddRel(b.ctx, ir.RelSpec{
		Rel:          ir.Rel{Kind: ir.Aggregate, Scope: child.id, Effects: effects},
		Inputs:       []ir.RelID{input.id},
		Outputs:      []model.SlotID{slot},
		GroupingSets: [][]uint32{nil},
		Aggregates:   []ir.AggregateSpec{call},
	})
	if err != nil {
		return expression{}, err
	}
	aggregate := expression{typ: result, nullable: true, effects: effects.Union(ir.Effects{MayError: true}), position: position, reads: b.captures[child.id], collation: resultState}
	aggregate, err = b.addScalar(s, aggregate, ir.Scalar{Kind: ir.Subquery, Relation: aggregateID, SubqueryKind: ir.ScalarSubquery}, nil)
	if err != nil {
		return expression{}, err
	}
	empty, err := b.addScalar(s, expression{typ: result, value: model.Value{Type: result.Type.Storage, StrVal: "[]"}, position: position}, ir.Scalar{Kind: ir.Const}, nil)
	if err != nil {
		return expression{}, err
	}
	aggregate, err = b.addScalar(s, expression{typ: result, effects: aggregate.effects, position: position}, ir.Scalar{Kind: ir.Coalesce}, []expression{aggregate, empty})
	if err != nil {
		return expression{}, err
	}
	return b.convertSQLJSONAggregate(aggregate, target, outputFormat, outputEncoding, s, position)
}

func (b *binder) jsonIsPredicate(n *zpgparse.JsonIsPredicate, s *scope) (expression, error) {
	position := int(n.Location) + 1
	input, err := b.scalar(n.Expr, s)
	if err != nil {
		return expression{}, err
	}
	input, err = b.defaultType(s, input)
	if err != nil {
		return expression{}, err
	}
	format, encoding, err := sqlJSONFormat(n.Format)
	if err != nil {
		return expression{}, err
	}
	if encoding != ir.SQLJSONDefaultEncoding && input.typ.Type.ID != 17 {
		return expression{}, bindError("0A000", "JSON FORMAT ENCODING requires bytea input", position)
	}
	switch input.typ.Type.ID {
	case 17:
		if format == ir.SQLJSONDefaultFormat {
			format = ir.SQLJSONTextFormat
		}
	case 114, 3802:
	default:
		if b.typeCategory(input.typ) != 'S' {
			return expression{}, bindError("42804", "IS JSON input must be character, bytea, json, or jsonb", position)
		}
		input, err = b.coerce(s, input, b.primitive(25), ir.ImplicitCast)
		if err != nil {
			return expression{}, err
		}
	}
	itemType, err := sqlJSONItemType(n.ItemType)
	if err != nil {
		return expression{}, err
	}
	payload := sqlJSONPayload(ir.SQLJSONIs)
	payload.JSONFormat, payload.JSONEncoding = format, encoding
	payload.JSONItemType = itemType
	if n.UniqueKeys {
		payload.JSONFlags |= ir.SQLJSONUniqueKeys
	}
	return b.addSQLJSON(s, b.primitive(16), input.nullable, position, payload, []expression{input})
}

func (b *binder) jsonPathArguments(context *zpgparse.JsonValueExpr, path *zpgparse.Node, passing []*zpgparse.Node, s *scope, position int) ([]expression, ir.SQLJSONFormat, ir.SQLJSONEncoding, error) {
	input, format, encoding, err := b.jsonValueInput(context, s, position)
	if err != nil {
		return nil, 0, 0, err
	}
	input, format, err = b.sqlJSONDocumentInput(input, format, s, position, "SQL/JSON")
	if err != nil {
		return nil, 0, 0, err
	}
	pathExpr, err := b.scalar(path, s)
	if err != nil {
		return nil, 0, 0, err
	}
	pathExpr, err = b.coerce(s, pathExpr, b.primitive(4072), ir.AssignmentCast)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(passing) > math.MaxUint16 || uint64(len(passing)) > uint64(b.options.Limits.MaxOperands) {
		return nil, 0, 0, bindError("54000", "JSON PASSING operand limit exceeded", position)
	}
	args := []expression{input, pathExpr}
	names := make(map[string]struct{}, len(passing))
	for _, node := range passing {
		argument := node.GetJsonArgument()
		if argument == nil || argument.Val == nil {
			return nil, 0, 0, bindError("42601", "invalid JSON PASSING argument", position)
		}
		name := argument.Name
		if name == "" {
			return nil, 0, 0, bindError("42601", "JSON PASSING argument requires a name", position)
		}
		if _, duplicate := names[name]; duplicate {
			return nil, 0, 0, bindError("42710", "duplicate JSON PASSING variable", position)
		}
		names[name] = struct{}{}
		value, valueFormat, valueEncoding, err := b.jsonValueInput(argument.Val, s, position)
		if err != nil {
			return nil, 0, 0, err
		}
		nameExpr, err := b.addScalar(s, expression{typ: b.primitive(25), value: model.StringVal(name), position: position}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return nil, 0, 0, err
		}
		formatExpr, err := b.addScalar(s, expression{typ: b.primitive(23), value: model.Int64Val(int64(valueFormat)), position: position}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return nil, 0, 0, err
		}
		encodingExpr, err := b.addScalar(s, expression{typ: b.primitive(23), value: model.Int64Val(int64(valueEncoding)), position: position}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return nil, 0, 0, err
		}
		args = append(args, nameExpr, formatExpr, encodingExpr, value)
	}
	return args, format, encoding, nil
}

func (b *binder) sqlJSONPathVolatility(args []expression) (ir.Volatility, error) {
	if len(args) < 2 || args[1].typ.Type.ID != 4072 || args[1].value.IsNull {
		return ir.Immutable, nil
	}
	if args[1].value.Type != model.TypeBytes {
		return ir.Stable, nil
	}
	plan, err := builtin.DecodeJSONPathBinary(b.ctx, args[1].value.BytesVal, b.options.Limits.MaxBytes)
	if err != nil {
		return ir.Immutable, err
	}
	variables := make(map[string]model.TypeID, (len(args)-2)/4)
	for base := 2; base+3 < len(args); base += 4 {
		if args[base].value.Type == model.TypeString && !args[base].value.IsNull &&
			args[base+1].value.Type == model.TypeInt64 && args[base+1].value.IntVal == int64(ir.SQLJSONDefaultFormat) {
			variables[args[base].value.StrVal] = args[base+3].typ.Type.ID
		}
	}
	if plan.Mutable(variables) {
		return ir.Stable, nil
	}
	return ir.Immutable, nil
}

func (b *binder) jsonFunction(n *zpgparse.JsonFuncExpr, s *scope) (expression, error) {
	position := int(n.Location) + 1
	args, format, encoding, err := b.jsonPathArguments(n.ContextItem, n.Pathspec, n.Passing, s, position)
	if err != nil {
		return expression{}, err
	}
	volatility, err := b.sqlJSONPathVolatility(args)
	if err != nil {
		return expression{}, err
	}
	var operation ir.SQLJSONOperation
	var target model.TypeLayout
	var fallback model.TypeID
	switch n.Op {
	case zpgparse.JsonExprOp_JSON_EXISTS_OP:
		operation, target = ir.SQLJSONExists, b.primitive(16)
	case zpgparse.JsonExprOp_JSON_VALUE_OP:
		operation, fallback = ir.SQLJSONValue, 25
	case zpgparse.JsonExprOp_JSON_QUERY_OP:
		operation, fallback = ir.SQLJSONQuery, 3802
	default:
		return expression{}, bindError("42601", "invalid SQL/JSON function", position)
	}
	outputFormat, outputEncoding := ir.SQLJSONDefaultFormat, ir.SQLJSONDefaultEncoding
	if fallback != 0 {
		target, outputFormat, outputEncoding, err = b.sqlJSONOutput(n.Output, fallback, position)
		if err != nil {
			return expression{}, err
		}
		if operation == ir.SQLJSONValue && (target.Type.Storage == model.TypeArray || target.Type.Storage == model.TypeComposite) {
			return expression{}, bindError("42804", "JSON_VALUE RETURNING type must be scalar", position)
		}
	}
	wrapper, err := sqlJSONWrapper(n.Wrapper)
	if err != nil {
		return expression{}, err
	}
	quotes, err := sqlJSONQuotes(n.Quotes)
	if err != nil {
		return expression{}, err
	}
	if operation == ir.SQLJSONQuery && quotes == ir.SQLJSONOmitQuotes &&
		(wrapper == ir.SQLJSONConditionalWrapper || wrapper == ir.SQLJSONUnconditionalWrapper) {
		return expression{}, bindError("42601", "OMIT QUOTES cannot be used with JSON_QUERY WITH WRAPPER", position)
	}
	payload := sqlJSONPayload(operation)
	payload.JSONFormat, payload.JSONEncoding = format, encoding
	payload.JSONOutputFormat, payload.JSONOutputEncoding = outputFormat, outputEncoding
	payload.JSONWrapper, payload.JSONQuotes = wrapper, quotes
	payload.JSONPassing = uint16(len(n.Passing))
	captureEmpty := operation != ir.SQLJSONExists && (n.OnEmpty == nil || n.OnEmpty.Btype != zpgparse.JsonBehaviorType_JSON_BEHAVIOR_ERROR)
	captureError := n.OnError == nil || n.OnError.Btype != zpgparse.JsonBehaviorType_JSON_BEHAVIOR_ERROR
	if captureEmpty {
		payload.JSONFlags |= ir.SQLJSONCaptureEmpty
	}
	if captureError {
		payload.JSONFlags |= ir.SQLJSONCaptureError
	}
	payload.JSONFlags |= ir.SQLJSONUseTZ
	statusType := b.primitive(2249)
	statusType.Children = []model.TypeLayout{b.primitive(23), target}
	core, err := b.addScalar(s, expression{typ: statusType, effects: ir.Effects{MayError: true, Volatility: volatility}, position: position}, payload, args)
	if err != nil {
		return expression{}, err
	}
	core, err = b.once(s, core)
	if err != nil {
		return expression{}, err
	}
	status, err := b.compositeFieldAt(s, core, 0, position)
	if err != nil {
		return expression{}, err
	}
	result, err := b.compositeFieldAt(s, core, 1, position)
	if err != nil {
		return expression{}, err
	}
	if captureEmpty {
		branch, err := b.sqlJSONBehavior(n.OnEmpty, operation, target, s, position, false)
		if err != nil {
			return expression{}, err
		}
		result, err = b.sqlJSONStatusCase(status, 1, branch, result, s, position)
		if err != nil {
			return expression{}, err
		}
	}
	if captureError {
		branch, err := b.sqlJSONBehavior(n.OnError, operation, target, s, position, true)
		if err != nil {
			return expression{}, err
		}
		result, err = b.sqlJSONStatusCase(status, 2, branch, result, s, position)
		if err != nil {
			return expression{}, err
		}
	}
	return result, nil
}

func (b *binder) sqlJSONStatusCase(status expression, code int64, branch, otherwise expression, s *scope, position int) (expression, error) {
	constant, err := b.addScalar(s, expression{typ: b.primitive(23), value: model.Int64Val(code), position: position}, ir.Scalar{Kind: ir.Const}, nil)
	if err != nil {
		return expression{}, err
	}
	condition, err := b.operator(s, "=", []expression{status, constant}, position)
	if err != nil {
		return expression{}, err
	}
	return b.addScalar(s, expression{typ: otherwise.typ, nullable: branch.nullable || otherwise.nullable, position: position}, ir.Scalar{Kind: ir.Case}, []expression{condition, branch, otherwise})
}

func (b *binder) sqlJSONBehavior(behavior *zpgparse.JsonBehavior, operation ir.SQLJSONOperation, target model.TypeLayout, s *scope, position int, onError bool) (expression, error) {
	kind := zpgparse.JsonBehaviorType_JSON_BEHAVIOR_NULL
	var expressionNode *zpgparse.Node
	if behavior != nil {
		kind = behavior.Btype
		expressionNode = behavior.Expr
	}
	if operation == ir.SQLJSONExists && behavior == nil {
		kind = zpgparse.JsonBehaviorType_JSON_BEHAVIOR_FALSE
	}
	switch kind {
	case zpgparse.JsonBehaviorType_JSON_BEHAVIOR_NULL:
		if operation == ir.SQLJSONExists {
			return expression{}, bindError("42601", "NULL behavior is not valid for JSON_EXISTS", position)
		}
		return b.addScalar(s, expression{typ: target, nullable: true, value: model.NullVal(target.Type.Storage), position: position}, ir.Scalar{Kind: ir.Const}, nil)
	case zpgparse.JsonBehaviorType_JSON_BEHAVIOR_UNKNOWN:
		if operation != ir.SQLJSONExists || !onError {
			return expression{}, bindError("42601", "UNKNOWN behavior is only valid for JSON_EXISTS ON ERROR", position)
		}
		return b.addScalar(s, expression{typ: target, nullable: true, value: model.NullVal(target.Type.Storage), position: position}, ir.Scalar{Kind: ir.Const}, nil)
	case zpgparse.JsonBehaviorType_JSON_BEHAVIOR_TRUE, zpgparse.JsonBehaviorType_JSON_BEHAVIOR_FALSE:
		if operation != ir.SQLJSONExists || !onError {
			return expression{}, bindError("42601", "TRUE or FALSE behavior is only valid for JSON_EXISTS ON ERROR", position)
		}
		value, err := b.addScalar(s, expression{typ: b.primitive(16), value: model.BoolVal(kind == zpgparse.JsonBehaviorType_JSON_BEHAVIOR_TRUE), position: position}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil || target.Type.ID == 16 {
			return value, err
		}
		return b.coerce(s, value, target, ir.AssignmentCast)
	case zpgparse.JsonBehaviorType_JSON_BEHAVIOR_EMPTY, zpgparse.JsonBehaviorType_JSON_BEHAVIOR_EMPTY_ARRAY, zpgparse.JsonBehaviorType_JSON_BEHAVIOR_EMPTY_OBJECT:
		if operation != ir.SQLJSONQuery {
			return expression{}, bindError("42601", "EMPTY behavior is only valid for JSON_QUERY", position)
		}
		text := "[]"
		if kind == zpgparse.JsonBehaviorType_JSON_BEHAVIOR_EMPTY_OBJECT {
			text = "{}"
		}
		literal, err := b.addScalar(s, expression{typ: b.primitive(25), value: model.StringVal(text), position: position}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return expression{}, err
		}
		payload := sqlJSONPayload(ir.SQLJSONParse)
		return b.addSQLJSON(s, target, false, position, payload, []expression{literal})
	case zpgparse.JsonBehaviorType_JSON_BEHAVIOR_DEFAULT:
		if operation == ir.SQLJSONExists {
			return expression{}, bindError("42601", "DEFAULT behavior is not valid for JSON_EXISTS", position)
		}
		if expressionNode == nil {
			return expression{}, bindError("42601", "DEFAULT behavior requires an expression", position)
		}
		value, err := b.scalar(expressionNode, s)
		if err != nil {
			return expression{}, err
		}
		return b.coerce(s, value, target, ir.AssignmentCast)
	case zpgparse.JsonBehaviorType_JSON_BEHAVIOR_ERROR:
		return expression{}, bindError("XX000", "uncaptured SQL/JSON error behavior reached lowering", position)
	default:
		return expression{}, bindError("42601", "invalid SQL/JSON behavior", position)
	}
}

func (b *binder) jsonArrayAggregate(n *zpgparse.JsonArrayAgg, s *scope) (expression, error) {
	if n.Constructor == nil || n.Arg == nil {
		return expression{}, bindError("42601", "invalid JSON_ARRAYAGG", 0)
	}
	position := int(n.Constructor.Location) + 1
	argument, err := b.bindSQLJSONAggregateArgument(n.Arg, s, position)
	if err != nil {
		return expression{}, err
	}
	target, format, encoding, err := b.sqlJSONOutput(n.Constructor.Output, 114, position)
	if err != nil {
		return expression{}, err
	}
	if !b.sqlJSONDocumentOutput(target) {
		return expression{}, bindError("42804", "JSON_ARRAYAGG RETURNING type must be json, jsonb, character, or bytea", position)
	}
	jsonb := target.Type.ID == 3802
	function := model.FunctionID(3175)
	if n.AbsentOnNull {
		function = 6276
	}
	if jsonb {
		function = 3267
		if n.AbsentOnNull {
			function = 6284
		}
	}
	result, err := b.invokeSQLJSONAggregate(n.Constructor, s, function, []expression{argument})
	if err != nil {
		return expression{}, err
	}
	return b.convertSQLJSONAggregate(result, target, format, encoding, s, position)
}

func (b *binder) jsonObjectAggregate(n *zpgparse.JsonObjectAgg, s *scope) (expression, error) {
	if n.Constructor == nil || n.Arg == nil {
		return expression{}, bindError("42601", "invalid JSON_OBJECTAGG", 0)
	}
	position := int(n.Constructor.Location) + 1
	if s.inAggregate {
		return expression{}, bindError("42803", "aggregate calls cannot be nested", position)
	}
	previousAggregate, previousAllowWindow := s.inAggregate, s.allowWindow
	s.inAggregate, s.allowWindow = true, false
	key, err := b.scalar(n.Arg.Key, s)
	if err == nil {
		key, err = b.coerce(s, key, b.primitive(25), ir.AssignmentCast)
	}
	var value expression
	if err == nil {
		value, err = b.jsonFormattedValue(n.Arg.Value, s, position)
	}
	s.inAggregate, s.allowWindow = previousAggregate, previousAllowWindow
	if err != nil {
		return expression{}, err
	}
	target, format, encoding, err := b.sqlJSONOutput(n.Constructor.Output, 114, position)
	if err != nil {
		return expression{}, err
	}
	if !b.sqlJSONDocumentOutput(target) {
		return expression{}, bindError("42804", "JSON_OBJECTAGG RETURNING type must be json, jsonb, character, or bytea", position)
	}
	jsonb := target.Type.ID == 3802
	function := model.FunctionID(3197)
	switch {
	case jsonb && n.Unique && n.AbsentOnNull:
		function = 6290
	case jsonb && n.Unique:
		function = 6289
	case jsonb && n.AbsentOnNull:
		function = 6288
	case jsonb:
		function = 3270
	case n.Unique && n.AbsentOnNull:
		function = 6282
	case n.Unique:
		function = 6281
	case n.AbsentOnNull:
		function = 6280
	}
	result, err := b.invokeSQLJSONAggregate(n.Constructor, s, function, []expression{key, value})
	if err != nil {
		return expression{}, err
	}
	return b.convertSQLJSONAggregate(result, target, format, encoding, s, position)
}

func (b *binder) bindSQLJSONAggregateArgument(value *zpgparse.JsonValueExpr, s *scope, position int) (expression, error) {
	if s.inAggregate {
		return expression{}, bindError("42803", "aggregate calls cannot be nested", position)
	}
	previousAggregate, previousAllowWindow := s.inAggregate, s.allowWindow
	s.inAggregate, s.allowWindow = true, false
	argument, err := b.jsonFormattedValue(value, s, position)
	s.inAggregate, s.allowWindow = previousAggregate, previousAllowWindow
	return argument, err
}

func (b *binder) invokeSQLJSONAggregate(n *zpgparse.JsonAggConstructor, s *scope, function model.FunctionID, args []expression) (expression, error) {
	sig, ok := builtin.LookupSignature(function)
	if !ok {
		return expression{}, bindError("XX000", "SQL/JSON aggregate signature is unavailable", int(n.Location)+1)
	}
	call := &zpgparse.FuncCall{Location: n.Location, AggFilter: n.AggFilter, AggOrder: n.AggOrder, Over: n.Over}
	if n.Over != nil {
		return b.windowFunction(call, s, sig, args)
	}
	return b.aggregateFunction(call, s, sig, args, 0)
}

func (b *binder) convertSQLJSONAggregate(input expression, target model.TypeLayout, format ir.SQLJSONFormat, encoding ir.SQLJSONEncoding, s *scope, position int) (expression, error) {
	if input.typ.Type.ID == target.Type.ID && target.Type.Modifier == input.typ.Type.Modifier && format == ir.SQLJSONDefaultFormat && encoding == ir.SQLJSONDefaultEncoding {
		return input, nil
	}
	payload := sqlJSONPayload(ir.SQLJSONSerialize)
	payload.JSONFormat = ir.SQLJSONBinaryFormat
	if input.typ.Type.ID == 114 {
		payload.JSONFormat = ir.SQLJSONTextFormat
	}
	payload.JSONOutputFormat, payload.JSONOutputEncoding = format, encoding
	return b.addSQLJSON(s, target, input.nullable, position, payload, []expression{input})
}
