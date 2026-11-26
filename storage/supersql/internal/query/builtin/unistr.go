package builtin

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func unicodeString(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	input := args[0].StrVal
	size, err := decodeUnicodeString(ctx, input, nil, budget)
	if err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(size)
	if _, err := decodeUnicodeString(ctx, input, &output, budget); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(output.String()), nil
}

func decodeUnicodeString(ctx context.Context, input string, output *strings.Builder, budget int64) (int, error) {
	var pending uint32
	size, checkpoint := 0, 0
	for position := 0; position < len(input); {
		if position >= checkpoint {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			checkpoint = position + 4096
		}
		if input[position] != '\\' {
			if pending != 0 {
				return 0, unicodePairError()
			}
			if err := outputBudget(int64(size)+1, budget); err != nil {
				return 0, err
			}
			if output != nil {
				output.WriteByte(input[position])
			}
			size++
			position++
			continue
		}
		if position+1 < len(input) && input[position+1] == '\\' {
			if pending != 0 {
				return 0, unicodePairError()
			}
			if err := outputBudget(int64(size)+1, budget); err != nil {
				return 0, err
			}
			if output != nil {
				output.WriteByte('\\')
			}
			size++
			position += 2
			continue
		}
		codepoint, consumed, err := unicodeStringEscape(input[position:])
		if err != nil {
			return 0, err
		}
		position += consumed
		if codepoint == 0 || codepoint > utf8.MaxRune {
			return 0, sqlError("22023", "invalid Unicode code point")
		}
		low := codepoint >= 0xdc00 && codepoint <= 0xdfff
		if pending != 0 {
			if !low {
				return 0, unicodePairError()
			}
			codepoint = 0x10000 + (pending-0xd800)*1024 + codepoint - 0xdc00
			pending = 0
		} else if low {
			return 0, unicodePairError()
		}
		if codepoint >= 0xd800 && codepoint <= 0xdbff {
			pending = codepoint
			continue
		}
		length := utf8.RuneLen(rune(codepoint))
		if err := outputBudget(int64(size)+int64(length), budget); err != nil {
			return 0, err
		}
		if output != nil {
			output.WriteRune(rune(codepoint))
		}
		size += length
	}
	if pending != 0 {
		return 0, unicodePairError()
	}
	return size, ctx.Err()
}

func unicodeStringEscape(input string) (uint32, int, error) {
	if value, ok := unicodeHex(input, 1, 4); ok {
		return value, 5, nil
	}
	if len(input) > 1 {
		width := 0
		switch input[1] {
		case 'u':
			width = 4
		case '+':
			width = 6
		case 'U':
			width = 8
		}
		if width != 0 {
			if value, ok := unicodeHex(input, 2, width); ok {
				return value, width + 2, nil
			}
		}
	}
	return 0, 0, sqlError("42601", "invalid Unicode escape")
}

func unicodeHex(input string, offset, count int) (uint32, bool) {
	if len(input)-offset < count {
		return 0, false
	}
	var value uint32
	for i := offset; i < offset+count; i++ {
		nibble := digit(input[i])
		if nibble < 0 || nibble > 15 {
			return 0, false
		}
		value = value*16 + uint32(nibble)
	}
	return value, true
}
func unicodePairError() error { return sqlError("42601", "invalid Unicode surrogate pair") }
