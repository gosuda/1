package builtin

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type SQLJSONKind uint8

const (
	SQLJSONParseKind SQLJSONKind = iota + 1
	SQLJSONScalarKind
	SQLJSONSerializeKind
	SQLJSONArrayKind
	SQLJSONObjectKind
	SQLJSONIsKind
	SQLJSONExistsKind
	SQLJSONValueKind
	SQLJSONQueryKind
)

type SQLJSONInputFormat uint8

const (
	SQLJSONDefaultInput SQLJSONInputFormat = iota + 1
	SQLJSONTextInput
	SQLJSONBinaryInput
)

type SQLJSONEncoding uint8

const (
	SQLJSONDefaultEncoding SQLJSONEncoding = iota + 1
	SQLJSONUTF8
	SQLJSONUTF16
	SQLJSONUTF32
)

type SQLJSONWrapper uint8

const (
	SQLJSONUnspecifiedWrapper SQLJSONWrapper = iota + 1
	SQLJSONWithoutWrapper
	SQLJSONConditionalWrapper
	SQLJSONUnconditionalWrapper
)

type SQLJSONQuotes uint8

const (
	SQLJSONUnspecifiedQuotes SQLJSONQuotes = iota + 1
	SQLJSONKeepQuotes
	SQLJSONOmitQuotes
)

type SQLJSONItemType uint8

const (
	SQLJSONAnyItem SQLJSONItemType = iota + 1
	SQLJSONObjectItem
	SQLJSONArrayItem
	SQLJSONScalarItem
)

type SQLJSONPlanSpec struct {
	Kind           SQLJSONKind
	InputFormat    SQLJSONInputFormat
	InputEncoding  SQLJSONEncoding
	OutputFormat   SQLJSONInputFormat
	OutputEncoding SQLJSONEncoding
	Wrapper        SQLJSONWrapper
	Quotes         SQLJSONQuotes
	ItemType       SQLJSONItemType
	Passing        int
	AbsentOnNull   bool
	UniqueKeys     bool
	CaptureEmpty   bool
	CaptureError   bool
	UseTZ          bool
}

type sqlJSONInputPlan struct {
	call      Callable
	arguments [3]model.Value
	count     int
}

type SQLJSONPlan struct {
	spec       SQLJSONPlanSpec
	arguments  []model.TypeLayout
	result     model.TypeLayout
	target     model.TypeLayout
	encoders   []*JSONValueEncoder
	input      sqlJSONInputPlan
	jsonInput  jsonRecordField
	decodeJSON bool
	options    ResolveOptions
	ownedBytes uint64
}

func (plan *SQLJSONPlan) OwnedBytes() uint64 {
	if plan == nil {
		return 0
	}
	return plan.ownedBytes
}

