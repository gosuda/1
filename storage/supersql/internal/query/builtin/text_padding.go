package builtin

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type textTrimEdges uint8

const (
	textTrimLeading textTrimEdges = 1 << iota
	textTrimTrailing
)

func compileTextPadding(signature Signature) scalarKernel {
	switch signature.ID {
	case 873, 874, 879, 880:
		prepend := signature.ID == 873 || signature.ID == 879
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			fill := " "
			if len(args) == 3 {
				fill = args[2].StrVal
			}
			return padText(ctx, args[0].StrVal, fill, max(0, args[1].IntVal), budget, prepend)
		}
	case 875, 876, 881, 882, 884, 885, 2015, 6195, 6196:
		edges := textTrimLeading | textTrimTrailing
		switch signature.ID {
		case 875, 881, 6195:
			edges = textTrimLeading
		case 876, 882, 6196:
			edges = textTrimTrailing
		}
		if signature.Arguments[0] == 17 {
			return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
				budget := callBudget.OutputBytes
				return trimBytes(ctx, args[0].BytesVal, args[1].BytesVal, edges, budget)
			}
		}
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			set := " "
			if len(args) == 2 {
				set = args[1].StrVal
			}
			return trimText(ctx, args[0].StrVal, set, edges, budget)
		}
	default:
		return nil
	}
}

func padText(ctx context.Context, text, fill string, length, budget int64, prepend bool) (model.Value, error) {
	characters, err := textCharacters(ctx, text)
	if err != nil {
		return model.Value{}, err
	}
	fillCharacters, err := textCharacters(ctx, fill)
	if err != nil {
		return model.Value{}, err
	}
	if length < characters {
		end, err := textByteOffset(ctx, text, length)
		if err != nil {
			return model.Value{}, err
		}
		return ownedText(ctx, budget, text[:end])
	}
	if length == characters || fillCharacters == 0 {
		return ownedText(ctx, budget, text)
	}
	padding := length - characters
	partial, err := textByteOffset(ctx, fill, padding%fillCharacters)
	if err != nil {
		return model.Value{}, err
	}
	repetitions := padding / fillCharacters
	if repetitions > budget/int64(len(fill)) {
		return model.Value{}, sqlError("54000", "requested padding exceeds output byte budget")
	}
	paddingBytes := repetitions * int64(len(fill))
	if int64(partial) > budget-paddingBytes {
		return model.Value{}, sqlError("54000", "requested padding exceeds output byte budget")
	}
	paddingBytes += int64(partial)
	if int64(len(text)) > budget-paddingBytes {
		return model.Value{}, sqlError("54000", "requested padding exceeds output byte budget")
	}
	size := int64(len(text)) + paddingBytes
	if err := outputBudget(size, budget); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(int(size))
	for part := range 2 {
		piece, partBytes := text, int64(len(text))
		if (part == 0) == prepend {
			piece, partBytes = fill, paddingBytes
		}
		start := output.Len()
		for written := int64(0); written < partBytes; {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			var next string
			if written < int64(len(piece)) {
				next = piece[written:]
			} else {
				// Grow reserved the final size, so appending cannot invalidate this prefix.
				next = output.String()[start+int(written%int64(len(piece))):]
			}
			next = next[:min(int64(len(next)), partBytes-written, cancellationChunk)]
			output.WriteString(next)
			written += int64(len(next))
		}
	}
	return model.StringVal(output.String()), nil
}

func trimText(ctx context.Context, text, set string, edges textTrimEdges, budget int64) (model.Value, error) {
	if _, err := textCharacters(ctx, text); err != nil {
		return model.Value{}, err
	}
	if _, err := textCharacters(ctx, set); err != nil {
		return model.Value{}, err
	}
	begin, end, work := 0, len(text), 0
	if edges&textTrimLeading != 0 {
		for begin < end {
			character, width := utf8.DecodeRuneInString(text[begin:end])
			member, err := textTrimContains(ctx, set, character, &work)
			if err != nil {
				return model.Value{}, err
			}
			if !member {
				break
			}
			begin += width
		}
	}
	if edges&textTrimTrailing != 0 {
		for begin < end {
			character, width := utf8.DecodeLastRuneInString(text[begin:end])
			member, err := textTrimContains(ctx, set, character, &work)
			if err != nil {
				return model.Value{}, err
			}
			if !member {
				break
			}
			end -= width
		}
	}
	return ownedText(ctx, budget, text[begin:end])
}

func textTrimContains(ctx context.Context, set string, character rune, work *int) (bool, error) {
	for _, candidate := range set {
		if *work%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		(*work)++
		if candidate == character {
			return true, nil
		}
	}
	return false, nil
}

func trimBytes(ctx context.Context, input, set []byte, edges textTrimEdges, budget int64) (model.Value, error) {
	if len(input) == 0 || len(set) == 0 {
		return ownedBytes(ctx, budget, input)
	}
	var members [4]uint64
	for i, b := range set {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		members[b/64] |= uint64(1) << (b % 64)
	}
	begin, end := 0, len(input)
	if edges&textTrimLeading != 0 {
		for begin < end && members[input[begin]/64]&(uint64(1)<<(input[begin]%64)) != 0 {
			if begin%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			begin++
		}
	}
	if edges&textTrimTrailing != 0 {
		for begin < end && members[input[end-1]/64]&(uint64(1)<<(input[end-1]%64)) != 0 {
			if (len(input)-end)%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			end--
		}
	}
	return ownedBytes(ctx, budget, input[begin:end])
}
