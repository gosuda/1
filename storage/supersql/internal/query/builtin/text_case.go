package builtin

import (
	"context"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTextCase(signature Signature, options ResolveOptions) scalarKernel {
	switch signature.ID {
	case 870, 871, 872, 1622, 3062, 6382:
	default:
		return nil
	}
	collation := options.Collation
	if collation == 100 {
		collation = options.DefaultCollation
	}
	caseConversion := signature.ID == 870 || signature.ID == 871 || signature.ID == 872
	indeterminate := caseConversion && collation != 950 && collation != 951
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		if indeterminate {
			return model.Value{}, sqlError("42P22", "case conversion requires a resolved C or POSIX collation")
		}
		if signature.ID == 6382 {
			return reverseBytes(ctx, args[0].BytesVal, budget)
		}
		input := args[0].StrVal
		if _, err := textCharacters(ctx, input); err != nil {
			return model.Value{}, err
		}
		switch signature.ID {
		case 1622:
			return repeatText(ctx, input, args[1].IntVal, budget)
		case 3062:
			return reverseText(ctx, input, budget)
		default:
			return convertTextCase(ctx, input, signature.ID, budget)
		}
	}
}

func convertTextCase(ctx context.Context, input string, id model.FunctionID, budget int64) (model.Value, error) {
	if err := outputBudget(int64(len(input)), budget); err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(len(input))
	upper, initcap, wasAlnum := id == 871, id == 872, false
	for i := range len(input) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		value := input[i]
		if upper || (initcap && !wasAlnum) {
			if value >= 'a' && value <= 'z' {
				value -= 'a' - 'A'
			}
		} else if value >= 'A' && value <= 'Z' {
			value += 'a' - 'A'
		}
		if initcap {
			// PostgreSQL's asc_initcap treats non-ASCII bytes as word boundaries.
			wasAlnum = (value >= 'A' && value <= 'Z') ||
				(value >= 'a' && value <= 'z') || (value >= '0' && value <= '9')
		}
		output.WriteByte(value)
	}
	return model.StringVal(output.String()), nil
}

func repeatText(ctx context.Context, input string, count, budget int64) (model.Value, error) {
	if count <= 0 || len(input) == 0 {
		return ownedText(ctx, budget)
	}
	if int64(len(input)) > math.MaxInt64/count {
		return model.Value{}, sqlError("54000", "requested length too large")
	}
	size := int64(len(input)) * count
	if err := outputBudget(size, budget); err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(int(size))
	for start := 0; start < len(input); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		output.WriteString(input[start:min(start+cancellationChunk, len(input))])
	}
	// Copy the owned prefix rather than writing one occurrence at a time.
	for output.Len() < int(size) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		start := output.Len() % len(input)
		width := min(output.Len()-start, int(size)-output.Len(), cancellationChunk)
		output.WriteString(output.String()[start : start+width])
	}
	return model.StringVal(output.String()), nil
}

func reverseText(ctx context.Context, input string, budget int64) (model.Value, error) {
	if err := outputBudget(int64(len(input)), budget); err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(len(input))
	nextCheck := 0
	for end := len(input); end > 0; {
		if processed := len(input) - end; processed >= nextCheck {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			nextCheck = processed + cancellationChunk
		}
		_, width := utf8.DecodeLastRuneInString(input[:end])
		output.WriteString(input[end-width : end])
		end -= width
	}
	return model.StringVal(output.String()), nil
}

func reverseBytes(ctx context.Context, input []byte, budget int64) (model.Value, error) {
	if len(input) < 2 {
		return ownedBytes(ctx, budget, input)
	}
	if err := outputBudget(int64(len(input)), budget); err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	output := make([]byte, len(input))
	for i, value := range input {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		output[len(input)-1-i] = value
	}
	return model.BytesVal(output), nil
}