func ResolveSQLJSON(ctx context.Context, spec SQLJSONPlanSpec, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (*SQLJSONPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Kind < SQLJSONParseKind || spec.Kind > SQLJSONQueryKind || spec.InputFormat < SQLJSONDefaultInput || spec.InputFormat > SQLJSONBinaryInput || spec.OutputFormat < SQLJSONDefaultInput || spec.OutputFormat > SQLJSONBinaryInput || spec.InputEncoding < SQLJSONDefaultEncoding || spec.InputEncoding > SQLJSONUTF32 || spec.OutputEncoding < SQLJSONDefaultEncoding || spec.OutputEncoding > SQLJSONUTF32 || spec.Wrapper < SQLJSONUnspecifiedWrapper || spec.Wrapper > SQLJSONUnconditionalWrapper || spec.Quotes < SQLJSONUnspecifiedQuotes || spec.Quotes > SQLJSONOmitQuotes || spec.ItemType < SQLJSONAnyItem || spec.ItemType > SQLJSONScalarItem {
		return nil, sqlError("XX000", "invalid SQL/JSON plan metadata")
	}
	if spec.Passing < 0 || spec.Passing > MaxCallArguments || !validSQLJSONArity(spec, len(arguments)) {
		return nil, sqlError("XX000", "invalid SQL/JSON plan arity")
	}
	used, err := model.TypeLayoutBytes(ctx, result, maximum)
	if err != nil {
		return nil, codecLayoutError(err)
	}
	if used > maximum {
		return nil, sqlError("54000", "SQL/JSON metadata budget exceeded")
	}
	remaining := maximum - used
	ownedArguments := make([]model.TypeLayout, len(arguments))
	for i, layout := range arguments {
		bytes, err := model.TypeLayoutBytes(ctx, layout, remaining)
		if err != nil {
			return nil, codecLayoutError(err)
		}
		remaining -= bytes
		ownedArguments[i] = cloneSQLJSONLayout(layout)
		if err := validateConcreteLayout(ctx, layout); err != nil {
			return nil, err
		}
	}
	plan := &SQLJSONPlan{spec: spec, arguments: ownedArguments, result: cloneSQLJSONLayout(result), options: options}
	plan.target = plan.result
	if spec.Kind == SQLJSONExistsKind || spec.Kind == SQLJSONValueKind || spec.Kind == SQLJSONQueryKind {
		if result.Type.Storage != model.TypeComposite || len(result.Children) != 2 || result.Children[0].Type.ID != 23 || result.Children[0].Type.Storage != model.TypeInt64 {
			return nil, sqlError("XX000", "SQL/JSON path result status layout is invalid")
		}
		plan.target = result.Children[1]
	}
	plan.encoders = make([]*JSONValueEncoder, len(arguments))
	for _, index := range sqlJSONEncoderArguments(spec, len(arguments)) {
		encoder, err := ResolveJSONValue(ctx, arguments[index], options, remaining)
		if err != nil {
			return nil, err
		}
		if encoder.OwnedBytes() > remaining {
			return nil, sqlError("54000", "SQL/JSON metadata budget exceeded")
		}
		remaining -= encoder.OwnedBytes()
		plan.encoders[index] = encoder
	}
	plan.input, err = resolveSQLJSONInput(ctx, plan.target, options, remaining)
	if err != nil {
		return nil, err
	}
	if owned := plan.input.call.OwnedBytes(); owned > remaining {
		return nil, sqlError("54000", "SQL/JSON metadata budget exceeded")
	} else {
		remaining -= owned
	}
	if plan.target.Type.Storage == model.TypeArray || plan.target.Type.Storage == model.TypeComposite {
		plan.decodeJSON = true
		if err := plan.jsonInput.resolve(ctx, plan.target, options, &remaining); err != nil {
			return nil, err
		}
	}
	plan.ownedBytes = maximum - remaining + uint64(unsafe.Sizeof(*plan)) + uint64(len(arguments))*uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(len(plan.encoders))*uint64(unsafe.Sizeof((*JSONValueEncoder)(nil)))
	if plan.ownedBytes > maximum {
		return nil, sqlError("54000", "SQL/JSON metadata budget exceeded")
	}
	return plan, nil
}

func validSQLJSONArity(spec SQLJSONPlanSpec, count int) bool {
	switch spec.Kind {
	case SQLJSONParseKind, SQLJSONScalarKind, SQLJSONSerializeKind, SQLJSONIsKind:
		return count == 1 && spec.Passing == 0
	case SQLJSONArrayKind:
		return spec.Passing == 0
	case SQLJSONObjectKind:
		return count%2 == 0 && spec.Passing == 0
	case SQLJSONExistsKind, SQLJSONValueKind, SQLJSONQueryKind:
		return count == 2+4*spec.Passing
	default:
		return false
	}
}

func sqlJSONEncoderArguments(spec SQLJSONPlanSpec, count int) []int {
	indexes := make([]int, 0, count)
	switch spec.Kind {
	case SQLJSONScalarKind, SQLJSONArrayKind:
		for i := range count {
			indexes = append(indexes, i)
		}
	case SQLJSONObjectKind:
		for i := range count {
			indexes = append(indexes, i)
		}
	case SQLJSONParseKind, SQLJSONSerializeKind, SQLJSONIsKind, SQLJSONExistsKind, SQLJSONValueKind, SQLJSONQueryKind:
		indexes = append(indexes, 0)
	}
	return indexes
}

func cloneSQLJSONLayout(layout model.TypeLayout) model.TypeLayout {
	result := model.TypeLayout{Type: layout.Type, Children: make([]model.TypeLayout, len(layout.Children))}
	result.FieldNames = append([]string(nil), layout.FieldNames...)
	for i, child := range layout.Children {
		result.Children[i] = cloneSQLJSONLayout(child)
	}
	return result
}

func resolveSQLJSONInput(ctx context.Context, target model.TypeLayout, options ResolveOptions, maximum uint64) (sqlJSONInputPlan, error) {
	if target.Type.Storage == model.TypeArray || target.Type.Storage == model.TypeComposite {
		return sqlJSONInputPlan{}, nil
	}
	if target.Type.ID == 114 || target.Type.ID == 3802 || target.Type.ID == 17 {
		return sqlJSONInputPlan{}, nil
	}
	info, ok := lookupResolvedTypeIO(options, target)
	if !ok || info.Input == 0 {
		return sqlJSONInputPlan{}, sqlError("42804", "SQL/JSON RETURNING type has no input function")
	}
	signature, ok := LookupSignature(info.Input)
	if !ok || signature.ArgumentCount < 1 || signature.ArgumentCount > 3 || signature.Arguments[0] != 2275 {
		return sqlJSONInputPlan{}, sqlError("42804", "SQL/JSON RETURNING type input function is invalid")
	}
	layouts := make([]model.TypeLayout, int(signature.ArgumentCount))
	layouts[0] = model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
	var values [3]model.Value
	for i := 1; i < int(signature.ArgumentCount); i++ {
		id := signature.Arguments[i]
		layouts[i] = model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: primitiveStorage(id)}}
		parameter := int64(info.Parameter)
		if i == 2 {
			parameter = int64(target.Type.Modifier)
		}
		values[i] = model.Int64Val(parameter)
	}
	call, err := ResolveTyped(ctx, info.Input, layouts, target, options, maximum)
	return sqlJSONInputPlan{call: call, arguments: values, count: int(signature.ArgumentCount)}, err
}

