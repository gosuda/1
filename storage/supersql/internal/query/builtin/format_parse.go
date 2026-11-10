package builtin

import (
	"context"
	"math"
)

type formatDirective struct {
	argument      int
	widthArgument int
	width         int64
	indirectWidth bool
	left          bool
	conversion    byte
}

func formatEnd() error       { return sqlError("22023", "unterminated format() type specifier") }
func formatArguments() error { return sqlError("22023", "too few arguments for format()") }

func parseFormatNumber(ctx context.Context, text string, position int) (int, int, bool, error) {
	start, value := position, 0
	for position < len(text) && text[position] >= '0' && text[position] <= '9' {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, position, false, err
			}
		}
		digit := int(text[position] - '0')
		if value > (math.MaxInt32-digit)/10 {
			return 0, position, false, sqlError("22003", "number is out of range")
		}
		value = value*10 + digit
		position++
		if position == len(text) {
			return 0, position, false, formatEnd()
		}
	}
	return value, position, position != start, nil
}

func parseFormatDirective(ctx context.Context, text string, position int) (formatDirective, int, error) {
	spec := formatDirective{argument: -1, widthArgument: -1}
	if position >= len(text) {
		return spec, position, formatEnd()
	}
	number, next, found, err := parseFormatNumber(ctx, text, position)
	if err != nil {
		return spec, next, err
	}
	position = next
	if found {
		if text[position] != '$' {
			spec.width = int64(number)
			return finishFormatDirective(text, position, spec)
		}
		if number == 0 {
			return spec, position, sqlError("22023", "format arguments are numbered from 1")
		}
		spec.argument = number - 1
		position++
		if position == len(text) {
			return spec, position, formatEnd()
		}
	}
	for text[position] == '-' {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return spec, position, err
			}
		}
		spec.left = true
		position++
		if position == len(text) {
			return spec, position, formatEnd()
		}
	}
	if text[position] == '*' {
		spec.indirectWidth = true
		position++
		if position == len(text) {
			return spec, position, formatEnd()
		}
		number, next, found, err = parseFormatNumber(ctx, text, position)
		if err != nil {
			return spec, next, err
		}
		position = next
		if found {
			if text[position] != '$' {
				return spec, position, sqlError("22023", "width argument position must end with $")
			}
			if number == 0 {
				return spec, position, sqlError("22023", "format arguments are numbered from 1")
			}
			spec.widthArgument = number - 1
			position++
			if position == len(text) {
				return spec, position, formatEnd()
			}
		}
	} else {
		number, next, _, err = parseFormatNumber(ctx, text, position)
		if err != nil {
			return spec, next, err
		}
		position = next
		spec.width = int64(number)
	}
	return finishFormatDirective(text, position, spec)
}

func finishFormatDirective(text string, position int, spec formatDirective) (formatDirective, int, error) {
	spec.conversion = text[position]
	if spec.conversion != 's' && spec.conversion != 'I' && spec.conversion != 'L' {
		return spec, position, sqlError("22023", "unrecognized format() type specifier")
	}
	return spec, position + 1, nil
}
