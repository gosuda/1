package builtin

import (
	"context"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileUnicodeInfo(signature Signature) scalarKernel {
	switch signature.ID {
	case 4549:
		return unicodeVersion
	case 6105:
		return unicodeAssigned
	default:
		return nil
	}
}

func unicodeVersion(_ context.Context, _ []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	if err := outputBudget(int64(len(unicodeDataVersion)), budget); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(unicodeDataVersion), nil
}

func unicodeAssigned(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	text := args[0].StrVal
	checkpoint := 0
	for offset := 0; offset < len(text); {
		if offset >= checkpoint {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			checkpoint = offset + 4096
		}
		if text[offset] == 0 {
			break
		}
		value, width := utf8.DecodeRuneInString(text[offset:])
		if value == utf8.RuneError && width == 1 {
			return model.Value{}, sqlError("22021", "invalid UTF8 sequence")
		}
		if !unicodeAssignedCodepoint(value) {
			return model.BoolVal(false), nil
		}
		offset += width
	}
	return model.BoolVal(true), ctx.Err()
}