func (plan *SQLJSONPlan) Call(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
	if plan == nil || len(values) != len(plan.arguments) {
		return model.Value{}, sqlError("XX000", "invalid SQL/JSON invocation")
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	for i, value := range values {
		if value.Type != plan.arguments[i].Type.Storage {
			return model.Value{}, sqlError("42804", "SQL/JSON argument storage differs from its plan")
		}
	}
	switch plan.spec.Kind {
	case SQLJSONParseKind:
		return plan.parse(ctx, values[0], budget)
	case SQLJSONScalarKind:
		return plan.scalar(ctx, values[0], budget)
	case SQLJSONSerializeKind:
		return plan.serialize(ctx, values[0], budget)
	case SQLJSONArrayKind:
		return plan.array(ctx, values, budget)
	case SQLJSONObjectKind:
		return plan.object(ctx, values, budget)
	case SQLJSONIsKind:
		return plan.predicate(ctx, values[0], budget)
	case SQLJSONExistsKind, SQLJSONValueKind, SQLJSONQueryKind:
		return plan.path(ctx, values, budget)
	default:
		return model.Value{}, sqlError("XX000", "unknown SQL/JSON operation")
	}
}

func (plan *SQLJSONPlan) parse(ctx context.Context, value model.Value, budget CallBudget) (model.Value, error) {
	if value.IsNull {
		return model.NullVal(plan.target.Type.Storage), nil
	}
	text, err := plan.documentText(ctx, value, plan.arguments[0], budget)
	if err != nil {
		return model.Value{}, err
	}
	if plan.spec.UniqueKeys {
		if err := validateUniqueJSONKeys(ctx, text, budget.MemoryBytes); err != nil {
			return model.Value{}, err
		}
	} else if _, _, err := scanJSONText(ctx, text); err != nil {
		return model.Value{}, err
	}
	return plan.convertText(ctx, text, budget)
}

func (plan *SQLJSONPlan) scalar(ctx context.Context, value model.Value, budget CallBudget) (model.Value, error) {
	layout := plan.arguments[0]
	if layout.Type.Storage == model.TypeArray || layout.Type.Storage == model.TypeComposite || layout.Type.ID == 114 || layout.Type.ID == 3802 {
		return model.Value{}, sqlError("22023", "JSON_SCALAR requires an SQL scalar value")
	}
	encoded, err := plan.encoders[0].EncodeValue(ctx, value, JSONCompact, budget)
	if err != nil {
		return model.Value{}, err
	}
	return plan.convertText(ctx, encoded.StrVal, budget)
}

func (plan *SQLJSONPlan) serialize(ctx context.Context, value model.Value, budget CallBudget) (model.Value, error) {
	if value.IsNull {
		return model.NullVal(plan.target.Type.Storage), nil
	}
	text, err := plan.documentText(ctx, value, plan.arguments[0], budget)
	if err != nil {
		return model.Value{}, err
	}
	if _, _, err := scanJSONText(ctx, text); err != nil {
		return model.Value{}, err
	}
	return plan.convertText(ctx, text, budget)
}

func (plan *SQLJSONPlan) array(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
	var output strings.Builder
	output.WriteByte('[')
	written := 0
	for i, value := range values {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if value.IsNull && plan.spec.AbsentOnNull {
			continue
		}
		encoded, err := plan.encoders[i].EncodeValue(ctx, value, JSONCompact, budget)
		if err != nil {
			return model.Value{}, err
		}
		if written != 0 {
			output.WriteString(", ")
		}
		if err := appendSQLJSON(&output, encoded.StrVal, budget.OutputBytes); err != nil {
			return model.Value{}, err
		}
		written++
	}
	output.WriteByte(']')
	if int64(output.Len()) > budget.OutputBytes {
		return model.Value{}, sqlError("54000", "SQL/JSON output exceeds budget")
	}
	return plan.convertText(ctx, output.String(), budget)
}

func (plan *SQLJSONPlan) object(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
	var output strings.Builder
	output.WriteByte('{')
	seen := make(map[string]struct{}, len(values)/2)
	written := 0
	for i := 0; i < len(values); i += 2 {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if values[i].IsNull {
			return model.Value{}, sqlError("22023", "JSON object key must not be null")
		}
		if values[i+1].IsNull && plan.spec.AbsentOnNull {
			continue
		}
		key, err := plan.encoders[i].EncodeKey(ctx, values[i], budget)
		if err != nil {
			return model.Value{}, err
		}
		if plan.spec.UniqueKeys {
			if _, exists := seen[key.StrVal]; exists {
				return model.Value{}, sqlError("22037", "duplicate JSON object key value")
			}
			seen[key.StrVal] = struct{}{}
		}
		value, err := plan.encoders[i+1].EncodeValue(ctx, values[i+1], JSONCompact, budget)
		if err != nil {
			return model.Value{}, err
		}
		if written != 0 {
			output.WriteString(", ")
		}
		if err := appendSQLJSON(&output, key.StrVal+": "+value.StrVal, budget.OutputBytes); err != nil {
			return model.Value{}, err
		}
		written++
	}
	output.WriteByte('}')
	if int64(output.Len()) > budget.OutputBytes {
		return model.Value{}, sqlError("54000", "SQL/JSON output exceeds budget")
	}
	return plan.convertText(ctx, output.String(), budget)
}

func (plan *SQLJSONPlan) predicate(ctx context.Context, value model.Value, budget CallBudget) (model.Value, error) {
	if value.IsNull {
		return model.NullVal(model.TypeBoolean), nil
	}
	text, err := plan.documentText(ctx, value, plan.arguments[0], budget)
	if err != nil {
		if suppressibleSQLJSONError(err) {
			return model.BoolVal(false), nil
		}
		return model.Value{}, err
	}
	var shape jsonbShape
	if plan.spec.UniqueKeys {
		shape, err = inspectUniqueJSONKeys(ctx, text, budget.MemoryBytes)
	} else {
		shape, err = walkJSONB(ctx, text, nil)
	}
	if err != nil {
		if suppressibleSQLJSONError(err) {
			return model.BoolVal(false), nil
		}
		return model.Value{}, err
	}
	matches := plan.spec.ItemType == SQLJSONAnyItem || plan.spec.ItemType == SQLJSONObjectItem && shape.kind == jsonTextObject || plan.spec.ItemType == SQLJSONArrayItem && shape.kind == jsonTextArray || plan.spec.ItemType == SQLJSONScalarItem && shape.kind != jsonTextObject && shape.kind != jsonTextArray
	return model.BoolVal(matches), nil
}

func (plan *SQLJSONPlan) path(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
	targetNull := model.NullVal(plan.target.Type.Storage)
	status := func(code int64, value model.Value) model.Value {
		return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(code), value}}
	}
	if values[0].IsNull || values[1].IsNull {
		return status(0, targetNull), nil
	}
	document, err := plan.documentText(ctx, values[0], plan.arguments[0], budget)
	if err != nil {
		return plan.pathError(err, status, targetNull)
	}
	path, err := plan.pathPlan(ctx, values[1], plan.arguments[1], budget.MemoryBytes)
	if err != nil {
		return plan.pathError(err, status, targetNull)
	}
	passing := make([]JSONPathVariable, plan.spec.Passing)
	for i := range passing {
		base := 2 + i*4
		if values[base].IsNull || values[base].Type != model.TypeString || values[base+1].IsNull || values[base+2].IsNull {
			return model.Value{}, sqlError("XX000", "invalid SQL/JSON PASSING metadata")
		}
		format, encoding := SQLJSONInputFormat(values[base+1].IntVal), SQLJSONEncoding(values[base+2].IntVal)
		value, layout := values[base+3], plan.arguments[base+3]
		if format != SQLJSONDefaultInput {
			text, decodeErr := decodeSQLJSONDocument(value, layout, format, encoding)
			if decodeErr != nil {
				return plan.pathError(decodeErr, status, targetNull)
			}
			value = model.Value{Type: model.TypeJSONB, StrVal: text, IsNull: value.IsNull}
			layout = model.TypeLayout{Type: model.Type{ID: 114, Modifier: -1, Storage: model.TypeJSONB}}
		}
		passing[i] = JSONPathVariable{Name: values[base].StrVal, Layout: layout, Value: value}
	}
	var count int
	var first jsonPathItem
	var firstText string
	var queryOutput strings.Builder
	err = executeJSONPathTypedItems(ctx, document, path, "{}", passing, plan.spec.UseTZ, budget, plan.options, func(execution *jsonPathExecution, item jsonPathItem) error {
		count++
		if count == 1 {
			first = item
		}
		if plan.spec.Kind == SQLJSONExistsKind || plan.spec.Kind == SQLJSONValueKind && count > 1 {
			return errJSONPathStop
		}
		if plan.spec.Kind != SQLJSONQueryKind {
			return nil
		}
		text, renderErr := execution.render(item)
		if renderErr != nil {
			return renderErr
		}
		if count == 1 {
			firstText = text
			return nil
		}
		if count == 2 {
			queryOutput.WriteByte('[')
			if appendErr := appendSQLJSON(&queryOutput, firstText, budget.OutputBytes); appendErr != nil {
				return appendErr
			}
		}
		queryOutput.WriteString(", ")
		return appendSQLJSON(&queryOutput, text, budget.OutputBytes)
	})
	if errors.Is(err, errJSONPathStop) {
		err = nil
	}
	if err != nil {
		return plan.pathError(err, status, targetNull)
	}
	if plan.spec.Kind == SQLJSONExistsKind {
		value, convertErr := plan.convertBoolean(ctx, count != 0, budget)
		if convertErr != nil {
			return plan.pathError(convertErr, status, targetNull)
		}
		return status(0, value), nil
	}
	if count == 0 {
		if plan.spec.CaptureEmpty {
			return status(1, targetNull), nil
		}
		return model.Value{}, sqlError("22035", "SQL/JSON item is empty")
	}
	if plan.spec.Kind == SQLJSONValueKind {
		if count != 1 || first.kind == jsonTextObject || first.kind == jsonTextArray {
			return plan.pathError(sqlError("22038", "JSON_VALUE requires one scalar item"), status, targetNull)
		}
		value, convertErr := plan.convertItem(ctx, first, budget)
		if convertErr != nil {
			return plan.pathError(convertErr, status, targetNull)
		}
		return status(0, value), nil
	}
	text, queryErr := plan.queryResultText(first, count, firstText, &queryOutput, budget)
	if queryErr != nil {
		return plan.pathError(queryErr, status, targetNull)
	}
	value, convertErr := plan.convertQueryResult(ctx, first, count, firstText, text, budget)
	if convertErr != nil {
		return plan.pathError(convertErr, status, targetNull)
	}
	return status(0, value), nil
}

