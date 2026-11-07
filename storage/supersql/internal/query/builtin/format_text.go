package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileFormatText(codec *variadicTextCodec) scalarKernel {
	return codec.format
}

func (codec *variadicTextCodec) format(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	if arguments[0].IsNull {
		return model.NullVal(model.TypeString), nil
	}
	values, err := codec.values(arguments)
	if err != nil {
		return model.Value{}, err
	}
	text := arguments[0].StrVal
	if _, err := textCharacters(ctx, text); err != nil {
		return model.Value{}, err
	}
	output := textBuffer{budget: min(budget, (1<<30)-5)}
	nextArgument, start := 0, 0
	for position := 0; position < len(text); {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		if text[position] != '%' {
			position++
			continue
		}
		if err := appendFormatFragment(ctx, &output, text[start:position], 0); err != nil {
			return model.Value{}, err
		}
		position++
		if position == len(text) {
			return model.Value{}, formatEnd()
		}
		if text[position] == '%' {
			if err := appendFormatFragment(ctx, &output, "%", 0); err != nil {
				return model.Value{}, err
			}
			position++
			start = position
			continue
		}
		spec, end, err := parseFormatDirective(ctx, text, position)
		if err != nil {
			return model.Value{}, err
		}
		width := spec.width
		if spec.indirectWidth {
			index := nextArgument
			if spec.widthArgument >= 0 {
				index = spec.widthArgument
			}
			if index >= len(values.values) {
				return model.Value{}, formatArguments()
			}
			nested, err := callBudget.After(uint64(output.builder.Cap()))
			if err != nil {
				return model.Value{}, err
			}
			width, err = formatWidth(ctx, values, index, nested.WithOutput(output.budget-output.capacity))
			if err != nil {
				return model.Value{}, err
			}
			nextArgument = index + 1
		}
		index := nextArgument
		if spec.argument >= 0 {
			index = spec.argument
		}
		if index >= len(values.values) {
			return model.Value{}, formatArguments()
		}
		nextArgument = index + 1
		if err := appendFormatValue(ctx, &output, values, index, spec, width, callBudget); err != nil {
			return model.Value{}, err
		}
		position, start = end, end
	}
	if err := appendFormatFragment(ctx, &output, text[start:], 0); err != nil {
		return model.Value{}, err
	}
	return output.finishTemporary(ctx)
}

func formatWidth(ctx context.Context, values variadicTextValues, index int, callBudget CallBudget) (int64, error) {
	value := values.values[index]
	if value.IsNull {
		return 0, nil
	}
	typ := values.typ(index)
	if value.Type != typ.Storage {
		return 0, sqlError("42804", "format width storage differs from its SQL type")
	}
	if typ.ID == 21 || typ.ID == 23 {
		if err := integerRange(value.IntVal, typ.ID); err != nil {
			return 0, err
		}
		return value.IntVal, nil
	}
	text, err := values.text(ctx, index, callBudget)
	if err != nil {
		return 0, err
	}
	parsed, err := parseInteger(ctx, text.StrVal, 23)
	if err != nil {
		return 0, err
	}
	return parsed.IntVal, nil
}

func appendFormatValue(ctx context.Context, output *textBuffer, values variadicTextValues, index int, spec formatDirective, width int64, callBudget CallBudget) error {
	value := values.values[index]
	if value.IsNull {
		switch spec.conversion {
		case 's':
			return appendFormatPadded(ctx, output, "", 0, width, spec.left)
		case 'L':
			return appendFormatPadded(ctx, output, "NULL", 0, width, spec.left)
		default:
			return sqlError("22004", "null values cannot be formatted as an SQL identifier")
		}
	}
	nested, err := callBudget.After(uint64(output.builder.Cap()))
	if err != nil {
		return err
	}
	rendered, err := values.text(ctx, index, nested.WithOutput(output.budget-output.capacity))
	if err != nil {
		return err
	}
	available := output.budget - output.capacity - int64(len(rendered.StrVal))
	switch spec.conversion {
	case 'I':
		quoted, err := quoteIdentifierText(ctx, rendered.StrVal, available)
		if err != nil {
			return err
		}
		rendered = quoted
	case 'L':
		quoted, err := quoteLiteralText(ctx, rendered.StrVal, uint64(available))
		if err != nil {
			return err
		}
		rendered = quoted
	}
	return appendFormatPadded(ctx, output, rendered.StrVal, int64(len(rendered.StrVal)), width, spec.left)
}

func appendFormatFragment(ctx context.Context, output *textBuffer, text string, temporary int64) error {
	if err := output.reserveTemporary(ctx, int64(len(text)), temporary); err != nil {
		return err
	}
	return copyArrayText(ctx, &output.builder, text, false)
}

func appendFormatPadded(ctx context.Context, output *textBuffer, text string, temporary, width int64, left bool) error {
	if width == math.MinInt32 {
		return sqlError("22003", "number is out of range")
	}
	if width < 0 {
		width = -width
		left = true
	}
	characters, err := textCharacters(ctx, text)
	if err != nil {
		return err
	}
	padding := max(int64(0), width-characters)
	if err := output.reserveTemporary(ctx, int64(len(text))+padding, temporary); err != nil {
		return err
	}
	if !left {
		if err := writeTextSpaces(ctx, &output.builder, padding); err != nil {
			return err
		}
	}
	if err := copyArrayText(ctx, &output.builder, text, false); err != nil {
		return err
	}
	if left {
		return writeTextSpaces(ctx, &output.builder, padding)
	}
	return nil
}
