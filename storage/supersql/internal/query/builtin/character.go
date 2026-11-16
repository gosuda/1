package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileCharacter(signature Signature, op primitiveOp) scalarKernel {
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		text := args[0].StrVal
		characters, err := textCharacters(ctx, text)
		if err != nil {
			return model.Value{}, err
		}
		switch op {
		case opInput:
			return characterTypmod(ctx, text, characters, args[2].IntVal, false, signature.Result, budget)
		case opTypmod:
			return characterTypmod(ctx, text, characters, args[1].IntVal, args[2].IntVal != 0, signature.Result, budget)
		case opOutput:
			return ownedText(ctx, budget, text)
		case opCast:
			text, err = trimCharacterSpaces(ctx, text)
			if err != nil {
				return model.Value{}, err
			}
			return ownedText(ctx, budget, text)
		}
		if !isComparison(op) {
			return model.Value{}, sqlError("XX000", "invalid character kernel operation")
		}
		right := args[1].StrVal
		if _, err := textCharacters(ctx, right); err != nil {
			return model.Value{}, err
		}
		text, err = trimCharacterSpaces(ctx, text)
		if err != nil {
			return model.Value{}, err
		}
		right, err = trimCharacterSpaces(ctx, right)
		if err != nil {
			return model.Value{}, err
		}
		order, err := compareText(ctx, text, right)
		if err != nil {
			return model.Value{}, err
		}
		return comparisonResult(order, op), nil
	}
}

func characterTypmod(ctx context.Context, text string, characters, typmod int64, explicit bool, target model.TypeID, budget int64) (model.Value, error) {
	if typmod < 4 {
		return ownedText(ctx, budget, text)
	}
	limit := typmod - 4
	if characters > limit {
		end, err := textByteOffset(ctx, text, limit)
		if err != nil {
			return model.Value{}, err
		}
		if !explicit {
			if err := characterSpaceSuffix(ctx, text[end:]); err != nil {
				return model.Value{}, err
			}
		}
		return ownedText(ctx, budget, text[:end])
	}
	if target != 1042 || characters == limit {
		return ownedText(ctx, budget, text)
	}
	return paddedCharacterText(ctx, text, limit-characters, budget)
}

func paddedCharacterText(ctx context.Context, text string, padding, budget int64) (model.Value, error) {
	size := int64(len(text)) + padding
	if err := outputBudget(size, budget); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(int(size))
	for start := 0; start < len(text); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		output.WriteString(text[start:min(start+cancellationChunk, len(text))])
	}
	if err := writeTextSpaces(ctx, &output, padding); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(output.String()), nil
}

func writeTextSpaces(ctx context.Context, output *strings.Builder, padding int64) error {
	const spaces = "                                                                "
	for padding > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := min(padding, int64(len(spaces)))
		output.WriteString(spaces[:chunk])
		padding -= chunk
	}
	return nil
}

func trimCharacterSpaces(ctx context.Context, text string) (string, error) {
	end := len(text)
	for end > 0 && text[end-1] == ' ' {
		if (len(text)-end)%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		end--
	}
	return text[:end], nil
}

func characterSpaceSuffix(ctx context.Context, text string) error {
	for i := range len(text) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if text[i] != ' ' {
			return sqlError("22001", "value too long for type character")
		}
	}
	return nil
}