func (plan *SQLJSONPlan) pathFrom(execution *jsonPathExecution, path *JSONPathPlan, root jsonPathItem, budget CallBudget) (model.Value, error) {
	targetNull := model.NullVal(plan.target.Type.Storage)
	status := func(code int64, value model.Value) model.Value {
		return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(code), value}}
	}
	if execution == nil || path == nil || path.expression == nil {
		return model.Value{}, sqlError("22023", "invalid JSON_TABLE path execution")
	}
	previousRoot, previousPlan := execution.root, execution.plan
	execution.root, execution.plan = root, path
	defer func() {
		execution.root, execution.plan = previousRoot, previousPlan
	}()
	var count int
	var first jsonPathItem
	var firstText string
	var queryOutput strings.Builder
	err := execution.evaluate(path.expression, jsonPathItem{}, -1, func(item jsonPathItem) error {
		count++
		if count == 1 {
			first = item
		}
		if plan.spec.Kind == SQLJSONExistsKind || plan.spec.Kind == SQLJSONValueKind && count > 1 {
			return errJSONPathStop
		}
		if plan.spec.Kind != SQLJSONQueryKind {
			return nil
		}
		text, renderErr := execution.render(item)
		if renderErr != nil {
			return renderErr
		}
		if count == 1 {
			firstText = text
			return nil
		}
		if count == 2 {
			queryOutput.WriteByte('[')
			if appendErr := appendSQLJSON(&queryOutput, firstText, budget.OutputBytes); appendErr != nil {
				return appendErr
			}
		}
		queryOutput.WriteString(", ")
		return appendSQLJSON(&queryOutput, text, budget.OutputBytes)
	})
	if errors.Is(err, errJSONPathStop) {
		err = nil
	}
	if err != nil {
		return plan.pathError(err, status, targetNull)
	}
	if plan.spec.Kind == SQLJSONExistsKind {
		value, convertErr := plan.convertBoolean(execution.ctx, count != 0, budget)
		if convertErr != nil {
			return plan.pathError(convertErr, status, targetNull)
		}
		return status(0, value), nil
	}
	if count == 0 {
		if plan.spec.CaptureEmpty {
			return status(1, targetNull), nil
		}
		return model.Value{}, sqlError("22035", "SQL/JSON item is empty")
	}
	if plan.spec.Kind == SQLJSONValueKind {
		if count != 1 || first.kind == jsonTextObject || first.kind == jsonTextArray {
			return plan.pathError(sqlError("22038", "JSON_VALUE requires one scalar item"), status, targetNull)
		}
		value, convertErr := plan.convertItem(execution.ctx, first, budget)
		if convertErr != nil {
			return plan.pathError(convertErr, status, targetNull)
		}
		return status(0, value), nil
	}
	text, queryErr := plan.queryResultText(first, count, firstText, &queryOutput, budget)
	if queryErr != nil {
		return plan.pathError(queryErr, status, targetNull)
	}
	value, convertErr := plan.convertQueryResult(execution.ctx, first, count, firstText, text, budget)
	if convertErr != nil {
		return plan.pathError(convertErr, status, targetNull)
	}
	return status(0, value), nil
}

