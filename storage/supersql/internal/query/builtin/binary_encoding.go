package builtin

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileBinaryEncoding(signature Signature) scalarKernel {
	switch signature.ID {
	case 1946:
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			format, err := binaryEncodingFormat(ctx, args[1].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			return encodeBinary(ctx, args[0].BytesVal, format, budget)
		}
	case 1947:
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			format, err := binaryEncodingFormat(ctx, args[1].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			input := args[0].StrVal
			if _, err := textCharacters(ctx, input); err != nil {
				return model.Value{}, err
			}
			decode := decodeBinaryHex
			switch format {
			case "base64":
				decode = decodeBinaryBase64
			case "escape":
				decode = decodeBinaryEscape
			}
			size, err := decode(ctx, input, nil)
			if err != nil {
				return model.Value{}, err
			}
			if err := outputBudget(int64(size), budget); err != nil {
				return model.Value{}, err
			}
			output := make([]byte, size)
			if _, err := decode(ctx, input, output); err != nil {
				return model.Value{}, err
			}
			return model.BytesVal(output), nil
		}
	default:
		return nil
	}
}

func binaryEncodingFormat(ctx context.Context, name string) (string, error) {
	if _, err := textCharacters(ctx, name); err != nil {
		return "", err
	}
	var folded [6]byte
	if len(name) <= len(folded) {
		for i := range len(name) {
			value := name[i]
			if value >= 'A' && value <= 'Z' {
				value += 'a' - 'A'
			}
			folded[i] = value
		}
		switch string(folded[:len(name)]) {
		case "hex":
			return "hex", nil
		case "base64":
			return "base64", nil
		case "escape":
			return "escape", nil
		}
	}
	return "", sqlError("22023", "unrecognized encoding")
}

func encodeBinary(ctx context.Context, input []byte, format string, budget int64) (model.Value, error) {
	// Every encoding expands its input; this also bounds the size arithmetic.
	if err := outputBudget(int64(len(input)), budget); err != nil {
		return model.Value{}, err
	}
	var size int64
	switch format {
	case "hex":
		size = int64(len(input)) * 2
	case "base64":
		size = (int64(len(input))+2)/3*4 + int64(len(input))/57
	case "escape":
		for i, value := range input {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			switch {
			case value == 0 || value >= 0x80:
				size += 4
			case value == '\\':
				size += 2
			default:
				size++
			}
		}
	}
	if err := outputBudget(size, budget); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(int(size))
	switch format {
	case "hex":
		var encoded [cancellationChunk]byte
		for start := 0; start < len(input); {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			end := min(start+len(encoded)/2, len(input))
			size := hex.Encode(encoded[:], input[start:end])
			output.Write(encoded[:size])
			start = end
		}
	case "base64":
		var encoded [76]byte
		nextCheck := 0
		for start := 0; start < len(input); {
			if start >= nextCheck {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
				nextCheck = start + cancellationChunk
			}
			end := min(start+57, len(input))
			base64.StdEncoding.Encode(encoded[:], input[start:end])
			output.Write(encoded[:base64.StdEncoding.EncodedLen(end-start)])
			// PG wraps complete 57-byte groups, but not a padded final line.
			if end-start == 57 {
				output.WriteByte('\n')
			}
			start = end
		}
	case "escape":
		for i, value := range input {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			switch {
			case value == 0 || value >= 0x80:
				output.WriteByte('\\')
				output.WriteByte('0' + value>>6)
				output.WriteByte('0' + (value>>3)&7)
				output.WriteByte('0' + value&7)
			case value == '\\':
				output.WriteString(`\\`)
			default:
				output.WriteByte(value)
			}
		}
	}
	return model.StringVal(output.String()), nil
}

func binaryEncodingWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r'
}

func decodeBinaryHex(ctx context.Context, input string, output []byte) (int, error) {
	size, nextCheck := 0, 0
	for i := 0; i < len(input); {
		if i >= nextCheck {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			nextCheck = i + cancellationChunk
		}
		if binaryEncodingWhitespace(input[i]) {
			i++
			continue
		}
		first := digit(input[i])
		if first < 0 {
			_, width := utf8.DecodeRuneInString(input[i:])
			return 0, sqlError("22023", fmt.Sprintf("invalid hexadecimal digit: \"%s\"", input[i:i+width]))
		}
		i++
		if i == len(input) {
			return 0, sqlError("22023", "invalid hexadecimal data: odd number of digits")
		}
		// Whitespace is legal between bytes, never between their two digits.
		second := digit(input[i])
		if second < 0 {
			_, width := utf8.DecodeRuneInString(input[i:])
			return 0, sqlError("22023", fmt.Sprintf("invalid hexadecimal digit: \"%s\"", input[i:i+width]))
		}
		i++
		if output != nil {
			output[size] = byte(first<<4 | second)
		}
		size++
	}
	return size, nil
}

func decodeBinaryBase64(ctx context.Context, input string, output []byte) (int, error) {
	size, position, paddedBytes := 0, 0, 0
	var buffer uint32
	for i := range len(input) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		value := input[i]
		if binaryEncodingWhitespace(value) {
			continue
		}
		var bits uint32
		switch {
		case value == '=':
			if paddedBytes == 0 {
				switch position {
				case 2:
					paddedBytes = 1
				case 3:
					paddedBytes = 2
				default:
					return 0, sqlError("22023", "unexpected \"=\" while decoding base64 sequence")
				}
			}
		case value >= 'A' && value <= 'Z':
			bits = uint32(value - 'A')
		case value >= 'a' && value <= 'z':
			bits = uint32(value-'a') + 26
		case value >= '0' && value <= '9':
			bits = uint32(value-'0') + 52
		case value == '+':
			bits = 62
		case value == '/':
			bits = 63
		default:
			_, width := utf8.DecodeRuneInString(input[i:])
			return 0, sqlError("22023", fmt.Sprintf("invalid symbol \"%s\" found while decoding base64 sequence", input[i:i+width]))
		}
		buffer = buffer<<6 | bits
		position++
		if position != 4 {
			continue
		}
		bytes := 3
		if paddedBytes != 0 {
			bytes = paddedBytes
		}
		if output != nil {
			output[size] = byte(buffer >> 16)
			if bytes > 1 {
				output[size+1] = byte(buffer >> 8)
			}
			if bytes > 2 {
				output[size+2] = byte(buffer)
			}
		}
		size += bytes
		buffer, position = 0, 0
		// PG retains the first padding width even across subsequent groups.
	}
	if position != 0 {
		return 0, sqlError("22023", "invalid base64 end sequence")
	}
	return size, nil
}

func decodeBinaryEscape(ctx context.Context, input string, output []byte) (int, error) {
	size, nextCheck := 0, 0
	for i := 0; i < len(input); {
		if i >= nextCheck {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			nextCheck = i + cancellationChunk
		}
		value, next, err := byteaUnit(input, i, false)
		if err != nil {
			return 0, sqlError("22P02", "invalid input syntax for type bytea")
		}
		if output != nil {
			output[size] = value
		}
		i = next
		size++
	}
	return size, nil
}
