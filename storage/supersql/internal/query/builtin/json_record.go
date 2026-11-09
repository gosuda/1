package builtin

import (
	"context"
	"errors"
	"math"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type jsonRecordField struct {
	typ       model.Type
	name      string
	parameter model.TypeID
	input     Callable
	children  []jsonRecordField
}

type jsonRecordPlan struct {
	root          jsonRecordField
	jsonArgument  int
	binary        bool
	set           bool
	valid         bool
	metadata      uint64
	argumentTypes []model.Type
}

func resolveJSONRecordScalar(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	plan, err := newJSONRecordPlan(ctx, signature.ID, arguments, result, options, maximum)
	if err != nil {
		return Callable{}, err
	}
	call := Callable{signature: signature, kernel: plan.call, resultStorage: result.Type.Storage, argumentTypes: plan.argumentTypes, metadataBytes: plan.metadata}
	call.signature.Result = result.Type.ID
	return call, nil
}

func ResolveJSONRecordRows(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (RowSourcePlan, error) {
	return newJSONRecordPlan(ctx, id, arguments, result, options, maximum)
}

func newJSONRecordPlan(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (*jsonRecordPlan, error) {
	signature, known := LookupSignature(id)
	if !known {
		return nil, &UnknownFunctionError{ID: id}
	}
	if len(arguments) != int(signature.ArgumentCount) {
		return nil, sqlError("42883", "JSON record argument count does not match signature")
	}
	if uint64(unsafe.Sizeof(jsonRecordPlan{})) > maximum {
		return nil, jsonRecordMemoryError()
	}
	plan := &jsonRecordPlan{}
	switch id {
	case 3204, 3205:
	case 3490, 3491:
		plan.binary = true
	case 3960, 3961:
		plan.jsonArgument = 1
	case 3209, 3475, 6338:
		plan.jsonArgument, plan.binary = 1, true
	default:
		return nil, &UnknownFunctionError{ID: id}
	}
	plan.set = id == 3205 || id == 3491 || id == 3475 || id == 3961
	plan.valid = id == 6338
	layout := result
	if plan.valid {
		if result.Type.ID != 16 || result.Type.Storage != model.TypeBoolean {
			return nil, sqlError("42804", "JSON record validity requires boolean result")
		}
		layout = arguments[0]
	}
	if layout.Type.Storage != model.TypeComposite || len(layout.FieldNames) != len(layout.Children) {
		return nil, sqlError("0A000", "could not determine row type for result of "+signature.Name)
	}
	if layout.Type.ID == 2249 && len(layout.Children) == 0 && layout.Type.Modifier < 0 {
		return nil, sqlError("0A000", "could not determine row type for result of "+signature.Name)
	}
	if err := validateConcreteLayout(ctx, layout); err != nil {
		return nil, err
	}
	if plan.jsonArgument == 1 && (arguments[0].Type.Storage != model.TypeComposite || arguments[0].Type.ID != layout.Type.ID) {
		return nil, sqlError("42804", "first argument of JSON populate function must be a row type")
	}
	jsonID := model.TypeID(114)
	if plan.binary {
		jsonID = 3802
	}
	if arguments[plan.jsonArgument].Type.ID != jsonID || arguments[plan.jsonArgument].Type.Storage != model.TypeJSONB {
		return nil, sqlError("42804", "JSON record input type does not match signature")
	}
	if len(arguments) == 3 && (arguments[2].Type.ID != 16 || arguments[2].Type.Storage != model.TypeBoolean) {
		return nil, sqlError("42804", "JSON record option must be boolean")
	}
	plan.metadata = uint64(unsafe.Sizeof(jsonRecordPlan{})) + uint64(len(arguments))*uint64(unsafe.Sizeof(model.Type{}))
	if plan.metadata > maximum {
		return nil, jsonRecordMemoryError()
	}
	plan.argumentTypes = make([]model.Type, len(arguments))
	for i := range arguments {
		plan.argumentTypes[i] = arguments[i].Type
	}
	remaining := maximum - plan.metadata
	if err := plan.root.resolve(ctx, layout, options, &remaining); err != nil {
		return nil, err
	}
	plan.metadata = maximum - remaining
	return plan, nil
}

func (field *jsonRecordField) resolve(ctx context.Context, layout model.TypeLayout, options ResolveOptions, remaining *uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	field.typ = layout.Type
	if layout.Type.Storage == model.TypeComposite && len(layout.FieldNames) != len(layout.Children) {
		return sqlError("42804", "JSON record conversion requires declared field names")
	}
	bytes := uint64(len(layout.Children)) * uint64(unsafe.Sizeof(jsonRecordField{}))
	for _, name := range layout.FieldNames {
		if uint64(len(name)) > *remaining || bytes > *remaining-uint64(len(name)) {
			return jsonRecordMemoryError()
		}
		bytes += uint64(len(name))
	}
	if bytes > *remaining {
		return jsonRecordMemoryError()
	}
	*remaining -= bytes
	field.children = make([]jsonRecordField, len(layout.Children))
	for i, child := range layout.Children {
		if i < len(layout.FieldNames) {
			field.children[i].name = strings.Clone(layout.FieldNames[i])
		}
		if err := field.children[i].resolve(ctx, child, options, remaining); err != nil {
			return err
		}
	}
	info, known := lookupResolvedTypeIO(options, layout)
	if !known || info.Input == 0 {
		return sqlError("42804", "JSON record field has no resolved input metadata")
	}
	field.parameter = info.Parameter
	input, err := resolveElementInput(ctx, info.Input, layout, options, *remaining)
	if err != nil {
		return err
	}
	if input.signature.Volatility == 'v' {
		return sqlError("0A000", "volatile input functions are not supported by JSON record conversion")
	}
	if input.OwnedBytes() > *remaining {
		return jsonRecordMemoryError()
	}
	*remaining -= input.OwnedBytes()
	field.input = input
	return nil
}

func jsonRecordMemoryError() error {
	return sqlError("54000", "JSON record conversion memory budget exceeded")
}

func (plan *jsonRecordPlan) OwnedBytes() uint64 { return plan.metadata }

func (plan *jsonRecordPlan) call(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(budget.MemoryBytes)
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
		budget.Allocator = nil
	}
	input := arguments[plan.jsonArgument]
	if plan.binary && !input.IsNull {
		canonical, err := canonicalJSONB(ctx, input.StrVal, int64(min(budget.MemoryBytes, math.MaxInt64)), false)
		if err != nil {
			return model.Value{}, err
		}
		budget, err = budget.After(uint64(len(canonical)))
		if err != nil {
			return model.Value{}, err
		}
		var ownedArguments [3]model.Value
		copy(ownedArguments[:], arguments)
		ownedArguments[plan.jsonArgument].StrVal = canonical
		return plan.callPrepared(ctx, ownedArguments[:len(arguments)], budget)
	}
	return plan.callPrepared(ctx, arguments, budget)
}

func (plan *jsonRecordPlan) callPrepared(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if plan.valid {
		budget.OutputBytes = int64(min(budget.MemoryBytes, math.MaxInt64))
	}
	base := model.NullVal(model.TypeComposite)
	if plan.jsonArgument == 1 {
		base = arguments[0]
	}
	input := arguments[plan.jsonArgument]
	if input.IsNull {
		if plan.valid {
			if base.IsNull {
				return model.NullVal(model.TypeBoolean), nil
			}
			return model.BoolVal(true), nil
		}
		value, _, err := model.CopyValue(ctx, base, min(uint64(budget.OutputBytes), budget.MemoryBytes))
		return value, jsonRecordValueError(err)
	}
	value, err := plan.convert(ctx, input.StrVal, base, budget)
	if plan.valid {
		if err == nil {
			return model.BoolVal(true), nil
		}
		var sql *model.Error
		if errors.As(err, &sql) && strings.HasPrefix(sql.Code, "22") {
			return model.BoolVal(false), nil
		}
	}
	return value, err
}

func (plan *jsonRecordPlan) convert(ctx context.Context, text string, base model.Value, budget CallBudget) (model.Value, error) {
	// Decoding all strings matches the JSON semantic-action parser, including
	// invalid Unicode in fields that the record does not select.
	if _, _, err := scanJSONTextExtract(ctx, text, &jsonTextSelector{}); err != nil {
		return model.Value{}, err
	}
	scanner := jsonTextScanner{text: text}
	span, err := scanJSONCollectionValue(ctx, &scanner)
	if err != nil {
		return model.Value{}, err
	}
	if span.kind != jsonTextObject {
		return model.Value{}, jsonRecordObjectError(span.kind)
	}
	return plan.root.convert(ctx, text[span.start:span.end], span.kind, base, budget)
}

func jsonRecordObjectError(kind jsonTextKind) error {
	if kind == jsonTextArray {
		return sqlError("22023", "cannot call populate_composite on an array")
	}
	return sqlError("22023", "cannot call populate_composite on a scalar")
}

func (field *jsonRecordField) convert(ctx context.Context, text string, kind jsonTextKind, base model.Value, budget CallBudget) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if kind == jsonTextNull {
		return model.NullVal(field.typ.Storage), nil
	}
	if kind != jsonTextString {
		switch field.typ.Storage {
		case model.TypeComposite:
			if kind != jsonTextObject {
				return model.Value{}, jsonRecordObjectError(kind)
			}
			return field.object(ctx, text, base, budget)
		case model.TypeArray:
			if kind != jsonTextArray {
				return model.Value{}, sqlError("22P02", "expected JSON array")
			}
			return field.array(ctx, text, budget)
		}
	}
	var temporary uint64
	if kind == jsonTextString && field.typ.ID == 114 {
		canonical, err := canonicalJSONB(ctx, text, int64(min(budget.MemoryBytes, math.MaxInt64)), false)
		if err != nil {
			return model.Value{}, err
		}
		text, temporary = canonical, uint64(len(canonical))
	}
	if kind == jsonTextString && field.typ.ID != 114 && field.typ.ID != 3802 {
		decoded, err := decodeJSONString(ctx, text, int64(min(budget.MemoryBytes, math.MaxInt64)))
		if err != nil {
			return model.Value{}, err
		}
		text, temporary = decoded, uint64(len(decoded))
	}
	nested, err := budget.After(temporary)
	if err != nil {
		return model.Value{}, err
	}
	arguments := [3]model.Value{model.StringVal(text), model.Int64Val(int64(field.parameter)), model.Int64Val(int64(field.typ.Modifier))}
	return field.input.Call(ctx, arguments[:field.input.ArgumentCount()], nested)
}

func (field *jsonRecordField) object(ctx context.Context, text string, base model.Value, budget CallBudget) (model.Value, error) {
	headers := uint64(len(field.children)) * uint64(unsafe.Sizeof(model.Value{}))
	spanBytes := uint64(len(field.children)) * uint64(unsafe.Sizeof(jsonTextSpan{}))
	if headers > uint64(budget.OutputBytes) || headers > budget.MemoryBytes || spanBytes > budget.MemoryBytes-headers {
		return model.Value{}, jsonRecordMemoryError()
	}
	if !base.IsNull && (base.Type != model.TypeComposite || len(base.ArrayVal) != len(field.children)) {
		return model.Value{}, sqlError("42804", "JSON record base does not match declared layout")
	}
	spans := make([]jsonTextSpan, len(field.children))
	cursor := newJSONCollectionCursor(text, true)
	for {
		key, value, ok, err := cursor.next(ctx)
		if err != nil {
			return model.Value{}, err
		}
		if !ok {
			break
		}
		for i := range field.children {
			matched, err := jsonStringEquals(ctx, text[key.start:key.end], field.children[i].name)
			if err != nil {
				return model.Value{}, err
			}
			if matched {
				spans[i] = value
			}
		}
	}
	values := make([]model.Value, len(field.children))
	used := headers
	for i := range field.children {
		child := &field.children[i]
		span := spans[i]
		previous := model.NullVal(child.typ.Storage)
		if !base.IsNull {
			previous = base.ArrayVal[i]
		}
		remaining := uint64(budget.OutputBytes) - used
		nested, err := budget.After(used + spanBytes)
		if err != nil {
			return model.Value{}, err
		}
		nested.OutputBytes = int64(remaining)
		var value model.Value
		if span.kind == 0 {
			value, _, err = model.CopyValue(ctx, previous, min(remaining, nested.MemoryBytes))
		} else {
			value, err = child.convert(ctx, text[span.start:span.end], span.kind, previous, nested)
		}
		if err != nil {
			return model.Value{}, jsonRecordValueError(err)
		}
		size, err := model.ValueBytes(ctx, value, min(remaining, nested.MemoryBytes))
		if err != nil {
			return model.Value{}, jsonRecordValueError(err)
		}
		used += size
		values[i] = value
	}
	return model.Value{Type: model.TypeComposite, ArrayVal: values}, nil
}

func jsonRecordValueError(err error) error {
	if errors.Is(err, model.ErrValueBudget) || errors.Is(err, model.ErrValueDepth) {
		return jsonRecordMemoryError()
	}
	return err
}

// PostgreSQL fixes the leaf depth at the first scalar, object, or empty array.
// Arrays encountered at that depth are values, not additional SQL dimensions.
func (field *jsonRecordField) array(ctx context.Context, text string, budget CallBudget) (model.Value, error) {
	var dimensions [6]model.ArrayDimension
	for i := range dimensions {
		dimensions[i].Length = -1
	}
	depth := 0
	count, shapeErr := jsonRecordArrayShape(ctx, text, 0, &depth, &dimensions)
	if shapeErr != nil {
		var sql *model.Error
		if !errors.As(shapeErr, &sql) || sql.Code != "22P02" {
			return model.Value{}, shapeErr
		}
	}
	if count == 0 {
		return model.Value{Type: model.TypeArray}, shapeErr
	}
	headers := uint64(count)*uint64(unsafe.Sizeof(model.Value{})) + uint64(depth)*uint64(unsafe.Sizeof(model.ArrayDimension{}))
	if headers > uint64(budget.OutputBytes) || headers > budget.MemoryBytes {
		return model.Value{}, jsonRecordMemoryError()
	}
	values := make([]model.Value, count)
	used, position := headers, 0
	var visit func(string, int) error
	visit = func(input string, level int) error {
		scanner := jsonTextScanner{text: input, position: 1}
		for {
			// Earlier subtype errors precede a later malformed sub-array.
			if shapeErr != nil && position == count {
				return shapeErr
			}
			scanner.space(ctx)
			if scanner.peek() == ']' {
				return scanner.err
			}
			span, err := scanJSONCollectionValue(ctx, &scanner)
			if err != nil {
				return err
			}
			part := input[span.start:span.end]
			if level+1 < depth {
				if err := visit(part, level+1); err != nil {
					return err
				}
			} else {
				nested, err := budget.After(used)
				if err != nil {
					return err
				}
				nested.OutputBytes = budget.OutputBytes - int64(used)
				value, err := field.children[0].convert(ctx, part, span.kind, model.NullVal(field.children[0].typ.Storage), nested)
				if err != nil {
					return err
				}
				size, err := model.ValueBytes(ctx, value, min(uint64(nested.OutputBytes), nested.MemoryBytes))
				if err != nil {
					return jsonRecordValueError(err)
				}
				values[position] = value
				position++
				used += size
			}
			scanner.space(ctx)
			if scanner.peek() == ',' {
				scanner.advance(ctx, 1)
			}
		}
	}
	if err := visit(text, 0); err != nil {
		return model.Value{}, err
	}
	dims := make([]model.ArrayDimension, depth)
	copy(dims, dimensions[:depth])
	return model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: dims}, nil
}