func (plan *SQLJSONPlan) queryResultText(first jsonPathItem, count int, firstText string, queryOutput *strings.Builder, budget CallBudget) (string, error) {
	wrapper := plan.spec.Wrapper
	if wrapper == SQLJSONUnspecifiedWrapper {
		wrapper = SQLJSONWithoutWrapper
	}
	if wrapper == SQLJSONWithoutWrapper {
		if count != 1 {
			return "", sqlError("22038", "JSON_QUERY without wrapper requires one item")
		}
		if first.kind == jsonTextString && plan.spec.Quotes == SQLJSONOmitQuotes {
			return first.scalarText(), nil
		}
		return firstText, nil
	}
	if wrapper == SQLJSONConditionalWrapper && count == 1 {
		return firstText, nil
	}
	if count == 1 {
		if int64(len(firstText)) > budget.OutputBytes-2 {
			return "", sqlError("54000", "SQL/JSON output exceeds budget")
		}
		return "[" + firstText + "]", nil
	}
	queryOutput.WriteByte(']')
	if int64(queryOutput.Len()) > budget.OutputBytes {
		return "", sqlError("54000", "SQL/JSON output exceeds budget")
	}
	return queryOutput.String(), nil
}

func (plan *SQLJSONPlan) convertBoolean(ctx context.Context, value bool, budget CallBudget) (model.Value, error) {
	if plan.target.Type.ID == 16 && plan.target.Type.Storage == model.TypeBoolean {
		return model.BoolVal(value), nil
	}
	if value {
		return plan.convertScalarText(ctx, "true", budget)
	}
	return plan.convertScalarText(ctx, "false", budget)
}


