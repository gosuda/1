package builtin

import (
	"context"
	"errors"
	"math"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	arrayValueBytes     = int64(unsafe.Sizeof(model.Value{}))
	arrayDimensionBytes = int64(unsafe.Sizeof(model.ArrayDimension{}))
	arrayInputMaxBytes  = int64(1<<30) - 5
)

type arrayTextTokenKind uint8

const (
	arrayTextStart arrayTextTokenKind = iota
	arrayTextEnd
	arrayTextDelimiter
	arrayTextElement
	arrayTextNull
)

type arrayTextToken struct {
	kind arrayTextTokenKind
	text string
}

type arrayTextScanner struct {
	input     string
	position  int
	nextCheck int
	delimiter byte
	nulls     bool
}

func arrayTextError(detail string) error {
	return sqlError("22P02", "malformed array literal: "+detail)
}

func arrayDimensionError(specified bool) error {
	if specified {
		return arrayTextError("specified array dimensions do not match array contents")
	}
	return arrayTextError("multidimensional arrays must have sub-arrays with matching dimensions")
}

func (scanner *arrayTextScanner) check(ctx context.Context) error {
	if scanner.position < scanner.nextCheck {
		return nil
	}
	scanner.nextCheck = scanner.position + cancellationChunk
	return ctx.Err()
}

func (scanner *arrayTextScanner) space(ctx context.Context) error {
	for scanner.position < len(scanner.input) && isTSSpace(scanner.input[scanner.position]) {
		if err := scanner.check(ctx); err != nil {
			return err
		}
		scanner.position++
	}
	return nil
}

func (scanner *arrayTextScanner) dimensionInt(ctx context.Context) (int32, bool, error) {
	start := scanner.position
	position := start
	negative := false
	if position < len(scanner.input) && (scanner.input[position] == '-' || scanner.input[position] == '+') {
		negative = scanner.input[position] == '-'
		position++
	}
	if position == len(scanner.input) || scanner.input[position] < '0' || scanner.input[position] > '9' {
		return 0, false, nil
	}
	scanner.position = position
	limit := uint64(math.MaxInt32)
	if negative {
		limit++
	}
	var value uint64
	overflow := false
	for scanner.position < len(scanner.input) {
		if err := scanner.check(ctx); err != nil {
			return 0, false, err
		}
		c := scanner.input[scanner.position]
		if c < '0' || c > '9' {
			break
		}
		digit := uint64(c - '0')
		if value > (limit-digit)/10 {
			overflow = true
		} else if !overflow {
			value = value*10 + digit
		}
		scanner.position++
	}
	if overflow {
		return 0, false, sqlError("54000", "array bound is out of integer range")
	}
	if negative {
		return int32(-int64(value)), true, nil
	}
	return int32(value), true, nil
}

func (scanner *arrayTextScanner) dimensions(ctx context.Context, dimensions *[6]model.ArrayDimension) (int, error) {
	rank := 0
	for {
		if err := scanner.space(ctx); err != nil {
			return 0, err
		}
		if scanner.position == len(scanner.input) || scanner.input[scanner.position] != '[' {
			return rank, nil
		}
		scanner.position++
		if rank == len(dimensions) {
			return 0, sqlError("54000", "number of array dimensions exceeds the maximum allowed (6)")
		}
		first, found, err := scanner.dimensionInt(ctx)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, arrayTextError("'[' must introduce explicitly-specified array dimensions")
		}
		lower, upper := int32(1), first
		if scanner.position < len(scanner.input) && scanner.input[scanner.position] == ':' {
			lower = first
			scanner.position++
			upper, found, err = scanner.dimensionInt(ctx)
			if err != nil {
				return 0, err
			}
			if !found {
				return 0, arrayTextError("missing array dimension value")
			}
		}
		if scanner.position == len(scanner.input) || scanner.input[scanner.position] != ']' {
			return 0, arrayTextError("missing ']' after array dimensions")
		}
		scanner.position++
		if upper < lower {
			return 0, sqlError("2202E", "upper bound cannot be less than lower bound")
		}
		if upper == math.MaxInt32 {
			return 0, sqlError("54000", "array upper bound is too large")
		}
		length := int64(upper) - int64(lower) + 1
		if length > math.MaxInt32 {
			return 0, sqlError("54000", "array size exceeds the maximum allowed")
		}
		dimensions[rank] = model.ArrayDimension{LowerBound: lower, Length: int32(length)}
		rank++
	}
}

