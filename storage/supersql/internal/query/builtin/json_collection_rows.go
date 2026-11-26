package builtin

import (
	"context"
	"fmt"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type jsonCollectionPlan struct {
	name     string
	binary   bool
	object   bool
	keysOnly bool
	asText   bool
}

func ResolveJSONCollectionRows(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (RowSourcePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan := jsonCollectionPlan{}
	switch id {
	case 3208:
		plan = jsonCollectionPlan{name: "jsonb_each", binary: true, object: true}
	case 3219:
		plan = jsonCollectionPlan{name: "jsonb_array_elements", binary: true}
	case 3465:
		plan = jsonCollectionPlan{name: "jsonb_array_elements_text", binary: true, asText: true}
	case 3931:
		plan = jsonCollectionPlan{name: "jsonb_object_keys", binary: true, object: true, keysOnly: true}
	case 3932:
		plan = jsonCollectionPlan{name: "jsonb_each_text", binary: true, object: true, asText: true}
	case 3955:
		plan = jsonCollectionPlan{name: "json_array_elements"}
	case 3957:
		plan = jsonCollectionPlan{name: "json_object_keys", object: true, keysOnly: true}
	case 3958:
		plan = jsonCollectionPlan{name: "json_each", object: true}
	case 3959:
		plan = jsonCollectionPlan{name: "json_each_text", object: true, asText: true}
	case 3969:
		plan = jsonCollectionPlan{name: "json_array_elements_text", asText: true}
	default:
		return nil, &UnknownFunctionError{ID: id}
	}
	inputID := model.TypeID(114)
	if plan.binary {
		inputID = 3802
	}
	if len(arguments) != 1 || arguments[0].Type.ID != inputID || arguments[0].Type.Storage != model.TypeJSONB || len(arguments[0].Children) != 0 {
		return nil, sqlError("42804", "JSON collection requires its declared input type")
	}
	valueID, storage := inputID, model.TypeJSONB
	if plan.asText || plan.keysOnly {
		valueID, storage = 25, model.TypeString
	}
	valueLayout := result
	if plan.object && !plan.keysOnly {
		if result.Type.Storage != model.TypeComposite || len(result.Children) != 2 || result.Children[0].Type.ID != 25 || result.Children[0].Type.Storage != model.TypeString || len(result.Children[0].Children) != 0 {
			return nil, sqlError("42804", "JSON each requires text key and declared value columns")
		}
		valueLayout = result.Children[1]
	}
	if valueLayout.Type.ID != valueID || valueLayout.Type.Storage != storage || len(valueLayout.Children) != 0 {
		return nil, sqlError("42804", "JSON collection result type does not match its signature")
	}
	if plan.OwnedBytes() > maximum {
		return nil, sqlError("54000", "builtin type metadata budget exceeded")
	}
	return &plan, nil
}

func (plan *jsonCollectionPlan) OwnedBytes() uint64 {
	return uint64(unsafe.Sizeof(*plan))
}

func (plan *jsonCollectionPlan) columns() int {
	if plan.object && !plan.keysOnly {
		return 2
	}
	return 1
}

func (plan *jsonCollectionPlan) shapeError(kind jsonTextKind) error {
	if plan.object && kind == jsonTextObject || !plan.object && kind == jsonTextArray {
		return nil
	}
	if plan.keysOnly {
		shape := "a scalar"
		if kind == jsonTextArray {
			shape = "an array"
		}
		return sqlError("22023", fmt.Sprintf("cannot call %s on %s", plan.name, shape))
	}
	if plan.object {
		if plan.binary {
			return sqlError("22023", fmt.Sprintf("cannot call %s on a non-object", plan.name))
		}
		if kind == jsonTextArray {
			return sqlError("22023", "cannot deconstruct an array as an object")
		}
		return sqlError("22023", "cannot deconstruct a scalar")
	}
	if plan.binary {
		if kind == jsonTextObject {
			return sqlError("22023", "cannot extract elements from an object")
		}
		return sqlError("22023", "cannot extract elements from a scalar")
	}
	if kind == jsonTextObject {
		return sqlError("22023", fmt.Sprintf("cannot call %s on a non-array", plan.name))
	}
	return sqlError("22023", fmt.Sprintf("cannot call %s on a scalar", plan.name))
}

type jsonCollectionIterator struct {
	plan     jsonCollectionPlan
	options  RowSourceOptions
	cursor   jsonCollectionCursor
	row      [2]model.Value
	claim    columnar.Allocation
	retained uint64
	closed   bool
}

func jsonCollectionBudgetError() error {
	return sqlError("54000", "JSON collection row or memory byte budget exceeded")
}

func (plan *jsonCollectionPlan) Open(ctx context.Context, arguments []model.Value, options RowSourceOptions) (RowSourceIterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(arguments) != 1 || arguments[0].Type != model.TypeJSONB {
		return nil, sqlError("42804", "JSON collection requires one JSON argument")
	}
	base := uint64(unsafe.Sizeof(jsonCollectionIterator{}))
	if base > options.MaxMemoryBytes {
		return nil, jsonCollectionBudgetError()
	}
	var claim columnar.Allocation
	if options.Allocator != nil {
		var err error
		claim, err = options.Allocator.Reserve(base)
		if err != nil {
			return nil, err
		}
	}
	iterator := &jsonCollectionIterator{plan: *plan, options: options, claim: claim, retained: base}
	if arguments[0].IsNull {
		iterator.Close()
		return iterator, nil
	}
	if err := iterator.prepare(ctx, arguments[0].StrVal); err != nil {
		iterator.Close()
		return nil, err
	}
	return iterator, nil
}

func (iterator *jsonCollectionIterator) resize(bytes uint64) error {
	if bytes > iterator.options.MaxMemoryBytes {
		return jsonCollectionBudgetError()
	}
	if iterator.claim != nil {
		return iterator.claim.Resize(bytes)
	}
	return nil
}

func (iterator *jsonCollectionIterator) prepare(ctx context.Context, text string) error {
	if uint64(len(text)) > iterator.options.MaxInputBytes {
		return sqlError("54000", "JSON collection input byte budget exceeded")
	}
	plan := &iterator.plan
	decode := plan.object || plan.asText || plan.binary
	scanner := jsonTextScanner{text: text}
	first, err := nextJSONExtractToken(ctx, &scanner, decode, nil)
	if err != nil {
		return err
	}
	if first.kind == 0 {
		return scanner.failure()
	}
	if err := plan.shapeError(first.kind); err != nil {
		return err
	}
	if plan.binary {
		text, err = iterator.canonicalize(ctx, text)
	} else if decode {
		_, _, err = scanJSONExtraction(ctx, text, &jsonExtraction{selector: &jsonTextSelector{}})
	} else {
		_, _, err = scanJSONText(ctx, text)
	}
	if err != nil {
		return err
	}
	iterator.cursor = newJSONCollectionCursor(text, plan.object)
	// PostgreSQL materializes these rows before the caller can apply LIMIT.
	preflight := iterator.cursor
	for {
		key, value, ok, err := preflight.next(ctx)
		if err != nil || !ok {
			return err
		}
		if _, err := iterator.rowBytes(ctx, key, value); err != nil {
			return err
		}
	}
}

func (iterator *jsonCollectionIterator) canonicalize(ctx context.Context, text string) (string, error) {
	remaining := iterator.options.MaxMemoryBytes - iterator.retained
	shape, err := walkJSONB(ctx, text, nil)
	if err != nil {
		return "", err
	}
	stateBytes := uint64(shape.nodes)*uint64(unsafe.Sizeof(jsonbNode{})) + uint64(shape.depth)*uint64(unsafe.Sizeof(jsonbFrame{})) + uint64(shape.decodedBytes)
	if stateBytes > remaining {
		return "", jsonCollectionBudgetError()
	}
	var claim columnar.Allocation
	if iterator.options.Allocator != nil {
		claim, err = iterator.options.Allocator.Reserve(stateBytes)
		if err != nil {
			return "", err
		}
	}
	var document jsonbDocument
	defer func() {
		document = jsonbDocument{}
		if claim != nil {
			claim.Release()
		}
	}()
	document, err = parseJSONBDocument(ctx, text, int64(min(stateBytes, uint64(math.MaxInt64))))
	if err != nil {
		return "", err
	}
	size := uint64(document.nodes[0].bytes)
	if uint64(document.stateBytes) > remaining || size > remaining-uint64(document.stateBytes) {
		return "", jsonCollectionBudgetError()
	}
	if err := iterator.resize(iterator.retained + size); err != nil {
		return "", err
	}
	output, err := document.encode(ctx, int64(min(remaining, uint64(math.MaxInt64))), false)
	if err != nil {
		return "", err
	}
	iterator.retained += size
	return output, nil
}

func (iterator *jsonCollectionIterator) rowBytes(ctx context.Context, key, value jsonTextSpan) (uint64, error) {
	text := iterator.cursor.scanner.text
	size := uint64(iterator.plan.columns()) * uint64(unsafe.Sizeof(model.Value{}))
	if iterator.plan.object {
		decoded, err := walkJSONDecodedString(ctx, text[key.start:key.end], nil)
		if err != nil {
			return 0, err
		}
		size += uint64(decoded)
	}
	if !iterator.plan.keysOnly {
		if iterator.plan.asText && value.kind == jsonTextString {
			decoded, err := walkJSONDecodedString(ctx, text[value.start:value.end], nil)
			if err != nil {
				return 0, err
			}
			size += uint64(decoded)
		} else if !iterator.plan.asText || value.kind != jsonTextNull {
			size += uint64(value.end - value.start)
		}
	}
	if size > iterator.options.MaxRowBytes || size > iterator.options.MaxMemoryBytes-iterator.retained {
		return 0, jsonCollectionBudgetError()
	}
	return size, nil
}

func (iterator *jsonCollectionIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if iterator.closed {
		return nil, false, nil
	}
	iterator.row = [2]model.Value{}
	if err := ctx.Err(); err != nil {
		iterator.Close()
		return nil, false, err
	}
	if err := iterator.resize(iterator.retained); err != nil {
		iterator.Close()
		return nil, false, err
	}
	key, value, ok, err := iterator.cursor.next(ctx)
	if err != nil || !ok {
		iterator.Close()
		return nil, false, err
	}
	size, err := iterator.rowBytes(ctx, key, value)
	if err == nil {
		err = iterator.resize(iterator.retained + size)
	}
	if err == nil {
		err = iterator.fillRow(ctx, key, value)
	}
	if err != nil {
		iterator.Close()
		return nil, false, err
	}
	return iterator.row[:iterator.plan.columns()], true, nil
}

func (iterator *jsonCollectionIterator) fillRow(ctx context.Context, key, value jsonTextSpan) error {
	text := iterator.cursor.scanner.text
	limit := int64(min(iterator.options.MaxRowBytes, uint64(math.MaxInt64)))
	column := 0
	if iterator.plan.object {
		decoded, err := decodeJSONString(ctx, text[key.start:key.end], limit)
		if err != nil {
			return err
		}
		iterator.row[0] = model.StringVal(decoded)
		column++
	}
	if iterator.plan.keysOnly {
		return nil
	}
	output := model.Value{Type: model.TypeJSONB, StrVal: text[value.start:value.end]}
	if iterator.plan.asText {
		output.Type = model.TypeString
		if value.kind == jsonTextNull {
			output = model.NullVal(model.TypeString)
		} else if value.kind == jsonTextString {
			var err error
			output.StrVal, err = decodeJSONString(ctx, output.StrVal, limit)
			if err != nil {
				return err
			}
		}
	}
	iterator.row[column] = output
	return ctx.Err()
}

func (iterator *jsonCollectionIterator) Close() error {
	iterator.row = [2]model.Value{}
	iterator.cursor = jsonCollectionCursor{}
	iterator.options = RowSourceOptions{}
	iterator.retained = 0
	iterator.closed = true
	if iterator.claim != nil {
		iterator.claim.Release()
		iterator.claim = nil
	}
	return nil
}
