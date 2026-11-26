package builtin

import (
	"context"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileStringToArray(signature Signature, options ResolveOptions) scalarKernel {
	switch signature.ID {
	case 376, 394:
	default:
		return nil
	}
	collation := options.Collation
	if collation == 100 {
		collation = options.DefaultCollation
	}
	indeterminate := collation != 950 && collation != 951
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		if args[0].IsNull {
			return model.NullVal(model.TypeArray), nil
		}
		if args[0].StrVal == "" {
			return model.Value{Type: model.TypeArray}, nil
		}
		fields := stringArrayFields{input: args[0].StrVal, characters: args[1].IsNull}
		if !args[1].IsNull {
			fields.delimiter = args[1].StrVal
		}
		if signature.ID == 376 && !args[2].IsNull {
			fields.replaceNulls = true
			fields.nullMarker = args[2].StrVal
		}
		// PostgreSQL resolves collation only when searching or comparing a field.
		if indeterminate && (fields.delimiter != "" || fields.replaceNulls) {
			return model.Value{}, sqlError("42P22", "text search requires a resolved C or POSIX collation")
		}
		for _, argument := range args {
			if !argument.IsNull {
				if _, err := textCharacters(ctx, argument.StrVal); err != nil {
					return model.Value{}, err
				}
			}
		}
		budget = min(budget, arrayInputMaxBytes)
		sizing := fields
		count, payload, total := int64(0), int64(0), arrayDimensionBytes
		for !sizing.done {
			field, err := sizing.next(ctx)
			if err != nil {
				return model.Value{}, err
			}
			if count == math.MaxInt32 {
				return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
			}
			if err := outputBudget(arrayValueBytes, budget-total); err != nil {
				return model.Value{}, err
			}
			fieldBytes := int64(len(field.StrVal))
			if err := outputBudget(fieldBytes, budget-total-arrayValueBytes); err != nil {
				return model.Value{}, err
			}
			count++
			payload += fieldBytes
			total += arrayValueBytes + fieldBytes
		}
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		var arena strings.Builder
		arena.Grow(int(payload))
		values := make([]model.Value, int(count))
		for i := range values {
			field, err := fields.next(ctx)
			if err != nil {
				return model.Value{}, err
			}
			if !field.IsNull {
				start := arena.Len()
				if err := copyArrayText(ctx, &arena, field.StrVal, false); err != nil {
					return model.Value{}, err
				}
				field.StrVal = arena.String()[start:]
			}
			values[i] = field
		}
		return model.Value{
			Type: model.TypeArray, ArrayVal: values,
			Dimensions: []model.ArrayDimension{{Length: int32(count), LowerBound: 1}},
		}, nil
	}
}

type stringArrayFields struct {
	input, delimiter, nullMarker string
	position                     int
	characters, replaceNulls     bool
	done                         bool
}

func (fields *stringArrayFields) next(ctx context.Context) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	start, end := fields.position, len(fields.input)
	switch {
	case fields.characters:
		_, width := utf8.DecodeRuneInString(fields.input[start:])
		end = start + width
		fields.position = end
		fields.done = end == len(fields.input)
	case fields.delimiter == "":
		fields.done = true
	default:
		match, err := searchSubstring(ctx, fields.input, fields.delimiter, start)
		if err != nil {
			return model.Value{}, err
		}
		if match < 0 {
			fields.done = true
		} else {
			end = match
			fields.position = match + len(fields.delimiter)
		}
	}
	field := fields.input[start:end]
	if fields.replaceNulls && len(field) == len(fields.nullMarker) {
		order, err := compareText(ctx, field, fields.nullMarker)
		if err != nil {
			return model.Value{}, err
		}
		if order == 0 {
			return model.NullVal(model.TypeString), nil
		}
	}
	return model.StringVal(field), nil
}
