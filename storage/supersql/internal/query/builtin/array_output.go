package builtin

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func formatArrayText(ctx context.Context, value model.Value, delimiter byte, element *arrayElementCall, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
		return model.Value{}, sqlError("2202E", "invalid array dimensions")
	}
	if len(value.ArrayVal) == 0 {
		return ownedText(ctx, budget, "{}")
	}
	if delimiter == 0 || delimiter >= utf8.RuneSelf {
		return model.Value{}, sqlError("22021", "invalid UTF8 array delimiter")
	}
	output := textBuffer{budget: min(budget, (1<<30)-5)}
	for _, dimension := range value.Dimensions {
		if dimension.LowerBound == 1 {
			continue
		}
		// Six dimensions with two signed int32 bounds and three separators each.
		var scratch [6*25 + 1]byte
		bounds := scratch[:0]
		for _, dimension := range value.Dimensions {
			bounds = append(bounds, '[')
			bounds = strconv.AppendInt(bounds, int64(dimension.LowerBound), 10)
			bounds = append(bounds, ':')
			bounds = strconv.AppendInt(bounds, int64(dimension.LowerBound)+int64(dimension.Length)-1, 10)
			bounds = append(bounds, ']')
		}
		bounds = append(bounds, '=')
		if err := output.reserve(ctx, int64(len(bounds))); err != nil {
			return model.Value{}, err
		}
		output.builder.Write(bounds)
		break
	}
	var positions [6]int32
	depth := 0
	for _, datum := range value.ArrayVal {
		for ; depth < len(value.Dimensions); depth++ {
			if err := output.writeByte(ctx, '{'); err != nil {
				return model.Value{}, err
			}
		}
		if err := output.writeElement(ctx, datum, delimiter, element, callBudget); err != nil {
			return model.Value{}, err
		}
		for depth = len(value.Dimensions) - 1; depth >= 0; depth-- {
			positions[depth]++
			if positions[depth] < value.Dimensions[depth].Length {
				if err := output.writeByte(ctx, delimiter); err != nil {
					return model.Value{}, err
				}
				break
			}
			positions[depth] = 0
			if err := output.writeByte(ctx, '}'); err != nil {
				return model.Value{}, err
			}
		}
		depth++
	}
	return output.finish(ctx)
}

func (output *textBuffer) writeElement(ctx context.Context, datum model.Value, delimiter byte, element *arrayElementCall, callBudget CallBudget) error {
	if datum.IsNull {
		if err := output.reserve(ctx, 4); err != nil {
			return err
		}
		output.builder.WriteString("NULL")
		return nil
	}
	nested, err := callBudget.After(uint64(output.builder.Cap()))
	if err != nil {
		return err
	}
	value, err := element.invoke(ctx, datum, nested.WithOutput(output.budget-int64(output.builder.Len())))
	if err != nil {
		return err
	}
	if value.IsNull || value.Type != model.TypeString {
		return sqlError("XX000", "array element output did not return cstring")
	}
	size, quoted, err := arrayTextElementSize(ctx, value.StrVal, delimiter)
	if err != nil {
		return err
	}
	if err := output.reserve(ctx, size); err != nil {
		return err
	}
	if quoted {
		output.builder.WriteByte('"')
	}
	if err := copyArrayText(ctx, &output.builder, value.StrVal, quoted); err != nil {
		return err
	}
	if quoted {
		output.builder.WriteByte('"')
	}
	return nil
}

func arrayTextElementSize(ctx context.Context, text string, delimiter byte) (int64, bool, error) {
	size := int64(len(text))
	quoted := len(text) == 0 || (len(text) == 4 && strings.EqualFold(text, "NULL"))
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
		case ch == delimiter || ch == '{' || ch == '}' || ch == ' ' || (ch >= '\t' && ch <= '\r'):
			quoted = true
		}
		i++
	}
	if quoted {
		size += 2
	}
	return size, quoted, nil
}
