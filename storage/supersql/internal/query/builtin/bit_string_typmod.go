package builtin

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func bitStringTypmodIn(ctx context.Context, argument model.Value, varying bool) (model.Value, error) {
	if len(argument.Dimensions) != 1 {
		return model.Value{}, sqlError("2202E", "typmod array must be one-dimensional")
	}
	if err := model.ValidateArrayShape(argument.Dimensions, uint64(len(argument.ArrayVal))); err != nil {
		return model.Value{}, sqlError("2202E", "invalid array dimensions")
	}
	for i, item := range argument.ArrayVal {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		if item.IsNull {
			return model.Value{}, sqlError("22004", "typmod array must not contain nulls")
		}
	}
	var modifier int64
	// PostgreSQL parses every modifier before checking the modifier count.
	for _, item := range argument.ArrayVal {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if item.Type != model.TypeString {
			return model.Value{}, sqlError("2202E", "typmod array must be type cstring[]")
		}
		text := item.StrVal
		for start := 0; start < len(text); start += 4096 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			if end := strings.IndexByte(text[start:min(start+4096, len(text))], 0); end >= 0 {
				text = text[:start+end]
				break
			}
		}
		value, err := parseInteger(ctx, text, 23)
		if err != nil {
			return model.Value{}, err
		}
		modifier = value.IntVal
	}
	if len(argument.ArrayVal) != 1 {
		return model.Value{}, sqlError("22023", "invalid type modifier")
	}
	name := "bit"
	if varying {
		name = "bit varying"
	}
	if modifier < 1 {
		return model.Value{}, sqlError("22023", fmt.Sprintf("length for type %s must be at least 1", name))
	}
	if modifier > 83886080 {
		return model.Value{}, sqlError("22023", fmt.Sprintf("length for type %s cannot exceed 83886080", name))
	}
	return model.Int64Val(modifier), nil
}

func bitStringTypmodOut(ctx context.Context, modifier int32, budget CallBudget) (model.Value, error) {
	var storage [16]byte
	text := storage[:0]
	if modifier >= 0 {
		text = append(text, '(')
		text = strconv.AppendInt(text, int64(modifier), 10)
		text = append(text, ')')
	}
	return bitStringOutput(ctx, int64(len(text)), budget, func() (model.Value, error) {
		return model.StringVal(string(text)), nil
	})
}