func (scanner *arrayTextScanner) token(ctx context.Context) (arrayTextToken, error) {
	if err := scanner.space(ctx); err != nil {
		return arrayTextToken{}, err
	}
	if scanner.position == len(scanner.input) || scanner.input[scanner.position] == 0 {
		return arrayTextToken{}, arrayTextError("unexpected end of input")
	}
	switch scanner.input[scanner.position] {
	case '{':
		scanner.position++
		return arrayTextToken{kind: arrayTextStart}, nil
	case '}':
		scanner.position++
		return arrayTextToken{kind: arrayTextEnd}, nil
	case '"':
		scanner.position++
		return scanner.element(ctx, true)
	default:
		if scanner.input[scanner.position] == scanner.delimiter {
			scanner.position++
			return arrayTextToken{kind: arrayTextDelimiter}, nil
		}
		return scanner.element(ctx, false)
	}
}

func (scanner *arrayTextScanner) element(ctx context.Context, quoted bool) (arrayTextToken, error) {
	start := scanner.position
	end, size, keptSize := start, 0, 0
	escaped := false
	for {
		if err := scanner.check(ctx); err != nil {
			return arrayTextToken{}, err
		}
		if scanner.position == len(scanner.input) || scanner.input[scanner.position] == 0 {
			return arrayTextToken{}, arrayTextError("unexpected end of input")
		}
		c := scanner.input[scanner.position]
		if c == '\\' {
			escaped = true
			scanner.position++
			if scanner.position == len(scanner.input) || scanner.input[scanner.position] == 0 {
				return arrayTextToken{}, arrayTextError("unexpected end of input")
			}
			scanner.position++
			size++
			end, keptSize = scanner.position, size
			continue
		}
		if c == '"' {
			if !quoted {
				return arrayTextToken{}, arrayTextError("incorrectly quoted array element")
			}
			end, keptSize = scanner.position, size
			scanner.position++
			if err := scanner.space(ctx); err != nil {
				return arrayTextToken{}, err
			}
			if scanner.position == len(scanner.input) || scanner.input[scanner.position] == 0 {
				return arrayTextToken{}, arrayTextError("unexpected end of input")
			}
			next := scanner.input[scanner.position]
			if next != scanner.delimiter && next != '{' && next != '}' {
				return arrayTextToken{}, arrayTextError("incorrectly quoted array element")
			}
			break
		}
		if !quoted {
			if c == '{' {
				return arrayTextToken{}, arrayTextError("unexpected '{' character")
			}
			if c == scanner.delimiter || c == '}' {
				break
			}
		}
		scanner.position++
		size++
		if !isTSSpace(c) {
			end, keptSize = scanner.position, size
		}
	}
	text := scanner.input[start:end]
	if escaped {
		// Escaped integer and boolean input can be much larger than its result.
		if err := outputBudget(int64(keptSize), arrayInputMaxBytes); err != nil {
			return arrayTextToken{}, err
		}
		var decoded strings.Builder
		decoded.Grow(keptSize)
		nextCheck := 0
		for i := 0; i < len(text); i++ {
			if i >= nextCheck {
				if err := ctx.Err(); err != nil {
					return arrayTextToken{}, err
				}
				nextCheck = i + cancellationChunk
			}
			if text[i] == '\\' {
				i++
			}
			decoded.WriteByte(text[i])
		}
		text = decoded.String()
	}
	kind := arrayTextElement
	if !quoted && !escaped && scanner.nulls && strings.EqualFold(text, "NULL") {
		kind = arrayTextNull
	}
	return arrayTextToken{kind: kind, text: text}, nil
}

func resizeArrayInput(ctx context.Context, values []model.Value, capacity int) ([]model.Value, error) {
	resized := make([]model.Value, len(values), capacity)
	const chunk = cancellationChunk / int(arrayValueBytes)
	for start := 0; start < len(values); start += chunk {
		if err := ctx.Err(); err != nil {
			clear(resized)
			return nil, err
		}
		copy(resized[start:min(start+chunk, len(values))], values[start:min(start+chunk, len(values))])
	}
	clear(values)
	return resized, nil
}