func jsonRecordArrayShape(ctx context.Context, text string, level int, depth *int, dimensions *[6]model.ArrayDimension) (int, error) {
	if level == len(dimensions) {
		return 0, sqlError("54000", "number of array dimensions exceeds the maximum allowed (6)")
	}
	scanner := jsonTextScanner{text: text, position: 1}
	length, leaves := 0, 0
	for {
		scanner.space(ctx)
		if scanner.err != nil {
			return 0, scanner.err
		}
		if scanner.peek() == ']' {
			if *depth == 0 {
				*depth = level + 1
			}
			break
		}
		span, err := scanJSONCollectionValue(ctx, &scanner)
		if err != nil {
			return 0, err
		}
		if *depth == 0 && span.kind != jsonTextArray {
			*depth = level + 1
		}
		count := 1
		if *depth == 0 || level+1 < *depth {
			if span.kind != jsonTextArray {
				return leaves, sqlError("22P02", "expected JSON array")
			}
			count, err = jsonRecordArrayShape(ctx, text[span.start:span.end], level+1, depth, dimensions)
			if err != nil {
				return leaves + count, err
			}
		}
		if length == math.MaxInt32 || count > math.MaxInt32-leaves {
			return 0, jsonRecordMemoryError()
		}
		length++
		leaves += count
		scanner.space(ctx)
		if scanner.peek() == ',' {
			scanner.advance(ctx, 1)
		}
	}
	if dimensions[level].Length >= 0 && dimensions[level].Length != int32(length) {
		return leaves, sqlError("22P02", "malformed JSON array: multidimensional arrays must have sub-arrays with matching dimensions")
	}
	dimensions[level] = model.ArrayDimension{LowerBound: 1, Length: int32(length)}
	return leaves, nil
}
