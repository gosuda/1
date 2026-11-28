package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONTextExtract(signature Signature) scalarKernel {
	var shape jsonTextKind
	var asText bool
	switch signature.ID {
	case 3947:
		shape = jsonTextObject
	case 3948:
		shape, asText = jsonTextObject, true
	case 3949:
		shape = jsonTextArray
	case 3950:
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
		if shape == jsonTextArray && selector.index < 0 && selector.index != math.MinInt32 {
			prefix := jsonTextScanner{text: text}
			prefix.space(ctx)
			if prefix.err != nil {
				return model.Value{}, prefix.err
			}
			if prefix.peek() == '[' {
				// PostgreSQL counts negative indexes without decoding strings first.
				_, count, err := scanJSONTextExtract(ctx, text, nil)
				if err != nil {
					return model.Value{}, err
				}
				selector.index += count
			}
		}
		span, _, err := scanJSONTextExtract(ctx, text, &selector)
		if err != nil {
			return model.Value{}, err
		}
		return jsonExtractResult(ctx, text, span, asText, resultType, budget)
	}
}

type jsonTextSelector struct {
	shape jsonTextKind
	field string
	index int64
}

type jsonTextSpan struct {
	kind       jsonTextKind
	start, end int
}

type jsonExtractToken struct {
	jsonTextSpan
	punctuation byte
	matches     bool
}

// A nil selector counts the root array without decoding strings, including the
// one-token lookahead performed by PostgreSQL's json_count_array_elements.
func scanJSONTextExtract(ctx context.Context, text string, selector *jsonTextSelector) (jsonTextSpan, int64, error) {
	return scanJSONExtraction(ctx, text, &jsonExtraction{selector: selector})
}

func scanJSONExtraction(ctx context.Context, text string, extraction *jsonExtraction) (jsonTextSpan, int64, error) {
	if err := ctx.Err(); err != nil {
		return jsonTextSpan{}, 0, err
	}
	scanner := jsonTextScanner{text: text}
	var states [jsonTextMaxDepth]uint8
	var selected jsonTextSpan
	depth := 0
	rootSeen, selectedContainer := false, false
	elements := int64(0)
	decode := extraction.pathMode || extraction.selector != nil
	for {
		var field *string
		if depth != 0 {
			field = extraction.field(depth, states[depth-1])
		}
		token, err := nextJSONExtractToken(ctx, &scanner, decode, field)
		if err != nil {
			return jsonTextSpan{}, 0, err
		}
		if depth == 0 && rootSeen {
			if decode && (token.kind != 0 || token.punctuation != 0) {
				return jsonTextSpan{}, 0, scanner.failure()
			}
			return selected, elements, nil
		}
		match, closed := extraction.value(0, false, 0), false
		if depth != 0 {
			state := &states[depth-1]
			switch *state {
			case jsonObjectFirst, jsonObjectKey:
				if *state == jsonObjectFirst && token.punctuation == '}' {
					closed = true
					break
				}
				if token.kind != jsonTextString {
					return jsonTextSpan{}, 0, scanner.failure()
				}
				extraction.key(depth, token.matches)
				*state = jsonObjectColon
				continue
			case jsonObjectColon:
				if token.punctuation != ':' {
					return jsonTextSpan{}, 0, scanner.failure()
				}
				*state = jsonObjectValue
				continue
			case jsonObjectNext:
				switch token.punctuation {
				case ',':
					*state = jsonObjectKey
					continue
				case '}':
					closed = true
				default:
					return jsonTextSpan{}, 0, scanner.failure()
				}
			case jsonArrayNext:
				switch token.punctuation {
				case ',':
					*state = jsonArrayValue
					continue
				case ']':
					closed = true
				default:
					return jsonTextSpan{}, 0, scanner.failure()
				}
			case jsonArrayFirst:
				if token.punctuation == ']' {
					closed = true
				}
			}
			if closed {
				depth--
				if selectedContainer && depth == extraction.selectedDepth {
					selected.end = token.end
					selectedContainer = false
				}
				continue
			}
			if *state == jsonObjectValue {
				match = extraction.value(depth, true, elements)
				*state = jsonObjectNext
			} else {
				match = extraction.value(depth, false, elements)
				*state = jsonArrayNext
				if depth == 1 {
					elements++
				}
			}
		}
		if token.kind == 0 {
			return jsonTextSpan{}, 0, scanner.failure()
		}
		if match && (!extraction.pathMode || depth == extraction.pathLength) {
			selected = token.jsonTextSpan
			extraction.selectedDepth = depth
			selectedContainer = token.kind == jsonTextObject || token.kind == jsonTextArray
		}
		rootSeen = true
		if token.kind == jsonTextObject || token.kind == jsonTextArray {
			if depth == len(states) {
				return jsonTextSpan{}, 0, sqlError("54001", "JSON nesting limit exceeded")
			}
			if err := extraction.enter(ctx, text, depth, token, match); err != nil {
				return jsonTextSpan{}, 0, err
			}
			states[depth] = jsonObjectFirst
			if token.kind == jsonTextArray {
				states[depth] = jsonArrayFirst
			}
			depth++
		}
	}
}

func nextJSONExtractToken(ctx context.Context, scanner *jsonTextScanner, decode bool, field *string) (jsonExtractToken, error) {
	scanner.space(ctx)
	if scanner.err != nil {
		return jsonExtractToken{}, scanner.err
	}
	token := jsonExtractToken{jsonTextSpan: jsonTextSpan{start: scanner.position, end: scanner.position}}
	if scanner.position == len(scanner.text) {
		return token, nil
	}
	switch scanner.peek() {
	case '}', ']', ',', ':':
		token.punctuation = scanner.peek()
		scanner.advance(ctx, 1)
	case '"':
		token.kind = jsonTextString
		if decode {
			quoted, err := locateJSONStringToken(ctx, scanner)
			if err != nil {
				return jsonExtractToken{}, err
			}
			if field != nil {
				matched, err := jsonStringEquals(ctx, quoted, *field)
				if err != nil {
					return jsonExtractToken{}, err
				}
				token.matches = matched
			} else if _, err := walkJSONDecodedString(ctx, quoted, nil); err != nil {
				return jsonExtractToken{}, err
			}
		} else if err := scanner.string(ctx); err != nil {
			return jsonExtractToken{}, err
		}
	default:
		kind, err := scanner.value(ctx)
		if err != nil {
			return jsonExtractToken{}, err
		}
		token.kind = kind
	}
	token.end = scanner.position
	return token, scanner.err
}
