package builtin

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileLikeEscape(signature Signature) scalarKernel {
	switch signature.ID {
	case 1637:
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			pattern, escape := args[0].StrVal, args[1].StrVal
			if _, err := textCharacters(ctx, pattern); err != nil {
				return model.Value{}, err
			}
			characters, err := textCharacters(ctx, escape)
			if err != nil {
				return model.Value{}, err
			}
			if characters > 1 {
				return model.Value{}, sqlError("22025", "invalid escape string")
			}
			if escape == `\` {
				return ownedText(ctx, budget, pattern)
			}
			return normalizeLikeEscapeText(ctx, pattern, escape, budget)
		}
	case 2009:
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			pattern, escape := args[0].BytesVal, args[1].BytesVal
			if len(escape) > 1 {
				return model.Value{}, sqlError("22025", "invalid escape string")
			}
			if len(escape) == 1 && escape[0] == '\\' {
				return ownedBytes(ctx, budget, pattern)
			}
			return normalizeLikeEscapeBytes(ctx, pattern, escape, budget)
		}
	default:
		return nil
	}
}

func normalizeLikeEscapeText(ctx context.Context, pattern, escape string, budget int64) (model.Value, error) {
	var size int64
	var output strings.Builder
	// Reuse the transformation for sizing so multibyte escapes can shrink the output.
	for pass := range 2 {
		if pass == 1 {
			if err := outputBudget(size, budget); err != nil {
				return model.Value{}, err
			}
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			output.Grow(int(size))
		}
		afterEscape, nextCheck := false, 0
		for i := 0; i < len(pattern); {
			if i >= nextCheck {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
				nextCheck = i + cancellationChunk
			}
			_, width := utf8.DecodeRuneInString(pattern[i:])
			fragment := pattern[i : i+width]
			i += width
			if fragment == escape && !afterEscape {
				fragment = `\`
				afterEscape = true
			} else {
				if fragment == `\` && !afterEscape {
					fragment = `\\`
				}
				afterEscape = false
			}
			if pass == 0 {
				size += int64(len(fragment))
			} else {
				output.WriteString(fragment)
			}
		}
	}
	return model.StringVal(output.String()), nil
}

func normalizeLikeEscapeBytes(ctx context.Context, pattern, escape []byte, budget int64) (model.Value, error) {
	var size int64
	var output []byte
	for pass := range 2 {
		if pass == 1 {
			if err := outputBudget(size, budget); err != nil {
				return model.Value{}, err
			}
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			output = make([]byte, int(size))
		}
		afterEscape, position := false, 0
		for i, value := range pattern {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			width := 1
			if len(escape) == 1 && value == escape[0] && !afterEscape {
				value = '\\'
				afterEscape = true
			} else {
				if value == '\\' && !afterEscape {
					width = 2
				}
				afterEscape = false
			}
			if pass == 0 {
				size += int64(width)
			} else {
				output[position] = value
				position++
				if width == 2 {
					output[position] = value
					position++
				}
			}
		}
	}
	return model.BytesVal(output), nil
}