func (plan *SQLJSONPlan) pathError(err error, status func(int64, model.Value) model.Value, null model.Value) (model.Value, error) {
	if plan.spec.CaptureError && suppressibleSQLJSONError(err) {
		return status(2, null), nil
	}
	return model.Value{}, err
}

func suppressibleSQLJSONError(err error) bool {
	if isSuppressibleJSONPathError(err) {
		return true
	}
	var sql *model.Error
	if !errors.As(err, &sql) {
		return false
	}
	switch sql.Code {
	case "22003", "22007", "22008", "22012", "22021", "22023",
		"22030", "22031", "22032", "22033", "22034", "22035", "22036",
		"22037", "22038", "22039", "2203A", "2203B", "2203C", "2203D",
		"2203E", "2203F", "22P02", "22P05":
		return true
	}
	return false
}

func (plan *SQLJSONPlan) pathPlan(ctx context.Context, value model.Value, layout model.TypeLayout, maximum uint64) (*JSONPathPlan, error) {
	if layout.Type.ID == 4072 && value.Type == model.TypeBytes {
		return DecodeJSONPathBinary(ctx, value.BytesVal, maximum)
	}
	if value.Type == model.TypeString {
		return CompileJSONPath(ctx, value.StrVal, maximum)
	}
	return nil, sqlError("42804", "SQL/JSON path expression must be jsonpath")
}

func (plan *SQLJSONPlan) documentText(ctx context.Context, value model.Value, layout model.TypeLayout, budget CallBudget) (string, error) {
	if plan.spec.InputFormat != SQLJSONDefaultInput || layout.Type.Storage == model.TypeJSONB || layout.Type.Storage == model.TypeString || layout.Type.Storage == model.TypeBytes {
		return decodeSQLJSONDocument(value, layout, plan.spec.InputFormat, plan.spec.InputEncoding)
	}
	encoded, err := plan.encoders[0].EncodeValue(ctx, value, JSONCompact, budget)
	return encoded.StrVal, err
}

func decodeSQLJSONDocument(value model.Value, layout model.TypeLayout, format SQLJSONInputFormat, encoding SQLJSONEncoding) (string, error) {
	if value.IsNull {
		return "", nil
	}
	if layout.Type.Storage == model.TypeJSONB {
		return value.StrVal, nil
	}
	if layout.Type.Storage == model.TypeString {
		if encoding != SQLJSONDefaultEncoding && encoding != SQLJSONUTF8 {
			return "", sqlError("22023", "JSON encoding applies only to bytea input")
		}
		return value.StrVal, nil
	}
	if layout.Type.Storage != model.TypeBytes || format == SQLJSONDefaultInput {
		return "", sqlError("42804", "SQL/JSON document input is not character, bytea, json, or jsonb")
	}
	return decodeSQLJSONBytes(value.BytesVal, encoding)
}

