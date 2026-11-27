package builtin

import (
	"context"
	"strings"
)

type recordInputToken struct {
	end    int
	bytes  int64
	decode bool
	null   bool
}

func recordTextError(detail string) error {
	return sqlError("22P02", "malformed record literal: "+detail)
}

// The first pass validates one field and measures decoded bytes without owning
// a copy. The second pass is used only when quote/escape removal needs storage.
func scanRecordField(ctx context.Context, input string, start int, output *strings.Builder) (recordInputToken, error) {
	token := recordInputToken{end: start}
	if start >= len(input) {
		return token, recordTextError("unexpected end of input")
	}
	if input[start] == ',' || input[start] == ')' {
		token.null = true
		return token, nil
	}
	position, span := start, start
	quoted := false
	for position < len(input) {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return token, err
			}
		}
		c := input[position]
		if c == 0 {
			return token, recordTextError("unexpected end of input")
		}
		if !quoted && (c == ',' || c == ')') {
			token.bytes += int64(position - span)
			if err := writeRecordInputSpan(ctx, output, input[span:position]); err != nil {
				return token, err
			}
			token.end = position
			return token, nil
		}
		if c != '\\' && c != '"' {
			position++
			continue
		}
		token.decode = true
		token.bytes += int64(position - span)
		if err := writeRecordInputSpan(ctx, output, input[span:position]); err != nil {
			return token, err
		}
		position++
		switch {
		case c == '\\':
			if position == len(input) || input[position] == 0 {
				return token, recordTextError("unexpected end of input")
			}
			token.bytes++
			if output != nil {
				output.WriteByte(input[position])
			}
			position++
		case !quoted:
			quoted = true
		case position < len(input) && input[position] == '"':
			token.bytes++
			if output != nil {
				output.WriteByte('"')
			}
			position++
		default:
			quoted = false
		}
		span = position
	}
	return token, recordTextError("unexpected end of input")
}

func recordInputSpace(ctx context.Context, input string, position int) (int, error) {
	for position < len(input) && isTSSpace(input[position]) {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return position, err
			}
		}
		position++
	}
	return position, nil
}

func writeRecordInputSpan(ctx context.Context, output *strings.Builder, text string) error {
	if output == nil {
		return nil
	}
	return copyArrayText(ctx, output, text, false)
}
