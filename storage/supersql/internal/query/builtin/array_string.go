package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileArrayToString(signature Signature, element Callable) scalarKernel {
	switch signature.ID {
	case 384, 395:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		if args[0].IsNull || args[1].IsNull {
			return model.NullVal(model.TypeString), nil
		}
		array, delimiter := args[0], args[1].StrVal
		if err := model.ValidateArrayShape(array.Dimensions, uint64(len(array.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		if _, err := textCharacters(ctx, delimiter); err != nil {
			return model.Value{}, err
		}
		includeNull := signature.ID == 384 && !args[2].IsNull
		nullText := ""
		if includeNull {
			nullText = args[2].StrVal
			if _, err := textCharacters(ctx, nullText); err != nil {
				return model.Value{}, err
			}
		}
		output := textBuffer{budget: min(budget, (1<<30)-5)}
		elementCall := arrayElementCall{function: element}
		printed := false
		for i, datum := range array.ArrayVal {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			if datum.IsNull && !includeNull {
				continue
			}
			separatorBytes := int64(0)
			if printed {
				separatorBytes = int64(len(delimiter))
			}
			remaining := output.budget - int64(output.builder.Len())
			if err := outputBudget(separatorBytes, remaining); err != nil {
				return model.Value{}, err
			}
			text := nullText
			if !datum.IsNull {
				nested, err := callBudget.After(uint64(output.builder.Cap()))
				if err != nil {
					return model.Value{}, err
				}
				value, err := elementCall.invoke(ctx, datum, nested.WithOutput(remaining-separatorBytes))
				if err != nil {
					return model.Value{}, err
				}
				if value.IsNull || value.Type != model.TypeString {
					return model.Value{}, sqlError("XX000", "array element output did not return cstring")
				}
				text = value.StrVal
				if _, err := textCharacters(ctx, text); err != nil {
					return model.Value{}, err
				}
			}
			if err := output.reserve(ctx, separatorBytes+int64(len(text))); err != nil {
				return model.Value{}, err
			}
			if printed {
				if err := copyArrayText(ctx, &output.builder, delimiter, false); err != nil {
					return model.Value{}, err
				}
			}
			if err := copyArrayText(ctx, &output.builder, text, false); err != nil {
				return model.Value{}, err
			}
			printed = true
		}
		return output.finish(ctx)
	}
}
