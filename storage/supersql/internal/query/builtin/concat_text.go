package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileConcatText(codec *variadicTextCodec) scalarKernel {
	switch codec.signature.ID {
	case 3058, 3059:
	default:
		return nil
	}
	return func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		separator := ""
		if codec.prefix != 0 {
			if arguments[0].IsNull {
				return model.NullVal(model.TypeString), nil
			}
			separator = arguments[0].StrVal
			if _, err := textCharacters(ctx, separator); err != nil {
				return model.Value{}, err
			}
		}
		values, err := codec.values(arguments)
		if err != nil {
			return model.Value{}, err
		}
		if values.nullArray {
			return model.NullVal(model.TypeString), nil
		}
		output := textBuffer{budget: min(budget, (1<<30)-5)}
		printed := false
		for i, datum := range values.values {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			if datum.IsNull {
				continue
			}
			nested, err := callBudget.After(uint64(output.builder.Cap()))
			if err != nil {
				return model.Value{}, err
			}
			converted, err := values.text(ctx, i, nested.WithOutput(output.budget-output.capacity))
			if err != nil {
				return model.Value{}, err
			}
			temporary := int64(len(converted.StrVal))
			additional := temporary
			if printed {
				additional += int64(len(separator))
			}
			if err := output.reserveTemporary(ctx, additional, temporary); err != nil {
				return model.Value{}, err
			}
			if printed {
				if err := copyArrayText(ctx, &output.builder, separator, false); err != nil {
					return model.Value{}, err
				}
			}
			if err := copyArrayText(ctx, &output.builder, converted.StrVal, false); err != nil {
				return model.Value{}, err
			}
			converted = model.Value{}
			printed = true
		}
		return output.finishTemporary(ctx)
	}
}
