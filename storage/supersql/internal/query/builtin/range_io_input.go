package builtin

import (
	"context"
	"math"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type rangeIOMemory struct {
	budget CallBudget
	used   uint64
	claim  columnar.Allocation
}

func (memory *rangeIOMemory) resize(ctx context.Context, size uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if size > memory.budget.MemoryBytes {
		return sqlError("54000", "range IO memory budget exceeded")
	}
	if memory.claim != nil {
		if err := memory.claim.Resize(size); err != nil {
			return err
		}
	} else if memory.budget.Allocator != nil {
		claim, err := memory.budget.Allocator.Reserve(size)
		if err != nil {
			return err
		}
		memory.claim = claim
	}
	memory.used = size
	return nil
}
func (memory *rangeIOMemory) close() {
	if memory.claim != nil {
		memory.claim.Release()
	}
}
func (memory *rangeIOMemory) remaining() (CallBudget, error) { return memory.budget.After(memory.used) }
func rangeIOSpace(ch byte) bool                              { return ch == ' ' || ch >= '\t' && ch <= '\r' }
func rangeIOSkip(ctx context.Context, text string, position int) (int, error) {
	for position < len(text) && rangeIOSpace(text[position]) {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		position++
	}
	return position, ctx.Err()
}
func rangeIOSyntax() error { return sqlError("22P02", "malformed range or multirange literal") }

type rangeIOToken struct {
	start, end, size int
	escaped          bool
}

func scanRangeIOBound(ctx context.Context, text string, start int) (rangeIOToken, error) {
	token := rangeIOToken{start: start}
	quoted := false
	for i := start; i < len(text); i++ {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return token, err
			}
		}
		ch := text[i]
		if ch == 0 {
			return token, rangeIOSyntax()
		}
		delimiter := ch == ',' || ch == ')' || ch == ']'
		if !quoted && delimiter {
			token.end = i
			return token, nil
		}
		switch ch {
		case '\\':
			token.escaped = true
			i++
			if i >= len(text) || text[i] == 0 {
				return token, rangeIOSyntax()
			}
			token.size++
		case '"':
			token.escaped = true
			if quoted && i+1 < len(text) && text[i+1] == '"' {
				i++
				token.size++
			} else {
				quoted = !quoted
			}
		default:
			token.size++
		}
	}
	return token, rangeIOSyntax()
}
func decodeRangeIOToken(ctx context.Context, text string, token rangeIOToken, budget CallBudget) (string, byteResultBuffer, error) {
	if !token.escaped {
		return text[token.start:token.end], byteResultBuffer{}, nil
	}
	budget.OutputBytes = math.MaxInt64
	buffer, err := newByteResult(ctx, uint64(token.size), budget)
	if err != nil {
		return "", buffer, err
	}
	quoted, position := false, 0
	for i := token.start; i < token.end; i++ {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				buffer.close()
				return "", byteResultBuffer{}, err
			}
		}
		ch := text[i]
		if ch == '\\' {
			i++
			ch = text[i]
		} else if ch == '"' {
			if quoted && i+1 < token.end && text[i+1] == '"' {
				i++
			} else {
				quoted = !quoted
				continue
			}
		}
		buffer.data[position] = ch
		position++
	}
	return unsafe.String(unsafe.SliceData(buffer.data), len(buffer.data)), buffer, nil
}
func (codec *rangeIOCodec) parse(ctx context.Context, text string, start int, element *arrayElementCall, memory *rangeIOMemory) (rangeDatum, int, error) {
	datum := rangeDatum{lower: rangeBound{side: rangeLower}, upper: rangeBound{side: rangeUpper}}
	position, err := rangeIOSkip(ctx, text, start)
	if err != nil {
		return datum, 0, err
	}
	if len(text)-position >= 5 && strings.EqualFold(text[position:position+5], "empty") {
		datum.empty = true
		return datum, position + 5, nil
	}
	if position >= len(text) || text[position] != '[' && text[position] != '(' {
		return datum, 0, rangeIOSyntax()
	}
	datum.lower.inclusive = text[position] == '['
	lower, err := scanRangeIOBound(ctx, text, position+1)
	if err != nil {
		return datum, 0, err
	}
	if text[lower.end] != ',' {
		return datum, 0, rangeIOSyntax()
	}
	upper, err := scanRangeIOBound(ctx, text, lower.end+1)
	if err != nil {
		return datum, 0, err
	}
	if text[upper.end] != ')' && text[upper.end] != ']' {
		return datum, 0, rangeIOSyntax()
	}
	datum.upper.inclusive = text[upper.end] == ']'
	end := upper.end + 1
	// Range input validates the entire envelope before invoking either subtype.
	if !codec.multi {
		final, err := rangeIOSkip(ctx, text, end)
		if err != nil {
			return datum, 0, err
		}
		if final != len(text) {
			return datum, 0, rangeIOSyntax()
		}
	}
	for i, token := range [2]rangeIOToken{lower, upper} {
		bound := &datum.lower
		if i == 1 {
			bound = &datum.upper
		}
		if token.start == token.end {
			bound.infinite = true
			bound.inclusive = false
			continue
		}
		nested, err := memory.remaining()
		if err != nil {
			return datum, 0, err
		}
		decoded, buffer, err := decodeRangeIOToken(ctx, text, token, nested)
		if err != nil {
			return datum, 0, err
		}
		nested, err = nested.After(uint64(len(buffer.data)))
		if err != nil {
			buffer.close()
			return datum, 0, err
		}
		nested.OutputBytes = int64(min(nested.MemoryBytes, uint64(math.MaxInt64)))
		value, err := element.invoke(ctx, model.StringVal(decoded), nested)
		buffer.close()
		if err != nil {
			return datum, 0, err
		}
		converted, err := rangeBoundFromValue(codec.ops.family, value, bound.side, bound.inclusive)
		if err != nil {
			return datum, 0, err
		}
		if err := memory.resize(ctx, memory.used+uint64(len(converted.numeric)+len(converted.bytes))); err != nil {
			return datum, 0, err
		}
		*bound = converted
	}
	nested, err := memory.remaining()
	if err != nil {
		return datum, 0, err
	}
	payloadBytes := uint64(len(datum.lower.numeric)+len(datum.lower.bytes)) + uint64(len(datum.upper.numeric)+len(datum.upper.bytes))
	datum, err = codec.ops.canonical(ctx, datum, nested)
	if err == nil && datum.empty {
		datum.lower.numeric, datum.upper.numeric = "", ""
		datum.lower.bytes, datum.upper.bytes = nil, nil
		err = memory.resize(ctx, memory.used-payloadBytes)
	}
	return datum, end, err
}

