package builtin

import (
	"context"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type recordOutputCodec struct {
	fields []recordOutputField
}

type recordOutputField struct {
	typ      model.Type
	callable Callable
	err      error
}

func resolveRecordOutput(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if len(arguments) != 1 || arguments[0].Type.Storage != model.TypeComposite {
		return Callable{}, sqlError("42804", "record output requires a composite argument")
	}
	if result.Type.ID != 2275 || result.Type.Storage != model.TypeString {
		return Callable{}, sqlError("42804", "record output requires cstring result")
	}
	if err := validateConcreteLayout(ctx, arguments[0]); err != nil {
		return Callable{}, err
	}
	logical, err := resolvedStorageLayout(arguments[0], model.TypeComposite, options)
	if err != nil {
		return Callable{}, err
	}
	fields := logical.Children
	errorBytes := max(uint64(unsafe.Sizeof(model.Error{})), uint64(unsafe.Sizeof(MissingImplementationError{})))
	metadata := uint64(unsafe.Sizeof(recordOutputCodec{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	fieldBytes := uint64(unsafe.Sizeof(recordOutputField{})) + errorBytes
	if metadata > maximum || uint64(len(fields)) > (maximum-metadata)/fieldBytes {
		return Callable{}, sqlError("54000", "record output metadata budget exceeded")
	}
	metadata += uint64(len(fields)) * fieldBytes
	codec := &recordOutputCodec{fields: make([]recordOutputField, len(fields))}
	cstring := model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
	for i, field := range fields {
		if err := ctx.Err(); err != nil {
			return Callable{}, err
		}
		selected := &codec.fields[i]
		selected.typ = field.Type
		info, known := lookupResolvedTypeIO(options, field)
		if !known || info.Output == 0 {
			selected.err = sqlError("42804", "record field has no resolved output metadata")
		} else {
			selected.callable, selected.err = ResolveTyped(ctx, info.Output, []model.TypeLayout{field}, cstring, options, maximum-metadata)
		}
		if selected.err != nil {
			if recordCodecResolutionFatal(selected.err) {
				return Callable{}, selected.err
			}
			continue
		}
		if selected.callable.OwnedBytes() > maximum-metadata {
			return Callable{}, sqlError("54000", "record output metadata budget exceeded")
		}
		metadata += selected.callable.OwnedBytes()
	}
	callable := Callable{signature: signature, resultStorage: model.TypeString, kernel: codec.output, metadataBytes: metadata}
	callable.signature.Arguments[0] = arguments[0].Type.ID
	callable.argumentStorage[0] = model.TypeComposite
	callable.signature.Result = result.Type.ID
	return callable, nil
}

func (codec *recordOutputCodec) output(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	value := arguments[0]
	if value.Type != model.TypeComposite || len(value.Dimensions) != 0 || len(value.ArrayVal) != len(codec.fields) {
		return model.Value{}, sqlError("42804", "record value differs from bound composite layout")
	}
	output := textBuffer{budget: min(budget, (1<<30)-5)}
	// Reserve all punctuation so empty and NULL-only records need no growth.
	if err := output.reserveTemporary(ctx, max(2, int64(len(codec.fields))+1), 0); err != nil {
		return model.Value{}, err
	}
	output.builder.WriteByte('(')
	var element arrayElementCall
	for i := range codec.fields {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if i != 0 {
			output.builder.WriteByte(',')
		}
		field := &codec.fields[i]
		datum := value.ArrayVal[i]
		if datum.Type != field.typ.Storage {
			return model.Value{}, sqlError("42804", "record field storage differs from bound composite layout")
		}
		if datum.IsNull {
			continue
		}
		if field.err != nil {
			return model.Value{}, field.err
		}
		element.function = field.callable
		nested, err := callBudget.After(uint64(output.builder.Cap()))
		if err != nil {
			return model.Value{}, err
		}
		converted, err := element.invoke(ctx, datum, nested.WithOutput(output.budget-output.capacity))
		if err != nil {
			return model.Value{}, err
		}
		if converted.IsNull || converted.Type != model.TypeString {
			return model.Value{}, sqlError("XX000", "record field output did not return cstring")
		}
		temporary := int64(len(converted.StrVal))
		if err := outputBudget(temporary, output.budget-output.capacity); err != nil {
			return model.Value{}, err
		}
		size, quoted, err := recordTextFieldSize(ctx, converted.StrVal)
		if err != nil {
			return model.Value{}, err
		}
		remainingPunctuation := int64(len(codec.fields) - i)
		if err := output.reserveTemporary(ctx, size+remainingPunctuation, temporary); err != nil {
			return model.Value{}, err
		}
		if quoted {
			output.builder.WriteByte('"')
		}
		if err := copyRecordText(ctx, &output.builder, converted.StrVal, quoted); err != nil {
			return model.Value{}, err
		}
		if quoted {
			output.builder.WriteByte('"')
		}
		converted = model.Value{}
	}
	output.builder.WriteByte(')')
	return output.finishTemporary(ctx)
}

func recordTextFieldSize(ctx context.Context, text string) (int64, bool, error) {
	size := int64(len(text))
	quoted := len(text) == 0
	nextCheck := 0
	for i := 0; i < len(text); {
		if i >= nextCheck {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
			nextCheck = i + cancellationChunk
		}
		ch := text[i]
		switch {
		case ch == 0:
			return 0, false, sqlError("22021", "invalid UTF8 text datum")
		case ch >= utf8.RuneSelf:
			r, width := utf8.DecodeRuneInString(text[i:])
			if r == utf8.RuneError && width == 1 {
				return 0, false, sqlError("22021", "invalid UTF8 text datum")
			}
			i += width
			continue
		case ch == '"' || ch == '\\':
			quoted = true
			size++
		case ch == ',' || ch == '(' || ch == ')' || ch == ' ' || (ch >= '\t' && ch <= '\r'):
			quoted = true
		}
		i++
	}
	if quoted {
		size += 2
	}
	return size, quoted, nil
}

func copyRecordText(ctx context.Context, output *strings.Builder, text string, escape bool) error {
	if !escape {
		return copyArrayText(ctx, output, text, false)
	}
	for start := 0; start < len(text); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+cancellationChunk, len(text))
		span := start
		for i := start; i < end; i++ {
			if text[i] == '"' || text[i] == '\\' {
				output.WriteString(text[span:i])
				output.WriteByte(text[i])
				span = i
			}
		}
		output.WriteString(text[span:end])
	}
	return nil
}
