package builtin

import "context"

const jsonbMaxStorageBytes = 0x0fffffff

type jsonbShape struct {
	kind         jsonTextKind
	elements     int64
	nodes        int64
	decodedBytes int64
	depth        int
}

// A zero kind marks a container end; keys are separate from value nodes.
type jsonbVisit func(kind jsonTextKind, token string, key bool, depth int) error

func walkJSONB(ctx context.Context, text string, visit jsonbVisit) (jsonbShape, error) {
	if err := ctx.Err(); err != nil {
		return jsonbShape{}, err
	}
	walker := jsonbWalker{scanner: jsonTextScanner{text: text}, visit: visit}
	for {
		walker.scanner.space(ctx)
		if walker.scanner.err != nil {
			return jsonbShape{}, walker.scanner.err
		}
		if walker.depth == 0 && walker.shape.kind != 0 {
			if walker.scanner.position != len(text) {
				return jsonbShape{}, walker.failure(ctx)
			}
			return walker.shape, ctx.Err()
		}
		if walker.depth != 0 {
			value, err := walker.container(ctx)
			if err != nil {
				return jsonbShape{}, err
			}
			if !value {
				continue
			}
		}
		if err := walker.value(ctx); err != nil {
			return jsonbShape{}, err
		}
	}
}

type jsonbWalker struct {
	scanner jsonTextScanner
	states  [jsonTextMaxDepth]uint8
	shape   jsonbShape
	depth   int
	visit   jsonbVisit
}

func (walker *jsonbWalker) failure(ctx context.Context) error {
	if walker.scanner.peek() == '"' {
		if _, err := scanJSONDecodedToken(ctx, &walker.scanner); err != nil {
			return err
		}
	}
	return walker.scanner.failure()
}

func (walker *jsonbWalker) notify(kind jsonTextKind, token string, key bool) error {
	if walker.visit == nil {
		return nil
	}
	return walker.visit(kind, token, key, walker.depth)
}

func (walker *jsonbWalker) string(ctx context.Context) (string, error) {
	quoted, err := locateJSONStringToken(ctx, &walker.scanner)
	if err != nil {
		return "", err
	}
	length, err := walkJSONDecodedString(ctx, quoted, nil)
	if err != nil {
		return "", err
	}
	if length > jsonbMaxStorageBytes {
		return "", sqlError("54000", "string too long to represent as jsonb string")
	}
	walker.shape.decodedBytes += length
	return quoted, nil
}

func (walker *jsonbWalker) container(ctx context.Context) (bool, error) {
	state := &walker.states[walker.depth-1]
	closing := false
	switch *state {
	case jsonObjectFirst, jsonObjectKey:
		if *state == jsonObjectFirst && walker.scanner.peek() == '}' {
			closing = true
			break
		}
		if walker.scanner.peek() != '"' {
			return false, walker.failure(ctx)
		}
		quoted, err := walker.string(ctx)
		if err != nil {
			return false, err
		}
		if err := walker.notify(jsonTextString, quoted, true); err != nil {
			return false, err
		}
		*state = jsonObjectColon
		return false, nil
	case jsonObjectColon:
		if walker.scanner.peek() != ':' {
			return false, walker.failure(ctx)
		}
		walker.scanner.advance(ctx, 1)
		*state = jsonObjectValue
		return false, nil
	case jsonObjectNext:
		if walker.scanner.peek() == ',' {
			walker.scanner.advance(ctx, 1)
			*state = jsonObjectKey
			return false, nil
		}
		if walker.scanner.peek() != '}' {
			return false, walker.failure(ctx)
		}
		closing = true
	case jsonArrayNext:
		if walker.scanner.peek() == ',' {
			walker.scanner.advance(ctx, 1)
			*state = jsonArrayValue
			return false, nil
		}
		if walker.scanner.peek() != ']' {
			return false, walker.failure(ctx)
		}
		closing = true
	case jsonArrayFirst:
		closing = walker.scanner.peek() == ']'
	}
	if closing {
		walker.scanner.advance(ctx, 1)
		walker.depth--
		return false, walker.notify(0, "", false)
	}
	if *state == jsonObjectValue {
		*state = jsonObjectNext
	} else {
		*state = jsonArrayNext
		if walker.depth == 1 {
			walker.shape.elements++
		}
	}
	return true, nil
}

func (walker *jsonbWalker) value(ctx context.Context) error {
	start := walker.scanner.position
	var kind jsonTextKind
	var err error
	if walker.scanner.peek() == '"' {
		kind = jsonTextString
		_, err = walker.string(ctx)
	} else {
		kind, err = walker.scanner.value(ctx)
	}
	if err != nil {
		return err
	}
	token := walker.scanner.text[start:walker.scanner.position]
	if kind == jsonTextNumber {
		if _, err := inspectJSONBNumber(ctx, token); err != nil {
			return err
		}
	}
	if walker.shape.kind == 0 {
		walker.shape.kind = kind
	}
	walker.shape.nodes++
	container := kind == jsonTextObject || kind == jsonTextArray
	if container && walker.depth == len(walker.states) {
		return sqlError("54001", "JSON nesting limit exceeded")
	}
	if err := walker.notify(kind, token, false); err != nil {
		return err
	}
	if container {
		walker.states[walker.depth] = jsonObjectFirst
		if kind == jsonTextArray {
			walker.states[walker.depth] = jsonArrayFirst
		}
		walker.depth++
		walker.shape.depth = max(walker.shape.depth, walker.depth)
	}
	return nil
}