func appendRangeInput(ctx context.Context, ranges []rangeDatum, datum rangeDatum, memory *rangeIOMemory) ([]rangeDatum, error) {
	if datum.empty {
		return ranges, nil
	}
	if len(ranges) < cap(ranges) {
		return append(ranges, datum), nil
	}
	capacity := max(1, cap(ranges)*2)
	size := uint64(capacity) * uint64(unsafe.Sizeof(rangeDatum{}))
	old := uint64(cap(ranges)) * uint64(unsafe.Sizeof(rangeDatum{}))
	if err := memory.resize(ctx, memory.used+size); err != nil {
		return nil, err
	}
	grown := make([]rangeDatum, len(ranges), capacity)
	for i := range ranges {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		grown[i] = ranges[i]
	}
	if err := memory.resize(ctx, memory.used-old); err != nil {
		return nil, err
	}
	return append(grown, datum), nil
}

func (codec *rangeIOCodec) input(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	id := codec.ops.family.Range
	if codec.multi {
		id = codec.ops.family.Multirange
	}
	if arguments[1].IntVal != int64(id) {
		return model.Value{}, sqlError("42804", "range input OID differs from bound family")
	}
	element := arrayElementCall{function: codec.element}
	element.arguments[1] = model.Int64Val(int64(codec.parameter))
	element.arguments[2] = arguments[2]
	memory := rangeIOMemory{budget: budget}
	defer memory.close()
	text := arguments[0].StrVal
	if !codec.multi {
		datum, end, err := codec.parse(ctx, text, 0, &element, &memory)
		if err != nil {
			return model.Value{}, err
		}
		end, err = rangeIOSkip(ctx, text, end)
		if err != nil {
			return model.Value{}, err
		}
		if end != len(text) {
			return model.Value{}, rangeIOSyntax()
		}
		nested, err := memory.remaining()
		if err != nil {
			return model.Value{}, err
		}
		return encodeRange(ctx, codec.ops.family, datum, nested)
	}
	position, err := rangeIOSkip(ctx, text, 0)
	if err != nil {
		return model.Value{}, err
	}
	if position >= len(text) || text[position] != '{' {
		return model.Value{}, rangeIOSyntax()
	}
	position++
	var ranges []rangeDatum
	position, err = rangeIOSkip(ctx, text, position)
	if err != nil {
		return model.Value{}, err
	}
	if position < len(text) && text[position] == '}' {
		position++
	} else {
		for {
			datum, end, err := codec.parse(ctx, text, position, &element, &memory)
			if err != nil {
				return model.Value{}, err
			}
			ranges, err = appendRangeInput(ctx, ranges, datum, &memory)
			if err != nil {
				return model.Value{}, err
			}
			position, err = rangeIOSkip(ctx, text, end)
			if err != nil {
				return model.Value{}, err
			}
			if position >= len(text) {
				return model.Value{}, rangeIOSyntax()
			}
			if text[position] == '}' {
				position++
				break
			}
			if text[position] != ',' {
				return model.Value{}, rangeIOSyntax()
			}
			position++
		}
	}
	position, err = rangeIOSkip(ctx, text, position)
	if err != nil {
		return model.Value{}, err
	}
	if position != len(text) {
		return model.Value{}, rangeIOSyntax()
	}
	nested, err := memory.remaining()
	if err != nil {
		return model.Value{}, err
	}
	return normalizeMultirange(ctx, &codec.ops, ranges, nested)
}
