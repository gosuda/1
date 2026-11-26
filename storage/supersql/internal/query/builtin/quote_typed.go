package builtin

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type quoteTypedCodec struct {
	conversion Callable
	input      Callable
	nullable   bool
}

func resolveQuoteTyped(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if len(arguments) != 1 || result.Type.ID != 25 || result.Type.Storage != model.TypeString || result.Type.Modifier != -1 {
		return Callable{}, sqlError("42804", "generic quoting requires one concrete argument and text result")
	}
	source := arguments[0]
	info, catalogued := LookupType(source.Type.ID)
	io, known := lookupResolvedTypeIO(options, source)
	pseudo := catalogued && info.Kind == 'p' && source.Type.ID != 2249 && source.Type.ID != 2287
	if !known || pseudo {
		return Callable{}, sqlError("42804", "generic quoting requires a concrete SQL type")
	}
	if err := validateConcreteLayout(ctx, source); err != nil {
		return Callable{}, err
	}
	metadata := uint64(unsafe.Sizeof(quoteTypedCodec{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "quote conversion metadata budget exceeded")
	}
	codec := &quoteTypedCodec{nullable: signature.ID == 1290}
	cast, hasCast := LookupCast(source.Type.ID, 25)
	var err error
	// The SQL wrappers cast to text, whose explicit casts can differ from typoutput.
	switch {
	case source.Type.ID == 25 || hasCast && cast.Method == 'b':
		if source.Type.Storage != model.TypeString {
			return Callable{}, sqlError("42804", "binary text cast requires string storage")
		}
	case hasCast && cast.Method == 'f':
		codec.conversion, err = resolveQuoteTextCast(ctx, cast.Function, source, options, maximum-metadata)
	case !hasCast || cast.Method == 'i':
		output, found := LookupSignature(io.Output)
		if !found || output.ArgumentCount != 1 || output.Result != 2275 {
			return Callable{}, sqlError("42804", "quote argument has no resolved output signature")
		}
		cstring := model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
		codec.conversion, err = ResolveTyped(ctx, io.Output, []model.TypeLayout{source}, cstring, options, maximum-metadata)
		if err != nil {
			return Callable{}, err
		}
		text, found := LookupType(25)
		if !found || text.Input == 0 {
			return Callable{}, sqlError("42804", "quote conversion has no resolved text input")
		}
		codec.input, err = resolveElementInput(ctx, text.Input, result, options, maximum-metadata-codec.conversion.OwnedBytes())
	default:
		return Callable{}, sqlError("42804", "unsupported selected text cast")
	}
	if err != nil {
		return Callable{}, err
	}
	callable := Callable{
		signature: signature, kernel: codec.call, resultStorage: model.TypeString,
		metadataBytes: metadata + codec.conversion.OwnedBytes() + codec.input.OwnedBytes(),
	}
	callable.signature.Arguments[0] = source.Type.ID
	callable.argumentStorage[0] = source.Type.Storage
	return callable, nil
}

func resolveQuoteTextCast(ctx context.Context, id model.FunctionID, source model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	signature, found := LookupSignature(id)
	if !found {
		return Callable{}, &UnknownFunctionError{ID: id}
	}
	invalidArity := signature.ArgumentCount < 1 || signature.ArgumentCount > 3
	invalidModifier := signature.ArgumentCount >= 2 && signature.Arguments[1] != 23
	invalidExplicitness := signature.ArgumentCount == 3 && signature.Arguments[2] != 16
	if invalidArity || invalidModifier || invalidExplicitness {
		return Callable{}, sqlError("42804", "unsupported text cast signature")
	}
	if source.Type.ID != signature.Arguments[0] {
		cast, found := LookupCast(source.Type.ID, signature.Arguments[0])
		if !found || cast.Context != 'i' || cast.Method != 'b' {
			return Callable{}, sqlError("42804", "text cast argument is not binary compatible")
		}
		source.Type.ID = signature.Arguments[0]
	}
	if signature.Result != 25 {
		cast, found := LookupCast(signature.Result, 25)
		if !found || cast.Method != 'b' {
			return Callable{}, sqlError("42804", "text cast result is not binary compatible")
		}
	}
	var arguments [3]model.TypeLayout
	arguments[0] = source
	for i := 1; i < int(signature.ArgumentCount); i++ {
		arguments[i].Type = model.Type{ID: signature.Arguments[i], Modifier: -1, Storage: primitiveStorage(signature.Arguments[i])}
	}
	result := model.TypeLayout{Type: model.Type{ID: signature.Result, Modifier: -1, Storage: model.TypeString}}
	return ResolveTyped(ctx, id, arguments[:signature.ArgumentCount], result, options, maximum)
}

func (codec *quoteTypedCodec) call(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	value := arguments[0]
	if value.IsNull {
		if codec.nullable {
			return ownedText(ctx, budget, "NULL")
		}
		return model.NullVal(model.TypeString), nil
	}
	if codec.conversion.kernel == nil {
		return quoteLiteralText(ctx, value.StrVal, uint64(budget))
	}
	text, err := codec.convert(ctx, value, callBudget)
	if err != nil {
		return model.Value{}, err
	}
	if text.IsNull {
		if codec.nullable {
			return ownedText(ctx, budget, "NULL")
		}
		return model.NullVal(model.TypeString), nil
	}
	// The conversion result remains live while its quoted replacement is allocated.
	return quoteLiteralText(ctx, text.StrVal, uint64(budget-int64(len(text.StrVal))))
}

func (codec *quoteTypedCodec) convert(ctx context.Context, value model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	var arguments [3]model.Value
	arguments[0] = value
	arguments[1] = model.Int64Val(-1)
	arguments[2] = model.BoolVal(true)
	text, err := codec.conversion.Call(ctx, arguments[:codec.conversion.signature.ArgumentCount], callBudget.WithOutput(budget))
	clear(arguments[:])
	if err != nil {
		return model.Value{}, err
	}
	if text.Type != model.TypeString {
		return model.Value{}, sqlError("XX000", "text conversion did not return string storage")
	}
	if err := outputBudget(int64(len(text.StrVal)), budget); err != nil {
		return model.Value{}, err
	}
	if codec.input.kernel == nil {
		return text, nil
	}
	if text.IsNull {
		return model.Value{}, sqlError("XX000", "type output returned NULL")
	}
	remaining := budget - int64(len(text.StrVal))
	arguments[0] = text
	arguments[1] = model.Int64Val(25)
	arguments[2] = model.Int64Val(-1)
	nested, err := callBudget.After(uint64(len(text.StrVal)))
	if err != nil {
		return model.Value{}, err
	}
	converted, err := codec.input.Call(ctx, arguments[:codec.input.signature.ArgumentCount], nested.WithOutput(remaining))
	clear(arguments[:])
	if err != nil {
		return model.Value{}, err
	}
	if converted.Type != model.TypeString {
		return model.Value{}, sqlError("XX000", "text input did not return string storage")
	}
	if err := outputBudget(int64(len(converted.StrVal)), remaining); err != nil {
		return model.Value{}, err
	}
	return converted, nil
}
