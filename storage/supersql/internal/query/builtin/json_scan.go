package builtin

import (
	"cmp"
	"context"
	"unicode/utf8"
)

type jsonTextKind uint8

const (
	jsonTextObject jsonTextKind = iota + 1
	jsonTextArray
	jsonTextString
	jsonTextNumber
	jsonTextBoolean
	jsonTextNull
)
const (
	jsonObjectFirst uint8 = iota + 1
	jsonObjectKey
	jsonObjectColon
	jsonObjectValue
	jsonObjectNext
	jsonArrayFirst
	jsonArrayValue
	jsonArrayNext
)

// PostgreSQL's incremental JSON parser also caps nesting at 6400. Fixed state
// keeps inspection independent of the variable-width result budget.
const jsonTextMaxDepth = 6400

type jsonTextScanner struct {
	text                 string
	position, checkpoint int
	err                  error
}

func (scanner *jsonTextScanner) peek() byte {
	if scanner.err != nil || scanner.position == len(scanner.text) {
		return 0
	}
	return scanner.text[scanner.position]
}
func (scanner *jsonTextScanner) advance(ctx context.Context, width int) {
	scanner.position += width
	if scanner.position >= scanner.checkpoint {
		scanner.err = ctx.Err()
		scanner.checkpoint = scanner.position + 4096
	}
}
func (scanner *jsonTextScanner) space(ctx context.Context) {
	for {
		switch scanner.peek() {
		case ' ', '\t', '\n', '\r':
			scanner.advance(ctx, 1)
		default:
			return
		}
	}
}
func (scanner *jsonTextScanner) failure() error {
	if scanner.err != nil {
		return scanner.err
	}
	return sqlError("22P02", "invalid input syntax for type json")
}

func (scanner *jsonTextScanner) tokenEnd() error {
	if scanner.err != nil {
		return scanner.err
	}
	value := scanner.peek()
	number := value >= '0' && value <= '9'
	if identifierStart(value) || number {
		return scanner.failure()
	}
	return nil
}

func scanJSONText(ctx context.Context, text string) (jsonTextKind, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	scanner := jsonTextScanner{text: text}
	var states [jsonTextMaxDepth]uint8
	depth := 0
	root := jsonTextKind(0)
	elements := int64(0)
	for {
		scanner.space(ctx)
		if scanner.err != nil {
			return 0, 0, scanner.err
		}
		if depth == 0 && root != 0 {
			if scanner.position != len(text) {
				return 0, 0, scanner.failure()
			}
			return root, elements, nil
		}
		if depth != 0 {
			state := &states[depth-1]
			switch *state {
			case jsonObjectFirst, jsonObjectKey:
				if *state == jsonObjectFirst && scanner.peek() == '}' {
					scanner.advance(ctx, 1)
					depth--
					continue
				}
				if scanner.peek() != '"' {
					return 0, 0, scanner.failure()
				}
				if err := scanner.string(ctx); err != nil {
					return 0, 0, err
				}
				*state = jsonObjectColon
				continue
			case jsonObjectColon:
				if scanner.peek() != ':' {
					return 0, 0, scanner.failure()
				}
				scanner.advance(ctx, 1)
				*state = jsonObjectValue
				continue
			case jsonObjectNext:
				switch scanner.peek() {
				case ',':
					scanner.advance(ctx, 1)
					*state = jsonObjectKey
				case '}':
					scanner.advance(ctx, 1)
					depth--
				default:
					return 0, 0, scanner.failure()
				}
				continue
			case jsonArrayNext:
				switch scanner.peek() {
				case ',':
					scanner.advance(ctx, 1)
					*state = jsonArrayValue
				case ']':
					scanner.advance(ctx, 1)
					depth--
				default:
					return 0, 0, scanner.failure()
				}
				continue
			case jsonArrayFirst:
				if scanner.peek() == ']' {
					scanner.advance(ctx, 1)
					depth--
					continue
				}
			}
			if *state == jsonObjectValue {
				*state = jsonObjectNext
			} else {
				*state = jsonArrayNext
				if depth == 1 {
					elements++
				}
			}
		}
		kind, err := scanner.value(ctx)
		if err != nil {
			return 0, 0, err
		}
		root = cmp.Or(root, kind)
		if kind == jsonTextObject || kind == jsonTextArray {
			if depth == len(states) {
				return 0, 0, sqlError("54001", "JSON nesting limit exceeded")
			}
			states[depth] = jsonObjectFirst
			if kind == jsonTextArray {
				states[depth] = jsonArrayFirst
			}
			depth++
		}
	}
}

