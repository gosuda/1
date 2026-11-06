package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func compileTextQuote(signature Signature) scalarKernel {
	switch signature.ID {
	case 1282:
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			return quoteIdentifierText(ctx, args[0].StrVal, budget)
		}
	case 1283:
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			return quoteLiteralText(ctx, args[0].StrVal, uint64(budget))
		}
	case 1289:
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			if args[0].IsNull {
				return ownedText(ctx, budget, "NULL")
			}
			return quoteLiteralText(ctx, args[0].StrVal, uint64(budget))
		}
	default:
		return nil
	}
}

func quoteIdentifierText(ctx context.Context, text string, budget int64) (model.Value, error) {
	if err := outputBudget(0, budget); err != nil {
		return model.Value{}, err
	}
	result, err := QuoteQualifiedName(ctx, []string{text}, uint64(budget))
	return model.StringVal(result), err
}

// QuoteQualifiedName borrows up to three identifiers and owns one exact-sized
// qualified result. No intermediate quoted strings coexist with the result.
func QuoteQualifiedName(ctx context.Context, parts []string, maximum uint64) (string, error) {
	if len(parts) == 0 || len(parts) > 3 {
		return "", identifierSyntaxError()
	}
	budget := int64(min(maximum, uint64(1<<63-1)))
	size := int64(len(parts) - 1)
	var safe [3]bool
	for i, part := range parts {
		length, unquoted, err := identifierQuoteSize(ctx, part, budget-size)
		if err != nil {
			return "", err
		}
		size += length
		safe[i] = unquoted
	}
	var output strings.Builder
	output.Grow(int(size))
	for i, part := range parts {
		if i != 0 {
			output.WriteByte('.')
		}
		if safe[i] {
			if err := writeText(ctx, &output, part); err != nil {
				return "", err
			}
			continue
		}
		output.WriteByte('"')
		if err := writeQuotedText(ctx, &output, part, '"'); err != nil {
			return "", err
		}
		output.WriteByte('"')
	}
	return output.String(), ctx.Err()
}

func identifierQuoteSize(ctx context.Context, text string, budget int64) (int64, bool, error) {
	if _, err := textCharacters(ctx, text); err != nil {
		return 0, false, err
	}
	if err := outputBudget(int64(len(text)), budget); err != nil {
		return 0, false, err
	}
	safe := len(text) > 0 && (text[0] >= 'a' && text[0] <= 'z' || text[0] == '_')
	var quotes int64
	for i := range len(text) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		character := text[i]
		letter := character >= 'a' && character <= 'z'
		digit := character >= '0' && character <= '9'
		if !letter && !digit && character != '_' {
			safe = false
			if character == '"' {
				quotes++
			}
		}
	}
	// Every pinned PostgreSQL keyword fits NAMEDATALEN-1; bound map hash work.
	if safe && len(text) <= 63 {
		keyword := zpgparse.KeywordCategory(text)
		safe = keyword == zpgparse.KeywordKind_NO_KEYWORD || keyword == zpgparse.KeywordKind_UNRESERVED_KEYWORD
	}
	size := int64(len(text))
	if !safe {
		size += quotes + 2
	}
	if err := outputBudget(size, budget); err != nil {
		return 0, false, err
	}
	return size, safe, ctx.Err()
}

func quoteLiteralText(ctx context.Context, text string, maximum uint64) (model.Value, error) {
	if _, err := textCharacters(ctx, text); err != nil {
		return model.Value{}, err
	}
	budget := int64(min(maximum, uint64((1<<30)-5)))
	if err := outputBudget(int64(len(text)), budget); err != nil {
		return model.Value{}, err
	}
	size := int64(len(text)) + 2
	escape := false
	for i := range len(text) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		if text[i] == '\'' || text[i] == '\\' {
			size++
		}
		if text[i] == '\\' {
			escape = true
		}
	}
	if escape {
		size++
	}
	if err := outputBudget(size, budget); err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(int(size))
	if escape {
		output.WriteByte('E')
	}
	output.WriteByte('\'')
	if err := writeQuotedText(ctx, &output, text, '\''); err != nil {
		return model.Value{}, err
	}
	output.WriteByte('\'')
	return model.StringVal(output.String()), nil
}

func writeQuotedText(ctx context.Context, output *strings.Builder, text string, quote byte) error {
	for start := 0; start < len(text); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+cancellationChunk, len(text))
		fragment := start
		for i := start; i < end; i++ {
			if text[i] == quote || quote == '\'' && text[i] == '\\' {
				output.WriteString(text[fragment:i])
				output.WriteByte(text[i])
				fragment = i
			}
		}
		output.WriteString(text[fragment:end])
	}
	return nil
}
