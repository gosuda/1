package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileOverlay(signature Signature) scalarKernel {
	switch signature.ID {
	case 1404, 1405:
		return overlayText
	case 749, 752:
		return overlayBytes
	default:
		return nil
	}
}

func overlayText(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	source, replacement := args[0].StrVal, args[1].StrVal
	size, err := textCharacters(ctx, source)
	if err != nil {
		return model.Value{}, err
	}
	length, err := textCharacters(ctx, replacement)
	if err != nil {
		return model.Value{}, err
	}
	if len(args) == 4 {
		length = args[3].IntVal
	}
	prefixEnd, suffixStart, err := overlayBounds(size, args[2].IntVal, length)
	if err != nil {
		return model.Value{}, err
	}
	firstCharacter, lastCharacter := min(prefixEnd, suffixStart), max(prefixEnd, suffixStart)
	first, err := textByteOffset(ctx, source, firstCharacter)
	if err != nil {
		return model.Value{}, err
	}
	last, err := textByteOffset(ctx, source[first:], lastCharacter-firstCharacter)
	if err != nil {
		return model.Value{}, err
	}
	last += first
	if prefixEnd > suffixStart {
		first, last = last, first
	}
	return ownedText(ctx, budget, source[:first], replacement, source[last:])
}

func overlayBytes(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	source, replacement := args[0].BytesVal, args[1].BytesVal
	length := int64(len(replacement))
	if len(args) == 4 {
		length = args[3].IntVal
	}
	prefixEnd, suffixStart, err := overlayBounds(int64(len(source)), args[2].IntVal, length)
	if err != nil {
		return model.Value{}, err
	}
	return ownedBytes(ctx, budget, source[:prefixEnd], replacement, source[suffixStart:])
}

func overlayBounds(size, start, length int64) (prefixEnd, suffixStart int64, err error) {
	if start <= 0 {
		return 0, 0, sqlError("22011", "negative substring length not allowed")
	}
	end := start + length
	if err := integerRange(end, 23); err != nil {
		return 0, 0, err
	}
	// PostgreSQL concatenates two independent substrings: a negative length
	// moves the suffix backward, duplicating source characters rather than failing.
	return min(start-1, size), min(max(end-1, 0), size), nil
}
