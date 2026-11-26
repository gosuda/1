package builtin

import (
	"bytes"
	"context"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const cancellationChunk = 4096

func textCharacters(ctx context.Context, text string) (int64, error) {
	var count int64
	nextCheck := 0
	for i := 0; i < len(text); {
		if i >= nextCheck {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			nextCheck = i + cancellationChunk
		}
		r, width := utf8.DecodeRuneInString(text[i:])
		if (r == utf8.RuneError && width == 1) || r == 0 {
			return 0, sqlError("22021", "invalid UTF8 text datum")
		}
		i += width
		count++
	}
	return count, nil
}

func textByteOffset(ctx context.Context, text string, character int64) (int, error) {
	i, nextCheck := 0, 0
	for character > 0 && i < len(text) {
		if i >= nextCheck {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			nextCheck = i + cancellationChunk
		}
		_, width := utf8.DecodeRuneInString(text[i:])
		i += width
		character--
	}
	return i, nil
}

func ownedText(ctx context.Context, budget int64, pieces ...string) (model.Value, error) {
	var size int64
	for _, piece := range pieces {
		size += int64(len(piece))
	}
	if err := outputBudget(size, budget); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(int(size))
	for _, piece := range pieces {
		if err := writeText(ctx, &output, piece); err != nil {
			return model.Value{}, err
		}
	}
	return model.StringVal(output.String()), nil
}

func writeText(ctx context.Context, output *strings.Builder, text string) error {
	for start := 0; start < len(text); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		output.WriteString(text[start:min(start+cancellationChunk, len(text))])
	}
	return nil
}

func ownedBytes(ctx context.Context, budget int64, pieces ...[]byte) (model.Value, error) {
	var size int64
	for _, piece := range pieces {
		size += int64(len(piece))
	}
	if err := outputBudget(size, budget); err != nil {
		return model.Value{}, err
	}
	output := make([]byte, int(size))
	position := 0
	for _, piece := range pieces {
		for start := 0; start < len(piece); start += cancellationChunk {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			position += copy(output[position:], piece[start:min(start+cancellationChunk, len(piece))])
		}
	}
	return model.BytesVal(output), nil
}

func compareText(ctx context.Context, a, b string) (int, error) {
	for i, size := 0, min(len(a), len(b)); i < size; i += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		end := min(i+cancellationChunk, size)
		if order := strings.Compare(a[i:end], b[i:end]); order != 0 {
			return order, nil
		}
	}
	if len(a) < len(b) {
		return -1, nil
	}
	if len(a) > len(b) {
		return 1, nil
	}
	return 0, nil
}

func compareBytes(ctx context.Context, a, b []byte) (int, error) {
	for i, size := 0, min(len(a), len(b)); i < size; i += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		end := min(i+cancellationChunk, size)
		if order := bytes.Compare(a[i:end], b[i:end]); order != 0 {
			return order, nil
		}
	}
	if len(a) < len(b) {
		return -1, nil
	}
	if len(a) > len(b) {
		return 1, nil
	}
	return 0, nil
}

func substringBounds(size, start int64, args []model.Value) (int64, int64, error) {
	begin := min(max(start-1, 0), size)
	if len(args) == 2 {
		return begin, size, nil
	}
	length := args[2].IntVal
	if length < 0 {
		return 0, 0, sqlError("22011", "negative substring length not allowed")
	}
	// The signature bounds start and length to int32, so the int64 sum is safe.
	end := min(max(start+length-1, begin), size)
	return begin, end, nil
}

func compileText(signature Signature, op primitiveOp) scalarKernel {
	text := signature.Arguments[0] == 25
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		size := int64(len(args[0].BytesVal))
		if text {
			var err error
			size, err = textCharacters(ctx, args[0].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			if len(args) > 1 && signature.Arguments[1] == 25 {
				if _, err := textCharacters(ctx, args[1].StrVal); err != nil {
					return model.Value{}, err
				}
			}
		}
		if isComparison(op) {
			var order int
			var err error
			if text {
				order, err = compareText(ctx, args[0].StrVal, args[1].StrVal)
			} else {
				order, err = compareBytes(ctx, args[0].BytesVal, args[1].BytesVal)
			}
			if err != nil {
				return model.Value{}, err
			}
			return comparisonResult(order, op), nil
		}
		switch op {
		case opLength, opOctets:
			if text && op == opOctets {
				size = int64(len(args[0].StrVal))
			}
			if err := integerRange(size, 23); err != nil {
				return model.Value{}, err
			}
			return model.Int64Val(size), nil
		case opCat:
			if text {
				return ownedText(ctx, budget, args[0].StrVal, args[1].StrVal)
			}
			return ownedBytes(ctx, budget, args[0].BytesVal, args[1].BytesVal)
		}
		begin, end := int64(0), size
		n := args[1].IntVal
		switch op {
		case opSubstr:
			var err error
			begin, end, err = substringBounds(size, n, args)
			if err != nil {
				return model.Value{}, err
			}
		case opLeft:
			if n < 0 {
				end = max(0, size+n)
			} else {
				end = min(size, n)
			}
		case opRight:
			if n < 0 {
				begin = min(size, -n)
			} else {
				begin = max(0, size-n)
			}
		default:
			return model.Value{}, sqlError("XX000", "invalid text kernel operation")
		}
		if !text {
			return ownedBytes(ctx, budget, args[0].BytesVal[begin:end])
		}
		first, err := textByteOffset(ctx, args[0].StrVal, begin)
		if err != nil {
			return model.Value{}, err
		}
		last, err := textByteOffset(ctx, args[0].StrVal[first:], end-begin)
		if err != nil {
			return model.Value{}, err
		}
		return ownedText(ctx, budget, args[0].StrVal[first:first+last])
	}
}
