package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBExtract(signature Signature) scalarKernel {
	var shape jsonTextKind
	var asText bool
	switch signature.ID {
	case 3478:
		shape = jsonTextObject
	case 3214:
		shape, asText = jsonTextObject, true
	case 3215:
		shape = jsonTextArray
	case 3216:
		shape, asText = jsonTextArray, true
	default:
		return nil
	}
	resultType := model.TypeJSONB
	if asText {
		resultType = model.TypeString
	}
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		text := args[0].StrVal
		selector := jsonTextSelector{shape: shape, field: args[1].StrVal, index: args[1].IntVal}
		span, err := selectJSONBExtract(ctx, text, selector)
		if err != nil {
			return model.Value{}, err
		}
		return jsonbExtractResult(ctx, text, span, asText, resultType, budget)
	}
}

func selectJSONBExtract(ctx context.Context, text string, selector jsonTextSelector) (jsonTextSpan, error) {
	// TypeJSONB lanes can contain uncanonicalized text. Validate even overwritten
	// or unselected values before shape checks, lookup, or output admission.
	shape, err := walkJSONB(ctx, text, nil)
	if err != nil {
		return jsonTextSpan{}, err
	}
	if selector.shape == jsonTextObject {
		if shape.kind != jsonTextObject {
			return jsonTextSpan{}, nil
		}
		// Unlike the JSON text operator, JSONB compares the complete text key.
		// A NUL cannot match any validated JSONB key; do not pass it to the
		// shared scanner's C-string matcher.
		for start := 0; start < len(selector.field); start += cancellationChunk {
			if err := ctx.Err(); err != nil {
				return jsonTextSpan{}, err
			}
			if strings.IndexByte(selector.field[start:min(start+cancellationChunk, len(selector.field))], 0) >= 0 {
				return jsonTextSpan{}, nil
			}
		}
	} else {
		if shape.kind == jsonTextObject {
			return jsonTextSpan{}, nil
		}
		elements := shape.elements
		if shape.kind != jsonTextArray {
			// PostgreSQL stores a root JSONB scalar in a one-element array.
			elements = 1
		}
		if selector.index < 0 {
			selector.index += elements
		}
		if selector.index < 0 || selector.index >= elements {
			return jsonTextSpan{}, nil
		}
		if shape.kind != jsonTextArray {
			scanner := jsonTextScanner{text: text}
			token, err := nextJSONExtractToken(ctx, &scanner, false, nil)
			return token.jsonTextSpan, err
		}
	}
	span, _, err := scanJSONTextExtract(ctx, text, &selector)
	return span, err
}

func quoteJSONBExtract(ctx context.Context, quoted string, budget int64) (string, error) {
	decoded, err := decodeJSONString(ctx, quoted, budget)
	if err != nil {
		return "", err
	}
	size, err := jsonQuotedBytes(ctx, decoded)
	if err != nil {
		return "", err
	}
	// The decoded string remains live while its canonical spelling is written.
	if err := outputBudget(int64(len(decoded))+size, budget); err != nil {
		return "", err
	}
	var output strings.Builder
	output.Grow(int(size))
	if err := appendJSONQuoted(ctx, &output, decoded); err != nil {
		return "", err
	}
	return output.String(), nil
}

func numberJSONBExtract(ctx context.Context, text string, budget int64) (string, error) {
	number, err := inspectJSONBNumber(ctx, text)
	if err != nil {
		return "", err
	}
	size := number.bytes()
	if err := outputBudget(size, budget); err != nil {
		return "", err
	}
	var output strings.Builder
	output.Grow(int(size))
	if err := number.append(ctx, &output); err != nil {
		return "", err
	}
	return output.String(), nil
}

func jsonbExtractResult(ctx context.Context, text string, span jsonTextSpan, asText bool, resultType model.DataType, budget int64) (model.Value, error) {
	var err error
	if span.kind == 0 || asText && span.kind == jsonTextNull {
		return model.NullVal(resultType), nil
	}
	selected := text[span.start:span.end]
	var output string
	switch span.kind {
	case jsonTextObject, jsonTextArray:
		output, err = canonicalJSONB(ctx, selected, budget, false)
	case jsonTextString:
		if asText {
			output, err = decodeJSONString(ctx, selected, budget)
		} else {
			output, err = quoteJSONBExtract(ctx, selected, budget)
		}
	case jsonTextNumber:
		output, err = numberJSONBExtract(ctx, selected, budget)
	default:
		var value model.Value
		value, err = ownedText(ctx, budget, selected)
		output = value.StrVal
	}
	if err != nil {
		return model.Value{}, err
	}
	return model.Value{Type: resultType, StrVal: output}, nil
}
