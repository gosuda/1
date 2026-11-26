package builtin

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const nameBytes = 63

func compileNameIO(signature Signature) scalarKernel {
	if signature.ID == 266 {
		return nameConcatOID
	}
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		text := args[0].StrVal
		end := min(len(text), nameBytes)
		if end < len(text) {
			for end > 0 && !utf8.RuneStart(text[end]) {
				end--
			}
		}
		text = text[:end]
		if end := strings.IndexByte(text, 0); end >= 0 {
			text = text[:end]
		}
		if _, err := textCharacters(ctx, text); err != nil {
			return model.Value{}, err
		}
		if signature.ID == 409 {
			// bpchar_name clips first, then removes blanks from the retained prefix.
			var err error
			text, err = trimCharacterSpaces(ctx, text)
			if err != nil {
				return model.Value{}, err
			}
		}
		return ownedText(ctx, budget, text)
	}
}

func nameConcatOID(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
	text := args[0].StrVal
	if end := strings.IndexByte(text, 0); end >= 0 {
		text = text[:end]
	}
	if _, err := textCharacters(ctx, text); err != nil {
		return model.Value{}, err
	}
	var suffix [11]byte
	suffix[0] = '_'
	tail := strconv.AppendUint(suffix[:1], uint64(uint32(args[1].IntVal)), 10)
	end := min(len(text), nameBytes-len(tail))
	for end < len(text) && end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	size := end + len(tail)
	if err := outputBudget(int64(size), budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	if uint64(size) > budget.MemoryBytes {
		return model.Value{}, sqlError("54000", "builtin invocation memory budget exceeded")
	}
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(uint64(size))
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	var output [nameBytes]byte
	copy(output[:], text[:end])
	copy(output[end:], tail)
	return model.StringVal(string(output[:size])), nil
}
