package builtin

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONStrip(signature Signature) scalarKernel {
	if signature.ID != 3261 {
		return nil
	}
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		text := args[0].StrVal
		if _, _, err := scanJSONTextExtract(ctx, text, &jsonTextSelector{}); err != nil {
			return model.Value{}, err
		}
		size, err := stripJSONText(ctx, text, args[1].IntVal != 0, nil)
		if err != nil {
			return model.Value{}, err
		}
		if err := outputBudget(size, budget); err != nil {
			return model.Value{}, err
		}
		var output strings.Builder
		output.Grow(int(size))
		if _, err := stripJSONText(ctx, text, args[1].IntVal != 0, &output); err != nil {
			return model.Value{}, err
		}
		return model.Value{Type: model.TypeJSONB, StrVal: output.String()}, nil
	}
}

type jsonStripWriter struct {
	output *strings.Builder
	size   int64
	err    error
}

func (writer *jsonStripWriter) add(bytes int64) bool {
	if writer.err != nil {
		return false
	}
	if bytes > int64((1<<30)-5)-writer.size {
		writer.err = sqlError("54000", "builtin output byte budget exceeded")
		return false
	}
	writer.size += bytes
	return true
}
func (writer *jsonStripWriter) byte(value byte) {
	if writer.add(1) && writer.output != nil {
		writer.output.WriteByte(value)
	}
}
func (writer *jsonStripWriter) raw(ctx context.Context, text string) {
	for start := 0; start < len(text) && writer.err == nil; start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			writer.err = err
			return
		}
		end := min(start+cancellationChunk, len(text))
		if writer.add(int64(end-start)) && writer.output != nil {
			writer.output.WriteString(text[start:end])
		}
	}
}
func (writer *jsonStripWriter) quoted(ctx context.Context, quoted string) {
	writer.byte('"')
	_, err := walkJSONDecodedString(ctx, quoted, func(value rune) error {
		switch value {
		case '"', '\\':
			writer.byte('\\')
			writer.byte(byte(value))
		case '\b':
			writer.raw(ctx, `\b`)
		case '\f':
			writer.raw(ctx, `\f`)
		case '\n':
			writer.raw(ctx, `\n`)
		case '\r':
			writer.raw(ctx, `\r`)
		case '\t':
			writer.raw(ctx, `\t`)
		default:
			if value < 0x20 {
				const hex = "0123456789abcdef"
				writer.raw(ctx, `\u00`)
				writer.byte(hex[value>>4])
				writer.byte(hex[value&15])
			} else if writer.add(int64(utf8.RuneLen(value))) && writer.output != nil {
				writer.output.WriteRune(value)
			}
		}
		return writer.err
	})
	if err != nil {
		writer.err = err
		return
	}
	writer.byte('"')
}

// The input is already grammar/Unicode validated. Only one borrowed key span
// and fixed-depth emitted-item flags are needed; dropped fields allocate nothing.
func stripJSONText(ctx context.Context, text string, arrays bool, output *strings.Builder) (int64, error) {
	scanner := jsonTextScanner{text: text}
	writer := jsonStripWriter{output: output}
	var frames [jsonTextMaxDepth]uint8
	depth := 0
	var key jsonTextSpan
	for {
		token, err := nextJSONExtractToken(ctx, &scanner, false, nil)
		if err != nil {
			return 0, err
		}
		if token.kind == 0 {
			switch token.punctuation {
			case 0:
				return writer.size, writer.err
			case '}', ']':
				writer.byte(token.punctuation)
				depth--
			}
			continue
		}
		if token.kind == jsonTextString && depth > 0 && frames[depth-1]&1 != 0 {
			scanner.space(ctx)
			if scanner.peek() == ':' {
				key = token.jsonTextSpan
				continue
			}
		}
		objectField := key.kind != 0
		stripNull := token.kind == jsonTextNull && (objectField || arrays)
		if depth > 0 && stripNull {
			key = jsonTextSpan{}
			continue
		}
		if depth > 0 {
			if frames[depth-1]&2 != 0 {
				writer.byte(',')
			}
			frames[depth-1] |= 2
		}
		if objectField {
			writer.quoted(ctx, text[key.start:key.end])
			writer.byte(':')
			key = jsonTextSpan{}
		}
		switch token.kind {
		case jsonTextObject:
			writer.byte('{')
			frames[depth] = 1
			depth++
		case jsonTextArray:
			writer.byte('[')
			frames[depth] = 0
			depth++
		case jsonTextString:
			writer.quoted(ctx, text[token.start:token.end])
		default:
			writer.raw(ctx, text[token.start:token.end])
		}
		if writer.err != nil {
			return 0, writer.err
		}
	}
}
