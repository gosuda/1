package builtin

import (
	"context"
	"strings"
	"unicode/utf8"
)

func jsonStringSyntaxError() error { return sqlError("22P02", "invalid JSON string") }

func walkJSONDecodedString(ctx context.Context, quoted string, emit func(rune) error) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(quoted) == 0 || quoted[0] != '"' {
		return 0, jsonStringSyntaxError()
	}
	var high uint32
	size := int64(0)
	checkpoint := 1
	for position := 1; position < len(quoted); {
		if position >= checkpoint {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			checkpoint = position + 4096
		}
		value := quoted[position]
		if value == '"' {
			if high != 0 || position != len(quoted)-1 {
				return 0, jsonStringSyntaxError()
			}
			return size, ctx.Err()
		}
		if value < 0x20 {
			return 0, jsonStringSyntaxError()
		}
		var code rune
		if value == '\\' {
			var err error
			code, err = readJSONEscape(quoted, &position, &high)
			if err != nil {
				return 0, err
			}
			if high != 0 {
				continue
			}
		} else {
			if high != 0 {
				return 0, jsonStringSyntaxError()
			}
			var width int
			code, width = utf8.DecodeRuneInString(quoted[position:])
			if code == utf8.RuneError && width == 1 {
				return 0, sqlError("22021", "invalid UTF8 sequence")
			}
			position += width
		}
		size += int64(utf8.RuneLen(code))
		if emit != nil {
			if err := emit(code); err != nil {
				return 0, err
			}
		}
	}
	return 0, jsonStringSyntaxError()
}

func readJSONEscape(quoted string, position *int, high *uint32) (rune, error) {
	*position++
	if *position == len(quoted) {
		return 0, jsonStringSyntaxError()
	}
	escape := quoted[*position]
	*position++
	if escape == 'u' {
		scalar, ok := unicodeHex(quoted, *position, 4)
		if !ok {
			return 0, jsonStringSyntaxError()
		}
		*position += 4
		return jsonUnicodeScalar(scalar, high)
	}
	if *high != 0 {
		return 0, jsonStringSyntaxError()
	}
	switch escape {
	case '"', '\\', '/':
		return rune(escape), nil
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	default:
		return 0, jsonStringSyntaxError()
	}
}

func jsonUnicodeScalar(scalar uint32, high *uint32) (rune, error) {
	if scalar >= 0xd800 && scalar <= 0xdbff {
		if *high != 0 {
			return 0, jsonStringSyntaxError()
		}
		*high = scalar
		return 0, nil
	}
	if scalar >= 0xdc00 && scalar <= 0xdfff {
		if *high == 0 {
			return 0, jsonStringSyntaxError()
		}
		scalar = 0x10000 + (*high-0xd800)*1024 + scalar - 0xdc00
		*high = 0
	}
	if *high != 0 {
		return 0, jsonStringSyntaxError()
	}
	if scalar == 0 {
		return 0, sqlError("22P05", "unsupported Unicode escape sequence")
	}
	return rune(scalar), nil
}

func decodeJSONString(ctx context.Context, quoted string, budget int64) (string, error) {
	size, err := walkJSONDecodedString(ctx, quoted, nil)
	if err != nil {
		return "", err
	}
	if err := outputBudget(size, budget); err != nil {
		return "", err
	}
	var output strings.Builder
	output.Grow(int(size))
	_, err = walkJSONDecodedString(ctx, quoted, func(code rune) error { output.WriteRune(code); return nil })
	if err != nil {
		return "", err
	}
	return output.String(), nil
}

func jsonStringEquals(ctx context.Context, quoted, wanted string) (bool, error) {
	position := 0
	var encoded [utf8.UTFMax]byte
	matches := true
	_, err := walkJSONDecodedString(ctx, quoted, func(code rune) error {
		if !matches {
			return nil
		}
		if position == len(wanted) || wanted[position] == 0 {
			matches = false
			return nil
		}
		bytes := utf8.AppendRune(encoded[:0], code)
		if len(bytes) > len(wanted)-position {
			matches = false
			return nil
		}
		for i, value := range bytes {
			if value != wanted[position+i] {
				matches = false
				return nil
			}
		}
		position += len(bytes)
		return nil
	})
	if err != nil {
		return false, err
	}
	return matches && (position == len(wanted) || wanted[position] == 0), nil
}

func scanJSONDecodedToken(ctx context.Context, scanner *jsonTextScanner) (string, error) {
	quoted, err := locateJSONStringToken(ctx, scanner)
	if err != nil {
		return "", err
	}
	_, err = walkJSONDecodedString(ctx, quoted, nil)
	return quoted, err
}

func locateJSONStringToken(ctx context.Context, scanner *jsonTextScanner) (string, error) {
	if scanner.peek() != '"' {
		return "", scanner.failure()
	}
	start := scanner.position
	scanner.advance(ctx, 1)
	escaped := false
	for scanner.position < len(scanner.text) && scanner.err == nil {
		value := scanner.peek()
		scanner.advance(ctx, 1)
		if value == '"' && !escaped {
			return scanner.text[start:scanner.position], scanner.err
		}
		escaped = value == '\\' && !escaped
	}
	if scanner.err != nil {
		return "", scanner.err
	}
	return scanner.text[start:scanner.position], nil
}

func jsonQuotedBytes(ctx context.Context, text string) (int64, error) {
	size := int64(2)
	for i := 0; i < len(text); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		switch text[i] {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			size += 2
		default:
			if text[i] < 0x20 {
				size += 6
			} else {
				size++
			}
		}
	}
	return size, ctx.Err()
}
func appendJSONQuoted(ctx context.Context, output *strings.Builder, text string) error {
	const hex = "0123456789abcdef"
	output.WriteByte('"')
	for i := 0; i < len(text); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		value := text[i]
		switch value {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteByte(value)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			if value < 0x20 {
				output.WriteString(`\u00`)
				output.WriteByte(hex[value>>4])
				output.WriteByte(hex[value&15])
			} else {
				output.WriteByte(value)
			}
		}
	}
	output.WriteByte('"')
	return ctx.Err()
}
