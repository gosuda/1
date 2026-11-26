package builtin

import (
	"context"
	"errors"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type recordInputField struct {
	typ       model.Type
	parameter model.TypeID
	callable  Callable
	err       error
}

type recordInputCodec struct {
	target model.Type
	fields []recordInputField
}

func resolveRecordInput(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if len(arguments) != 3 || result.Type.Storage != model.TypeComposite {
		return Callable{}, sqlError("42804", "record input requires a declared composite result")
	}
	if err := validateConcreteLayout(ctx, result); err != nil {
		return Callable{}, err
	}
	logical, err := resolvedStorageLayout(result, model.TypeComposite, options)
	if err != nil {
		return Callable{}, err
	}
	for i, argument := range arguments {
		if argument.Type.ID != signature.Arguments[i] || argument.Type.Storage != primitiveStorage(signature.Arguments[i]) {
			return Callable{}, sqlError("42804", "record input metadata argument mismatch")
		}
	}
	errorBytes := max(uint64(unsafe.Sizeof(model.Error{})), uint64(unsafe.Sizeof(MissingImplementationError{})))
	metadata := uint64(unsafe.Sizeof(recordInputCodec{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	fieldBytes := uint64(unsafe.Sizeof(recordInputField{})) + errorBytes
	if metadata > maximum || uint64(len(logical.Children)) > (maximum-metadata)/fieldBytes {
		return Callable{}, sqlError("54000", "record input metadata budget exceeded")
	}
	metadata += uint64(len(logical.Children)) * fieldBytes
	codec := &recordInputCodec{target: result.Type, fields: make([]recordInputField, len(logical.Children))}
	for i, layout := range logical.Children {
		if err := ctx.Err(); err != nil {
			return Callable{}, err
		}
		field := &codec.fields[i]
		field.typ = layout.Type
		info, known := lookupResolvedTypeIO(options, layout)
		if !known || info.Input == 0 {
			field.err = sqlError("42804", "record field has no resolved input metadata")
		} else {
			field.parameter = info.Parameter
			field.callable, field.err = resolveElementInput(ctx, info.Input, layout, options, maximum-metadata)
		}
		if field.err != nil {
			if recordCodecResolutionFatal(field.err) {
				return Callable{}, field.err
			}
			continue
		}
		if field.callable.OwnedBytes() > maximum-metadata {
			return Callable{}, sqlError("54000", "record input metadata budget exceeded")
		}
		metadata += field.callable.OwnedBytes()
	}
	callable := Callable{signature: signature, kernel: codec.input, resultStorage: model.TypeComposite, metadataBytes: metadata}
	callable.signature.Result = result.Type.ID
	for i, argument := range arguments {
		callable.argumentStorage[i] = argument.Type.Storage
	}
	return callable, nil
}

func (codec *recordInputCodec) input(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	if arguments[1].IntVal != int64(codec.target.ID) {
		return model.Value{}, sqlError("42804", "record input OID differs from bound type")
	}
	if codec.target.ID == 2249 {
		if arguments[2].IntVal < 0 {
			return model.Value{}, sqlError("0A000", "input of anonymous composite types is not implemented")
		}
		if arguments[2].IntVal != int64(codec.target.Modifier) {
			return model.Value{}, sqlError("42804", "record input typmod differs from bound descriptor")
		}
	}
	// Named composite descriptors are selected by OID; PostgreSQL ignores their typmod argument.
	budget = min(budget, arrayInputMaxBytes)
	if int64(len(codec.fields)) > budget/arrayValueBytes {
		return model.Value{}, sqlError("54000", "record field headers exceed output byte budget")
	}
	headers := int64(len(codec.fields)) * arrayValueBytes
	if err := outputBudget(headers, budget); err != nil {
		return model.Value{}, err
	}
	var values []model.Value
	if len(codec.fields) != 0 {
		values = make([]model.Value, len(codec.fields))
	}
	defer func() { clear(values) }()
	input := arguments[0].StrVal
	position, err := recordInputSpace(ctx, input, 0)
	if err != nil {
		return model.Value{}, err
	}
	if position == len(input) || input[position] != '(' {
		return model.Value{}, recordTextError("missing left parenthesis")
	}
	position++
	var retained int64
	var callArguments [3]model.Value
	for i := range codec.fields {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if i != 0 {
			if position == len(input) || input[position] != ',' {
				return model.Value{}, recordTextError("too few columns")
			}
			position++
		}
		token, err := scanRecordField(ctx, input, position, nil)
		if err != nil {
			return model.Value{}, err
		}
		remaining := budget - headers - retained
		text, temporary, err := decodeRecordInputToken(ctx, input, position, token, remaining)
		if err != nil {
			return model.Value{}, err
		}
		position = token.end
		field := &codec.fields[i]
		if field.err != nil {
			return model.Value{}, field.err
		}
		callArguments[0] = model.StringVal(text)
		if token.null {
			callArguments[0] = model.NullVal(model.TypeString)
		}
		callArguments[1] = model.Int64Val(int64(field.parameter))
		callArguments[2] = model.Int64Val(int64(field.typ.Modifier))
		nested, err := callBudget.After(uint64(headers + retained + temporary))
		if err != nil {
			return model.Value{}, err
		}
		value, err := field.callable.Call(ctx, callArguments[:field.callable.ArgumentCount()], nested.WithOutput(remaining-temporary))
		clear(callArguments[:])
		text = ""
		if err != nil {
			return model.Value{}, err
		}
		if value.IsNull != token.null {
			return model.Value{}, sqlError("XX000", "record input function returned unexpected nullness")
		}
		if value.Type != field.typ.Storage {
			return model.Value{}, sqlError("XX000", "record input function returned unexpected storage")
		}
		bytes, err := model.ValueBytes(ctx, value, uint64(remaining))
		if err != nil {
			if errors.Is(err, model.ErrValueBudget) {
				err = sqlError("54000", "record field payload exceeds output byte budget")
			}
			return model.Value{}, err
		}
		retained += int64(bytes)
		values[i] = value
	}
	if position == len(input) || input[position] != ')' {
		return model.Value{}, recordTextError("too many columns")
	}
	position, err = recordInputSpace(ctx, input, position+1)
	if err != nil {
		return model.Value{}, err
	}
	if position < len(input) && input[position] != 0 {
		return model.Value{}, recordTextError("junk after right parenthesis")
	}
	result := model.Value{Type: model.TypeComposite, ArrayVal: values}
	values = nil
	return result, nil
}

func decodeRecordInputToken(ctx context.Context, input string, start int, token recordInputToken, budget int64) (string, int64, error) {
	if token.null {
		return "", 0, nil
	}
	if !token.decode {
		return input[start:token.end], 0, nil
	}
	if err := outputBudget(token.bytes, budget); err != nil {
		return "", 0, err
	}
	var output strings.Builder
	output.Grow(int(token.bytes))
	if _, err := scanRecordField(ctx, input, start, &output); err != nil {
		return "", 0, err
	}
	return output.String(), token.bytes, nil
}
