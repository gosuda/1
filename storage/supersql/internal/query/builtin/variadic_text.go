package builtin

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type variadicTextCodec struct {
	signature     Signature
	prefix        int
	array         bool
	argumentTypes []model.Type
	outputs       []variadicTextOutput
}

type variadicTextOutput struct {
	typ      model.Type
	callable Callable
	err      error
}

type variadicTextValues struct {
	variadicValues
	outputs []variadicTextOutput
	array   bool
}

func variadicTextPrefix(id model.FunctionID) (int, bool) {
	switch id {
	case 3058:
		return 0, true
	case 3059, 3539:
		return 1, true
	default:
		return 0, false
	}
}

func resolveVariadicText(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64, array bool) (Callable, error) {
	prefix, _ := variadicTextPrefix(signature.ID)
	element, err := validateVariadicLayouts(ctx, signature, arguments, result, maximum, prefix, array, options)
	if err != nil {
		return Callable{}, err
	}
	if result.Type.ID != 25 || result.Type.Storage != model.TypeString {
		return Callable{}, sqlError("42804", "variadic text function requires text result")
	}
	for _, argument := range arguments[:prefix] {
		if argument.Type.ID != 25 || argument.Type.Storage != model.TypeString {
			return Callable{}, sqlError("42804", "variadic text prefix argument must be text")
		}
	}
	outputCount := len(arguments) - prefix
	if array {
		outputCount = 1
	}
	// Reserve possible deferred selection errors as well as all slice backing
	// storage before retaining a plan. Nested codecs debit the remaining budget.
	errorBytes := max(uint64(unsafe.Sizeof(model.Error{})), uint64(unsafe.Sizeof(MissingImplementationError{})))
	metadata := uint64(unsafe.Sizeof(variadicTextCodec{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	metadata += uint64(len(arguments)) * uint64(unsafe.Sizeof(model.Type{}))
	metadata += uint64(outputCount) * (uint64(unsafe.Sizeof(variadicTextOutput{})) + errorBytes)
	if metadata > maximum {
		return Callable{}, sqlError("54000", "variadic text metadata budget exceeded")
	}
	codec := &variadicTextCodec{
		signature: signature, prefix: prefix, array: array,
		argumentTypes: make([]model.Type, len(arguments)),
		outputs:       make([]variadicTextOutput, outputCount),
	}
	for i, argument := range arguments {
		codec.argumentTypes[i] = argument.Type
	}
	cstring := model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
	for i := range codec.outputs {
		if err := ctx.Err(); err != nil {
			return Callable{}, err
		}
		layout := element
		if !array {
			layout = arguments[prefix+i]
		}
		selected := &codec.outputs[i]
		selected.typ = layout.Type
		info, known := lookupResolvedTypeIO(options, layout)
		if !known || info.Output == 0 {
			selected.err = sqlError("42804", "variadic argument has no resolved output metadata")
		} else {
			selected.callable, selected.err = ResolveTyped(ctx, info.Output, []model.TypeLayout{layout}, cstring, options, maximum-metadata)
		}
		if selected.err != nil {
			var state *model.Error
			cancelled := errors.Is(selected.err, context.Canceled) || errors.Is(selected.err, context.DeadlineExceeded)
			budgetFailure := errors.As(selected.err, &state) && state.Code == "54000"
			if signature.ID != 3539 || cancelled || budgetFailure {
				return Callable{}, selected.err
			}
			continue
		}
		if selected.callable.OwnedBytes() > maximum-metadata {
			return Callable{}, sqlError("54000", "variadic text metadata budget exceeded")
		}
		metadata += selected.callable.OwnedBytes()
	}
	callable := Callable{signature: signature, resultStorage: model.TypeString, argumentTypes: codec.argumentTypes, metadataBytes: metadata}
	if signature.ID == 3539 {
		callable.kernel = compileFormatText(codec)
	} else {
		callable.kernel = compileConcatText(codec)
	}
	return callable, nil
}

func (codec *variadicTextCodec) values(arguments []model.Value) (variadicTextValues, error) {
	decoded, err := decodeVariadicValues(arguments, codec.prefix, codec.array)
	if err != nil {
		return variadicTextValues{}, err
	}
	if codec.array && len(codec.outputs) != 1 {
		return variadicTextValues{}, sqlError("XX000", "invalid variadic array argument plan")
	}
	if !codec.array && len(decoded.values) != len(codec.outputs) {
		return variadicTextValues{}, sqlError("XX000", "variadic output plan does not match arguments")
	}
	return variadicTextValues{variadicValues: decoded, outputs: codec.outputs, array: codec.array}, nil
}

func (values variadicTextValues) text(ctx context.Context, index int, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if index < 0 || index >= len(values.values) {
		return model.Value{}, sqlError("22023", "variadic argument index out of range")
	}
	if values.values[index].IsNull {
		return model.NullVal(model.TypeString), nil
	}
	plan := index
	if values.array {
		plan = 0
	}
	if plan >= len(values.outputs) {
		return model.Value{}, sqlError("XX000", "variadic argument has no output plan")
	}
	selected := &values.outputs[plan]
	if selected.err != nil {
		return model.Value{}, selected.err
	}
	value, err := selected.callable.Call(ctx, values.values[index:index+1], callBudget.WithOutput(budget))
	if err != nil {
		return model.Value{}, err
	}
	if value.IsNull || value.Type != model.TypeString {
		return model.Value{}, sqlError("XX000", "variadic argument output did not return cstring")
	}
	if err := outputBudget(int64(len(value.StrVal)), budget); err != nil {
		return model.Value{}, err
	}
	if _, err := textCharacters(ctx, value.StrVal); err != nil {
		return model.Value{}, err
	}
	return value, nil
}

func (values variadicTextValues) typ(index int) model.Type {
	if index < 0 || index >= len(values.values) {
		return model.Type{}
	}
	if values.array {
		index = 0
	}
	if index >= len(values.outputs) {
		return model.Type{}
	}
	return values.outputs[index].typ
}