func parseArrayText(ctx context.Context, input string, delimiter byte, nulls bool, element *arrayElementCall, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	budget = min(budget, arrayInputMaxBytes)
	scanner := arrayTextScanner{input: input, delimiter: delimiter, nulls: nulls}
	var dimensions [6]model.ArrayDimension
	for i := range dimensions {
		dimensions[i] = model.ArrayDimension{LowerBound: 1, Length: -1}
	}
	rank, err := scanner.dimensions(ctx, &dimensions)
	if err != nil {
		return model.Value{}, err
	}
	specified := rank != 0
	if specified {
		if scanner.position == len(input) || input[scanner.position] != '=' {
			return model.Value{}, arrayTextError("missing '=' after array dimensions")
		}
		scanner.position++
		if err := scanner.space(ctx); err != nil {
			return model.Value{}, err
		}
	}
	if scanner.position == len(input) || input[scanner.position] != '{' {
		return model.Value{}, arrayTextError("array value must start with '{' or dimension information")
	}
	var values []model.Value
	defer func() { clear(values) }()
	var counts [6]int32
	level := 0
	frozen, expectDelimiter := specified, false
	var payload int64
	for {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		token, err := scanner.token(ctx)
		if err != nil {
			return model.Value{}, err
		}
		switch token.kind {
		case arrayTextStart:
			if expectDelimiter {
				return model.Value{}, arrayTextError("unexpected '{' character")
			}
			if level == len(dimensions) {
				return model.Value{}, sqlError("54000", "number of array dimensions exceeds the maximum allowed (6)")
			}
			counts[level] = 0
			level++
			if level > rank {
				if frozen {
					return model.Value{}, arrayDimensionError(specified)
				}
				rank = level
			}
		case arrayTextEnd:
			if counts[level-1] > 0 && !expectDelimiter {
				return model.Value{}, arrayTextError("unexpected '}' character")
			}
			level--
			if level > 0 {
				if counts[level-1] == math.MaxInt32 {
					return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
				}
				counts[level-1]++
			}
			if dimensions[level].Length < 0 {
				dimensions[level].Length = counts[level]
			} else if counts[level] != dimensions[level].Length {
				return model.Value{}, arrayDimensionError(specified)
			}
			expectDelimiter = true
		case arrayTextDelimiter:
			if !expectDelimiter {
				return model.Value{}, arrayTextError("unexpected array delimiter")
			}
			expectDelimiter = false
		case arrayTextElement, arrayTextNull:
			if expectDelimiter {
				return model.Value{}, arrayTextError("unexpected array element")
			}
			headerBytes := int64(rank)*arrayDimensionBytes + int64(len(values)+1)*arrayValueBytes
			if err := outputBudget(headerBytes+payload, budget); err != nil {
				return model.Value{}, err
			}
			isNull := token.kind == arrayTextNull
			argument := model.StringVal(token.text)
			if isNull {
				argument = model.NullVal(model.TypeString)
			}
			remaining := budget - headerBytes - payload
			nested, err := callBudget.After(uint64(int64(cap(values))*arrayValueBytes + payload))
			if err != nil {
				return model.Value{}, err
			}
			value, err := element.invoke(ctx, argument, nested.WithOutput(remaining))
			argument, token = model.Value{}, arrayTextToken{}
			if err != nil {
				return model.Value{}, err
			}
			if value.IsNull != isNull {
				return model.Value{}, sqlError("XX000", "array input function returned an unexpected nullness")
			}
			bytes, err := model.ValueBytes(ctx, value, uint64(remaining))
			if err != nil {
				if errors.Is(err, model.ErrValueBudget) {
					err = sqlError("54000", "builtin output byte budget exceeded")
				}
				return model.Value{}, err
			}
			// Spare headers may be reclaimed only after the single codec call tells
			// us the decoded payload size; they never reduce that call's quota.
			payload += int64(bytes)
			maximumCapacity := int((budget - int64(rank)*arrayDimensionBytes - payload) / arrayValueBytes)
			capacity := cap(values)
			if capacity < len(values)+1 {
				capacity = min(max(1, capacity*2), maximumCapacity)
			} else if capacity > maximumCapacity {
				// Split remaining room between future headers and payload so each
				// subsequent compaction consumes at least half that headroom.
				capacity = len(values) + 1 + (maximumCapacity-len(values)-1)/2
			}
			if capacity != cap(values) {
				resized, err := resizeArrayInput(ctx, values, capacity)
				if err != nil {
					return model.Value{}, err
				}
				values = resized
			}
			values = append(values, value)
			// PostgreSQL converts the token before rejecting a shallower element.
			frozen = true
			if level != rank {
				return model.Value{}, arrayDimensionError(specified)
			}
			if counts[level-1] == math.MaxInt32 {
				return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
			}
			counts[level-1]++
			expectDelimiter = true
		}
		if level == 0 {
			break
		}
	}
	if err := scanner.space(ctx); err != nil {
		return model.Value{}, err
	}
	if scanner.position != len(input) {
		return model.Value{}, arrayTextError("junk after closing right brace")
	}
	if len(values) == 0 {
		return model.Value{Type: model.TypeArray}, nil
	}
	if err := model.ValidateArrayShape(dimensions[:rank], uint64(len(values))); err != nil {
		return model.Value{}, arrayDimensionError(specified)
	}
	if cap(values) != len(values) {
		resized, err := resizeArrayInput(ctx, values, len(values))
		if err != nil {
			return model.Value{}, err
		}
		values = resized
	}
	ownedDimensions := make([]model.ArrayDimension, rank)
	copy(ownedDimensions, dimensions[:rank])
	result := model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: ownedDimensions}
	values = nil
	return result, nil
}