func decodeSQLJSONBytes(input []byte, encoding SQLJSONEncoding) (string, error) {
	if encoding == SQLJSONDefaultEncoding || encoding == SQLJSONUTF8 {
		if !utf8.Valid(input) {
			return "", sqlError("22021", "invalid UTF-8 SQL/JSON input")
		}
		return string(input), nil
	}
	width := 2
	if encoding == SQLJSONUTF32 {
		width = 4
	}
	if len(input)%width != 0 {
		return "", sqlError("22021", "invalid encoded SQL/JSON input length")
	}
	little := false
	if len(input) >= width {
		if encoding == SQLJSONUTF16 && input[0] == 0xff && input[1] == 0xfe || encoding == SQLJSONUTF32 && input[0] == 0xff && input[1] == 0xfe && input[2] == 0 && input[3] == 0 {
			little = true
			input = input[width:]
		} else if encoding == SQLJSONUTF16 && input[0] == 0xfe && input[1] == 0xff || encoding == SQLJSONUTF32 && input[0] == 0 && input[1] == 0 && input[2] == 0xfe && input[3] == 0xff {
			input = input[width:]
		}
	}
	var order binary.ByteOrder = binary.BigEndian
	if little {
		order = binary.LittleEndian
	}
	var output strings.Builder
	output.Grow(len(input))
	if encoding == SQLJSONUTF16 {
		for offset := 0; offset < len(input); offset += 2 {
			unit := order.Uint16(input[offset:])
			switch {
			case unit >= 0xd800 && unit <= 0xdbff:
				if offset+4 > len(input) {
					return "", sqlError("22021", "invalid UTF-16 SQL/JSON input")
				}
				low := order.Uint16(input[offset+2:])
				if low < 0xdc00 || low > 0xdfff {
					return "", sqlError("22021", "invalid UTF-16 SQL/JSON input")
				}
				output.WriteRune(utf16.DecodeRune(rune(unit), rune(low)))
				offset += 2
			case unit >= 0xdc00 && unit <= 0xdfff:
				return "", sqlError("22021", "invalid UTF-16 SQL/JSON input")
			default:
				output.WriteRune(rune(unit))
			}
		}
	} else {
		for offset := 0; offset < len(input); offset += 4 {
			value := rune(order.Uint32(input[offset:]))
			if !utf8.ValidRune(value) {
				return "", sqlError("22021", "invalid UTF-32 SQL/JSON input")
			}
			output.WriteRune(value)
		}
	}
	return output.String(), nil
}

func (plan *SQLJSONPlan) convertItem(ctx context.Context, item jsonPathItem, budget CallBudget) (model.Value, error) {
	if item.kind == jsonTextNull {
		return model.NullVal(plan.target.Type.Storage), nil
	}
	text := item.scalarText()
	if item.isTemporal() {
		execution := jsonPathExecution{ctx: ctx, options: plan.options, budget: budget}
		var err error
		text, err = execution.render(item)
		if err != nil {
			return model.Value{}, err
		}
		text = strings.Trim(text, "\"")
	}
	if item.kind == jsonTextString && plan.target.Type.ID != 114 && plan.target.Type.ID != 3802 {
		return plan.convertScalarText(ctx, text, budget)
	}
	return plan.convertText(ctx, text, budget)
}

func (plan *SQLJSONPlan) convertScalarText(ctx context.Context, text string, budget CallBudget) (model.Value, error) {
	if plan.target.Type.ID == 17 {
		return model.BytesVal([]byte(text)), nil
	}
	if plan.input.count == 0 {
		return model.Value{}, sqlError("42804", "SQL/JSON scalar RETURNING type cannot accept text")
	}
	arguments := plan.input.arguments
	arguments[0] = model.StringVal(text)
	return plan.input.call.Call(ctx, arguments[:plan.input.count], budget)
}

func (plan *SQLJSONPlan) convertQueryResult(ctx context.Context, first jsonPathItem, count int, firstText, text string, budget CallBudget) (model.Value, error) {
	if !plan.decodeJSON {
		return plan.convertText(ctx, text, budget)
	}
	wrapper := plan.spec.Wrapper
	wrapped := wrapper == SQLJSONUnconditionalWrapper || wrapper == SQLJSONConditionalWrapper && count > 1
	if !wrapped && first.kind == jsonTextString && plan.spec.Quotes != SQLJSONOmitQuotes {
		return model.Value{}, sqlError("22P02", "quoted JSON string cannot be converted to the SQL/JSON RETURNING type")
	}
	if wrapped {
		return plan.jsonInput.convert(ctx, text, jsonTextArray, model.NullVal(plan.target.Type.Storage), budget)
	}
	return plan.jsonInput.convert(ctx, firstText, first.kind, model.NullVal(plan.target.Type.Storage), budget)
}

