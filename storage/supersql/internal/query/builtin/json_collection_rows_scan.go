package builtin

import "context"

// scanJSONCollectionValue consumes a value from a previously validated document.
func scanJSONCollectionValue(ctx context.Context, scanner *jsonTextScanner) (jsonTextSpan, error) {
	token, err := nextJSONExtractToken(ctx, scanner, false, nil)
	if err != nil {
		return jsonTextSpan{}, err
	}
	if token.kind == 0 {
		return jsonTextSpan{}, scanner.failure()
	}
	span := token.jsonTextSpan
	depth := 0
	if span.kind == jsonTextObject || span.kind == jsonTextArray {
		depth = 1
	}
	for depth != 0 {
		token, err = nextJSONExtractToken(ctx, scanner, false, nil)
		if err != nil {
			return jsonTextSpan{}, err
		}
		if token.kind == jsonTextObject || token.kind == jsonTextArray {
			depth++
		} else if token.punctuation == '}' || token.punctuation == ']' {
			depth--
		} else if token.kind == 0 && token.punctuation == 0 {
			return jsonTextSpan{}, scanner.failure()
		}
		span.end = token.end
	}
	return span, ctx.Err()
}

type jsonCollectionCursor struct {
	scanner jsonTextScanner
	object  bool
	first   bool
}

func newJSONCollectionCursor(text string, object bool) jsonCollectionCursor {
	return jsonCollectionCursor{scanner: jsonTextScanner{text: text}, object: object, first: true}
}

func (cursor *jsonCollectionCursor) next(ctx context.Context) (key, value jsonTextSpan, ok bool, err error) {
	scanner := &cursor.scanner
	scanner.space(ctx)
	if cursor.first {
		scanner.advance(ctx, 1)
		cursor.first = false
	} else if scanner.peek() == ',' {
		scanner.advance(ctx, 1)
	}
	scanner.space(ctx)
	if scanner.err != nil {
		return key, value, false, scanner.err
	}
	if scanner.peek() == '}' || scanner.peek() == ']' {
		return key, value, false, nil
	}
	if cursor.object {
		key, err = scanJSONCollectionValue(ctx, scanner)
		if err != nil {
			return key, value, false, err
		}
		scanner.space(ctx)
		if scanner.peek() != ':' {
			return key, value, false, scanner.failure()
		}
		scanner.advance(ctx, 1)
	}
	value, err = scanJSONCollectionValue(ctx, scanner)
	return key, value, err == nil, err
}
