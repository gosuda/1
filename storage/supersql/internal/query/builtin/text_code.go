package builtin

import (
	"context"
	"math/bits"
	"strconv"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTextCode(signature Signature) scalarKernel {
	switch signature.ID {
	case 1620:
		return textCodePoint
	case 1621:
		return textFromCodePoint
	case 2089, 2090, 6330, 6331, 6332, 6333:
		shift := 4
		if signature.ID == 6330 || signature.ID == 6331 {
			shift = 1
		}
		if signature.ID == 6332 || signature.ID == 6333 {
			shift = 3
		}
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			value := uint64(args[0].IntVal)
			if signature.Arguments[0] == 23 {
				value = uint64(uint32(value))
			}
			size := max(1, (bits.Len64(value)+shift-1)/shift)
			if err := outputBudget(int64(size), budget); err != nil {
				return model.Value{}, err
			}
			return model.StringVal(strconv.FormatUint(value, 1<<shift)), nil
		}
	default:
		return nil
	}
}

func textCodePoint(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	text := args[0].StrVal
	if _, err := textCharacters(ctx, text); err != nil {
		return model.Value{}, err
	}
	if text == "" {
		return model.Int64Val(0), nil
	}
	value, _ := utf8.DecodeRuneInString(text)
	return model.Int64Val(int64(value)), nil
}

func textFromCodePoint(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	value := args[0].IntVal
	if value < 0 {
		return model.Value{}, sqlError("22023", "character number must be positive")
	}
	if value == 0 {
		return model.Value{}, sqlError("54000", "null character not permitted")
	}
	if value > utf8.MaxRune || !utf8.ValidRune(rune(value)) {
		return model.Value{}, sqlError("54000", "requested character not valid for UTF8 encoding")
	}
	if err := outputBudget(int64(utf8.RuneLen(rune(value))), budget); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(string(rune(value))), nil
}