func (scanner *jsonTextScanner) value(ctx context.Context) (jsonTextKind, error) {
	switch scanner.peek() {
	case '{':
		scanner.advance(ctx, 1)
		return jsonTextObject, scanner.err
	case '[':
		scanner.advance(ctx, 1)
		return jsonTextArray, scanner.err
	case '"':
		return jsonTextString, scanner.string(ctx)
	case 't':
		return jsonTextBoolean, scanner.literal(ctx, "true")
	case 'f':
		return jsonTextBoolean, scanner.literal(ctx, "false")
	case 'n':
		return jsonTextNull, scanner.literal(ctx, "null")
	default:
		value := scanner.peek()
		if value == '-' || value >= '0' && value <= '9' {
			return jsonTextNumber, scanner.number(ctx)
		}
		return 0, scanner.failure()
	}
}
func (scanner *jsonTextScanner) literal(ctx context.Context, word string) error {
	if len(scanner.text)-scanner.position < len(word) || scanner.text[scanner.position:scanner.position+len(word)] != word {
		return scanner.failure()
	}
	scanner.advance(ctx, len(word))
	return scanner.tokenEnd()
}
func (scanner *jsonTextScanner) digits(ctx context.Context) bool {
	start := scanner.position
	for scanner.peek() >= '0' && scanner.peek() <= '9' {
		scanner.advance(ctx, 1)
	}
	return scanner.position != start
}
func (scanner *jsonTextScanner) number(ctx context.Context) error {
	if scanner.peek() == '-' {
		scanner.advance(ctx, 1)
	}
	if scanner.peek() == '0' {
		scanner.advance(ctx, 1)
	} else if !scanner.digits(ctx) {
		return scanner.failure()
	}
	if scanner.peek() == '.' {
		scanner.advance(ctx, 1)
		if !scanner.digits(ctx) {
			return scanner.failure()
		}
	}
	if scanner.peek() == 'e' || scanner.peek() == 'E' {
		scanner.advance(ctx, 1)
		if scanner.peek() == '+' || scanner.peek() == '-' {
			scanner.advance(ctx, 1)
		}
		if !scanner.digits(ctx) {
			return scanner.failure()
		}
	}
	return scanner.tokenEnd()
}
func (scanner *jsonTextScanner) string(ctx context.Context) error {
	scanner.advance(ctx, 1)
	for scanner.position < len(scanner.text) && scanner.err == nil {
		value := scanner.peek()
		if value == '"' {
			scanner.advance(ctx, 1)
			return scanner.err
		}
		if value < 0x20 {
			return scanner.failure()
		}
		if value == '\\' {
			if err := scanner.escape(ctx); err != nil {
				return err
			}
			continue
		}
		width := 1
		if value >= utf8.RuneSelf {
			code, size := utf8.DecodeRuneInString(scanner.text[scanner.position:])
			if code == utf8.RuneError && size == 1 {
				return sqlError("22021", "invalid UTF8 sequence")
			}
			width = size
		}
		scanner.advance(ctx, width)
	}
	if scanner.err != nil {
		return scanner.err
	}
	return scanner.failure()
}
func (scanner *jsonTextScanner) escape(ctx context.Context) error {
	scanner.advance(ctx, 1)
	switch scanner.peek() {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		scanner.advance(ctx, 1)
	case 'u':
		scanner.advance(ctx, 1)
		for range 4 {
			value := digit(scanner.peek())
			if value < 0 || value > 15 {
				return scanner.failure()
			}
			scanner.advance(ctx, 1)
		}
	default:
		return scanner.failure()
	}
	return scanner.err
}
