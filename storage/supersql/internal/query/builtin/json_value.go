package builtin

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// JSONFormat controls only the outer array or composite separator.
type JSONFormat uint8

const (
	JSONCompact JSONFormat = iota
	JSONLineFeeds
)

type jsonEncodeMode uint8

const (
	jsonTextValue jsonEncodeMode = iota
	jsonKeyToken
	jsonBinaryValue
)

// JSONValueEncoder owns an immutable SQL-type conversion plan. Values and
// invocation budgets are borrowed only for the duration of EncodeValue/EncodeKey.
type JSONValueEncoder struct {
	plan  jsonbDatumPlan
	bytes uint64
}

func ResolveJSONValue(ctx context.Context, layout model.TypeLayout, options ResolveOptions, maxMetadataBytes uint64) (*JSONValueEncoder, error) {
	if _, err := model.TypeLayoutBytes(ctx, layout, maxMetadataBytes); err != nil {
		return nil, codecLayoutError(err)
	}
	if err := validateConcreteLayout(ctx, layout); err != nil {
		return nil, err
	}
	base := uint64(unsafe.Sizeof(JSONValueEncoder{}))
	if base > maxMetadataBytes {
		return nil, sqlError("54000", "JSON conversion metadata budget exceeded")
	}
	remaining := maxMetadataBytes - base
	encoder := &JSONValueEncoder{}
	if err := encoder.plan.resolve(ctx, layout, options, &remaining); err != nil {
		return nil, err
	}
	encoder.bytes = maxMetadataBytes - remaining
	return encoder, nil
}

func (encoder *JSONValueEncoder) OwnedBytes() uint64 {
	return encoder.bytes
}

func (encoder *JSONValueEncoder) EncodeValue(ctx context.Context, value model.Value, format JSONFormat, budget CallBudget) (model.Value, error) {
	if format != JSONCompact && format != JSONLineFeeds {
		return model.Value{}, sqlError("22023", "invalid JSON output format")
	}
	return encoder.encode(ctx, value, format, jsonTextValue, budget)
}

// EncodeKey returns the quoted token, not the unescaped SQL output text.
func (encoder *JSONValueEncoder) EncodeKey(ctx context.Context, value model.Value, budget CallBudget) (model.Value, error) {
	result, err := encoder.encode(ctx, value, JSONCompact, jsonKeyToken, budget)
	result.Type = model.TypeString
	return result, err
}

func (encoder *JSONValueEncoder) encode(ctx context.Context, value model.Value, format JSONFormat, mode jsonEncodeMode, budget CallBudget) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if budget.OutputBytes < 0 {
		return model.Value{}, sqlError("22023", "negative builtin output byte budget")
	}
	budget.OutputBytes = min(budget.OutputBytes, int64(min(budget.MemoryBytes, uint64(1<<63-1))))
	argument := [1]model.Value{value}
	return jsonBuildDocument(ctx, budget, mode == jsonBinaryValue, func(writer *jsonbBuildWriter) error {
		if format == JSONLineFeeds {
			writer.lineFeeds = &encoder.plan
		}
		return encoder.plan.emit(writer, argument[:], mode == jsonKeyToken)
	})
}

func resolveJSONConversion(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if len(arguments) != int(signature.ArgumentCount) {
		return Callable{}, sqlError("42883", "builtin argument count does not match bound signature")
	}
	if result.Type.ID != signature.Result || result.Type.Storage != model.TypeJSONB {
		return Callable{}, sqlError("42804", "JSON conversion result type mismatch")
	}
	switch signature.ID {
	case 3153, 3154:
		if arguments[0].Type.Storage != model.TypeArray {
			return Callable{}, sqlError("42804", "array_to_json requires an array")
		}
	case 3155, 3156:
		if arguments[0].Type.Storage != model.TypeComposite {
			return Callable{}, sqlError("42804", "row_to_json requires a composite")
		}
	}
	if len(arguments) == 2 && (arguments[1].Type.ID != 16 || arguments[1].Type.Storage != model.TypeBoolean) {
		return Callable{}, sqlError("42804", "JSON line-feed argument must be boolean")
	}
	const closureBytes = uint64(unsafe.Sizeof(uintptr(0))) * 3
	if closureBytes > maximum {
		return Callable{}, sqlError("54000", "JSON conversion metadata budget exceeded")
	}
	encoder, err := ResolveJSONValue(ctx, arguments[0], options, maximum-closureBytes)
	if err != nil {
		return Callable{}, err
	}
	callable := Callable{signature: signature, resultStorage: model.TypeJSONB, metadataBytes: encoder.OwnedBytes() + closureBytes}
	for i, argument := range arguments {
		callable.signature.Arguments[i] = argument.Type.ID
		callable.argumentStorage[i] = argument.Type.Storage
	}
	mode := jsonTextValue
	if signature.ID == 3787 {
		mode = jsonBinaryValue
	}
	callable.kernel = func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		format := JSONCompact
		if len(values) == 2 && values[1].IntVal != 0 {
			format = JSONLineFeeds
		}
		return encoder.encode(ctx, values[0], format, mode, budget)
	}
	return callable, nil
}