func (plan *SQLJSONPlan) convertText(ctx context.Context, text string, budget CallBudget) (model.Value, error) {
	if int64(len(text)) > budget.OutputBytes || uint64(len(text)) > budget.MemoryBytes {
		return model.Value{}, sqlError("54000", "SQL/JSON output exceeds budget")
	}
	if plan.decodeJSON {
		scanner := jsonTextScanner{text: text}
		span, err := scanJSONCollectionValue(ctx, &scanner)
		if err != nil {
			return model.Value{}, err
		}
		return plan.jsonInput.convert(ctx, text[span.start:span.end], span.kind, model.NullVal(plan.target.Type.Storage), budget)
	}
	switch plan.target.Type.ID {
	case 114:
		return model.Value{Type: model.TypeJSONB, StrVal: strings.Clone(text)}, nil
	case 3802:
		canonical, err := canonicalJSONB(ctx, text, int64(min(uint64(math.MaxInt64), budget.MemoryBytes)), false)
		if err != nil {
			return model.Value{}, err
		}
		if int64(len(canonical)) > budget.OutputBytes {
			return model.Value{}, sqlError("54000", "SQL/JSON output exceeds budget")
		}
		return model.Value{Type: model.TypeJSONB, StrVal: canonical}, nil
	case 17:
		encoded, err := encodeSQLJSONBytes(text, plan.spec.OutputEncoding, budget.OutputBytes)
		return model.BytesVal(encoded), err
	}
	return plan.convertScalarText(ctx, text, budget)
}

func encodeSQLJSONBytes(text string, encoding SQLJSONEncoding, limit int64) ([]byte, error) {
	if encoding == SQLJSONDefaultEncoding || encoding == SQLJSONUTF8 {
		if int64(len(text)) > limit {
			return nil, sqlError("54000", "SQL/JSON output exceeds budget")
		}
		return []byte(text), nil
	}
	var output []byte
	if encoding == SQLJSONUTF16 {
		units := utf16.Encode([]rune(text))
		if int64(len(units)) > limit/2 {
			return nil, sqlError("54000", "SQL/JSON output exceeds budget")
		}
		output = make([]byte, len(units)*2)
		for i, unit := range units {
			binary.BigEndian.PutUint16(output[i*2:], unit)
		}
	} else {
		runes := []rune(text)
		if int64(len(runes)) > limit/4 {
			return nil, sqlError("54000", "SQL/JSON output exceeds budget")
		}
		output = make([]byte, len(runes)*4)
		for i, value := range runes {
			binary.BigEndian.PutUint32(output[i*4:], uint32(value))
		}
	}
	return output, nil
}

func appendSQLJSON(output *strings.Builder, text string, limit int64) error {
	if int64(output.Len()) > limit-int64(len(text)) {
		return sqlError("54000", "SQL/JSON output exceeds budget")
	}
	output.WriteString(text)
	return nil
}

func validateUniqueJSONKeys(ctx context.Context, text string, budget uint64) error {
	_, err := inspectUniqueJSONKeys(ctx, text, budget)
	return err
}

func inspectUniqueJSONKeys(ctx context.Context, text string, budget uint64) (jsonbShape, error) {
	keys := make([]map[string]struct{}, jsonTextMaxDepth)
	used := uint64(len(keys)) * uint64(unsafe.Sizeof(map[string]struct{}{}))
	return walkJSONB(ctx, text, func(kind jsonTextKind, token string, key bool, depth int) error {
		if kind == 0 {
			return nil
		}
		if kind == jsonTextObject && !key {
			if depth+1 >= len(keys) {
				return sqlError("54001", "JSON nesting limit exceeded")
			}
			keys[depth+1] = make(map[string]struct{})
			return nil
		}
		if !key {
			return nil
		}
		var decoded strings.Builder
		if _, err := walkJSONDecodedString(ctx, token, func(value rune) error { decoded.WriteRune(value); return nil }); err != nil {
			return err
		}
		name := decoded.String()
		cost := uint64(len(name)) + uint64(unsafe.Sizeof(""))*2
		if used > budget || cost > budget-used {
			return sqlError("54000", "SQL/JSON unique-key check exceeds budget")
		}
		used += cost
		if _, duplicate := keys[depth][name]; duplicate {
			return sqlError("22037", "duplicate JSON object key value")
		}
		keys[depth][name] = struct{}{}
		return nil
	})
}

