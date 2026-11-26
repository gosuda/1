package builtin

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTextSearch(signature Signature, options ResolveOptions) scalarKernel {
	switch signature.ID {
	case 849, 868, 2014, 2087, 878, 2088:
	default:
		return nil
	}
	collation := options.Collation
	if collation == 100 {
		collation = options.DefaultCollation
	}
	indeterminate := signature.ID != 2014 && signature.ID != 878 && collation != 950 && collation != 951
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		if indeterminate {
			return model.Value{}, sqlError("42P22", "text search requires a resolved C or POSIX collation")
		}
		if signature.ID == 2014 {
			position, err := searchSubstring(ctx, args[0].BytesVal, args[1].BytesVal, 0)
			if err != nil {
				return model.Value{}, err
			}
			if err := integerRange(int64(position)+1, 23); err != nil {
				return model.Value{}, err
			}
			return model.Int64Val(int64(position) + 1), nil
		}
		for i, argument := range args {
			if signature.Arguments[i] == 25 {
				if _, err := textCharacters(ctx, argument.StrVal); err != nil {
					return model.Value{}, err
				}
			}
		}
		switch signature.ID {
		case 849, 868:
			position, err := searchSubstring(ctx, args[0].StrVal, args[1].StrVal, 0)
			if err != nil {
				return model.Value{}, err
			}
			if position < 0 {
				return model.Int64Val(0), nil
			}
			characters, err := textCharacters(ctx, args[0].StrVal[:position])
			if err != nil {
				return model.Value{}, err
			}
			if err := integerRange(characters+1, 23); err != nil {
				return model.Value{}, err
			}
			return model.Int64Val(characters + 1), nil
		case 2087:
			return replaceText(ctx, args[0].StrVal, args[1].StrVal, args[2].StrVal, budget)
		case 878:
			return translateText(ctx, args[0].StrVal, args[1].StrVal, args[2].StrVal, budget)
		default:
			return splitTextPart(ctx, args[0].StrVal, args[1].StrVal, args[2].IntVal, budget)
		}
	}
}

// searchSubstring returns the first index >= start, or -1.
func searchSubstring[T ~string | ~[]byte](ctx context.Context, input, needle T, start int) (int, error) {
	width := len(needle)
	if width == 0 {
		return start, nil
	}
	if width > len(input)-start {
		return -1, nil
	}
	const prime uint64 = 16777619
	var wanted, hash uint64
	power := uint64(1)
	for i := range width {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return -1, err
			}
		}
		wanted = wanted*prime + uint64(needle[i])
		hash = hash*prime + uint64(input[start+i])
		power *= prime
	}
	for position := start; ; position++ {
		if (position-start)%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return -1, err
			}
		}
		if hash == wanted {
			matched, err := equalSearchWindow(ctx, input[position:position+width], needle)
			if err != nil {
				return -1, err
			}
			if matched {
				return position, nil
			}
		}
		if position == len(input)-width {
			return -1, nil
		}
		hash = hash*prime + uint64(input[position+width]) - power*uint64(input[position])
	}
}

func equalSearchWindow[T ~string | ~[]byte](ctx context.Context, input, needle T) (bool, error) {
	for i := range len(needle) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if input[i] != needle[i] {
			return false, nil
		}
	}
	return true, nil
}

