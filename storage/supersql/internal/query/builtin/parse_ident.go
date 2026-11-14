package builtin

import (
	"context"
	"math"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type identifierPart struct {
	start, end int
	bytes      int64
	quoted     bool
}
type identifierParts struct {
	input        string
	offset       int
	strict, done bool
	err          error
}

func parseIdentifier(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	parts := identifierParts{input: args[0].StrVal, strict: args[1].IntVal != 0}
	sizing := parts
	budget = min(budget, arrayInputMaxBytes)
	count, payload, total := int64(0), int64(0), arrayDimensionBytes
	for !sizing.done {
		part, err := sizing.next(ctx)
		if err != nil {
			return model.Value{}, err
		}
		if count == math.MaxInt32 {
			return model.Value{}, sqlError("54000", "identifier array exceeds maximum size")
		}
		if err := outputBudget(arrayValueBytes+part.bytes, budget-total); err != nil {
			return model.Value{}, err
		}
		total += arrayValueBytes + part.bytes
		count++
		payload += part.bytes
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	var arena strings.Builder
	arena.Grow(int(payload))
	values := make([]model.Value, int(count))
	for i := range values {
		part, err := parts.next(ctx)
		if err != nil {
			return model.Value{}, err
		}
		start := arena.Len()
		if err := part.appendTo(ctx, parts.input, &arena); err != nil {
			return model.Value{}, err
		}
		values[i] = model.StringVal(arena.String()[start:])
	}
	return model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{Length: int32(count), LowerBound: 1}}}, nil
}

func (parts *identifierParts) peek() byte {
	if parts.err != nil || parts.offset == len(parts.input) {
		return 0
	}
	return parts.input[parts.offset]
}
func (parts *identifierParts) advance(ctx context.Context) {
	parts.offset++
	if parts.offset&255 == 0 {
		parts.err = ctx.Err()
	}
}
func (parts *identifierParts) space(ctx context.Context) {
	for isTSSpace(parts.peek()) {
		parts.advance(ctx)
	}
}
func identifierStart(value byte) bool {
	lower := value | 32
	letter := lower >= 'a' && lower <= 'z'
	return value == '_' || value >= 128 || letter
}
func identifierContinue(value byte) bool {
	number := value >= '0' && value <= '9'
	return identifierStart(value) || number || value == '$'
}

func (parts *identifierParts) next(ctx context.Context) (identifierPart, error) {
	if err := ctx.Err(); err != nil {
		return identifierPart{}, err
	}
	parts.space(ctx)
	if parts.err != nil {
		return identifierPart{}, parts.err
	}
	part := identifierPart{start: parts.offset}
	if parts.peek() == '"' {
		parts.advance(ctx)
		part.start, part.quoted = parts.offset, true
		if err := parts.quotedPart(ctx, &part); err != nil {
			return part, err
		}
	} else {
		if !identifierStart(parts.peek()) {
			return part, identifierSyntaxError()
		}
		for identifierContinue(parts.peek()) {
			parts.advance(ctx)
			part.bytes++
		}
		part.end = parts.offset
	}
	if parts.err != nil {
		return part, parts.err
	}
	if _, err := textCharacters(ctx, parts.input[part.start:part.end]); err != nil {
		return part, err
	}
	parts.space(ctx)
	if parts.err != nil {
		return part, parts.err
	}
	switch parts.peek() {
	case '.':
		parts.advance(ctx)
	case 0:
		parts.done = true
	default:
		if parts.strict {
			return part, identifierSyntaxError()
		}
		parts.done = true
	}
	return part, nil
}

func (parts *identifierParts) quotedPart(ctx context.Context, part *identifierPart) error {
	for parts.peek() != 0 {
		if parts.peek() != '"' {
			parts.advance(ctx)
			part.bytes++
			continue
		}
		part.end = parts.offset
		parts.advance(ctx)
		if parts.peek() != '"' {
			if parts.err != nil {
				return parts.err
			}
			if part.bytes == 0 {
				return identifierSyntaxError()
			}
			return nil
		}
		parts.advance(ctx)
		part.bytes++
	}
	if parts.err != nil {
		return parts.err
	}
	return identifierSyntaxError()
}

func (part identifierPart) appendTo(ctx context.Context, input string, output *strings.Builder) error {
	for i := part.start; i < part.end; i++ {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		value := input[i]
		if part.quoted {
			if value == '"' {
				i++
			}
		} else if value >= 'A' && value <= 'Z' {
			value += 'a' - 'A'
		}
		output.WriteByte(value)
	}
	return ctx.Err()
}
func identifierSyntaxError() error { return sqlError("22023", "string is not a valid identifier") }