func replaceText(ctx context.Context, input, from, to string, budget int64) (model.Value, error) {
	if from == "" || input == "" {
		return ownedText(ctx, budget, input)
	}
	var size int64
	for start := 0; ; {
		position, err := searchSubstring(ctx, input, from, start)
		if err != nil {
			return model.Value{}, err
		}
		if position < 0 {
			if start == 0 {
				return ownedText(ctx, budget, input)
			}
			size += int64(len(input) - start)
			break
		}
		size += int64(position - start)
		if err := outputBudget(size, budget); err != nil {
			return model.Value{}, err
		}
		size += int64(len(to))
		if err := outputBudget(size, budget); err != nil {
			return model.Value{}, err
		}
		start = position + len(from)
	}
	if err := outputBudget(size, budget); err != nil {
		return model.Value{}, err
	}
	if size == 0 {
		return ownedText(ctx, budget, "")
	}
	var output strings.Builder
	output.Grow(int(size))
	for start := 0; ; {
		position, err := searchSubstring(ctx, input, from, start)
		if err != nil {
			return model.Value{}, err
		}
		if position < 0 {
			if err := writeSearchText(ctx, &output, input[start:]); err != nil {
				return model.Value{}, err
			}
			break
		}
		if err := writeSearchText(ctx, &output, input[start:position]); err != nil {
			return model.Value{}, err
		}
		if err := writeSearchText(ctx, &output, to); err != nil {
			return model.Value{}, err
		}
		start = position + len(from)
	}
	return model.StringVal(output.String()), nil
}

func writeSearchText(ctx context.Context, output *strings.Builder, text string) error {
	for start := 0; start < len(text); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		output.WriteString(text[start:min(start+cancellationChunk, len(text))])
	}
	return nil
}

func translateText(ctx context.Context, input, from, to string, budget int64) (model.Value, error) {
	if from == "" || input == "" {
		return ownedText(ctx, budget, input)
	}
	var output strings.Builder
	for pass := range 2 {
		var size int64
		nextCheck := 0
		for position := 0; position < len(input); {
			if position >= nextCheck {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
				nextCheck = position + cancellationChunk
			}
			_, width := utf8.DecodeRuneInString(input[position:])
			fragment, err := translatedCharacter(ctx, input[position:position+width], from, to)
			if err != nil {
				return model.Value{}, err
			}
			if pass == 0 {
				size += int64(len(fragment))
				if err := outputBudget(size, budget); err != nil {
					return model.Value{}, err
				}
			} else {
				output.WriteString(fragment)
			}
			position += width
		}
		if pass == 0 {
			if size == 0 {
				return ownedText(ctx, budget, "")
			}
			output.Grow(int(size))
		}
	}
	return model.StringVal(output.String()), nil
}

func translatedCharacter(ctx context.Context, character, from, to string) (string, error) {
	var index int64
	nextCheck := 0
	for position := 0; position < len(from); {
		if position >= nextCheck {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			nextCheck = position + cancellationChunk
		}
		_, width := utf8.DecodeRuneInString(from[position:])
		if from[position:position+width] == character {
			offset, err := textByteOffset(ctx, to, index)
			if err != nil {
				return "", err
			}
			if offset == len(to) {
				return "", nil
			}
			_, replacementWidth := utf8.DecodeRuneInString(to[offset:])
			return to[offset : offset+replacementWidth], nil
		}
		position += width
		index++
	}
	return character, nil
}

func splitTextPart(ctx context.Context, input, separator string, field, budget int64) (model.Value, error) {
	if field == 0 {
		return model.Value{}, sqlError("22023", "field position must not be zero")
	}
	if separator == "" {
		if field == 1 || field == -1 {
			return ownedText(ctx, budget, input)
		}
		return ownedText(ctx, budget, "")
	}
	if field < 0 {
		fields, start := int64(1), 0
		for {
			position, err := searchSubstring(ctx, input, separator, start)
			if err != nil {
				return model.Value{}, err
			}
			if position < 0 {
				break
			}
			fields++
			start = position + len(separator)
		}
		if field == -1 {
			return ownedText(ctx, budget, input[start:])
		}
		field += fields + 1
		if field <= 0 {
			return ownedText(ctx, budget, "")
		}
	}
	for start := 0; ; field-- {
		position, err := searchSubstring(ctx, input, separator, start)
		if err != nil {
			return model.Value{}, err
		}
		if position < 0 {
			if field == 1 {
				return ownedText(ctx, budget, input[start:])
			}
			return ownedText(ctx, budget, "")
		}
		if field == 1 {
			return ownedText(ctx, budget, input[start:position])
		}
		start = position + len(separator)
	}
}
