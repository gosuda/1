package builtin

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type catalogVectorValidation uint8

const (
	catalogVectorShape catalogVectorValidation = iota
	catalogVectorSendShape
)

func compileCatalogVector(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 40, 54:
		return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
			return parseCatalogVector(ctx, arguments[0].StrVal, signature.ID == 40, budget)
		}
	case 41, 55:
		return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
			return formatCatalogVector(ctx, arguments[0], signature.ID == 41, budget)
		}
	case 2411, 2421:
		return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
			return sendCatalogVector(ctx, arguments[0], signature.ID == 2411, budget)
		}
	case 457, 776:
		return func(ctx context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
			if err := validateCatalogVector(ctx, arguments[0], false, catalogVectorShape); err != nil {
				return model.Value{}, err
			}
			var seed uint64
			if signature.ID == 776 {
				seed = uint64(arguments[1].IntVal)
			}
			hash, err := hashCatalogVector(ctx, arguments[0].ArrayVal, seed)
			if signature.ID == 457 {
				return model.Int64Val(int64(int32(hash))), err
			}
			return model.Int64Val(int64(hash)), err
		}
	case 404, 619, 677, 678, 679, 680, 681:
		return func(ctx context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
			order, err := compareCatalogVectors(ctx, arguments)
			if err != nil {
				return model.Value{}, err
			}
			switch signature.ID {
			case 404:
				return model.Int64Val(int64(order)), nil
			case 619:
				return model.BoolVal(order != 0), nil
			case 677:
				return model.BoolVal(order < 0), nil
			case 678:
				return model.BoolVal(order <= 0), nil
			case 679:
				return model.BoolVal(order == 0), nil
			case 680:
				return model.BoolVal(order >= 0), nil
			default:
				return model.BoolVal(order > 0), nil
			}
		}
	default:
		return nil
	}
}

func compareCatalogVectors(ctx context.Context, arguments []model.Value) (int, error) {
	for _, value := range arguments {
		if err := validateCatalogVector(ctx, value, false, catalogVectorShape); err != nil {
			return 0, err
		}
	}
	a, b := arguments[0].ArrayVal, arguments[1].ArrayVal
	if len(a) != len(b) {
		return len(a) - len(b), nil
	}
	for i := range a {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if a[i].IntVal < b[i].IntVal {
			return -1, nil
		}
		if a[i].IntVal > b[i].IntVal {
			return 1, nil
		}
	}
	return 0, nil
}

func catalogVectorLimit(budget CallBudget) int64 {
	memory := budget.MemoryBytes
	if budget.Allocator != nil {
		memory = min(memory, budget.Allocator.AvailableBytes())
	}
	return min(budget.OutputBytes, int64(min(memory, uint64(math.MaxInt64))))
}

// PostgreSQL permits array casts with nonzero lower bounds. Only receive insists
// on zero; send uses array_send and also accepts NULLs and arbitrary array ranks.
func validateCatalogVector(ctx context.Context, value model.Value, small bool, mode catalogVectorValidation) error {
	send := mode == catalogVectorSendShape
	name := "oidvector"
	if small {
		name = "int2vector"
	}
	valid := value.Type == model.TypeArray && len(value.ArrayVal) <= math.MaxInt32
	if send {
		valid = valid && model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))) == nil
	} else {
		valid = valid && len(value.Dimensions) == 1 && int64(value.Dimensions[0].Length) == int64(len(value.ArrayVal))
	}
	if !valid {
		return sqlError("42804", "array is not a valid "+name)
	}
	for i, item := range value.ArrayVal {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if item.Type != model.TypeInt64 || (item.IsNull && !send) {
			return sqlError("42804", "array is not a valid "+name)
		}
		if item.IsNull {
			continue
		}
		if small {
			if err := integerRange(item.IntVal, 21); err != nil {
				return err
			}
		} else if item.IntVal < 0 || item.IntVal > math.MaxUint32 {
			return sqlError("22003", "OID argument is outside uint32 range")
		}
	}
	return ctx.Err()
}

func parseCatalogVector(ctx context.Context, input string, small bool, budget CallBudget) (model.Value, error) {
	if _, err := textCharacters(ctx, input); err != nil {
		return model.Value{}, err
	}
	limit := catalogVectorLimit(budget)
	count := 0
	for position := 0; ; {
		_, next, found, err := catalogVectorToken(ctx, input, position, small)
		if err != nil {
			return model.Value{}, err
		}
		if !found {
			break
		}
		count++
		if err := outputBudget(int64(count)*arrayValueBytes+arrayDimensionBytes, limit); err != nil {
			return model.Value{}, err
		}
		position = next
	}
	if err := outputBudget(int64(count)*arrayValueBytes+arrayDimensionBytes, limit); err != nil {
		return model.Value{}, err
	}
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(uint64(count)*uint64(arrayValueBytes) + uint64(arrayDimensionBytes))
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	values := make([]model.Value, count)
	position := 0
	for i := range values {
		value, next, _, err := catalogVectorToken(ctx, input, position, small)
		if err != nil {
			return model.Value{}, err
		}
		values[i] = model.Int64Val(value)
		position = next
	}
	return model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{LowerBound: 0, Length: int32(count)}}}, nil
}

// Match strtol(base 10) for int2vector and strtoul(base 0) for oidvector.
// OID tokens need no delimiter: "08" is two values, while int2 requires a space.
func catalogVectorToken(ctx context.Context, input string, position int, small bool) (int64, int, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, position, false, err
	}
	for position < len(input) && isTSSpace(input[position]) {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, position, false, err
			}
		}
		position++
	}
	if position == len(input) {
		return 0, position, false, nil
	}
	negative := input[position] == '-'
	if negative || input[position] == '+' {
		position++
	}
	base := uint64(10)
	if !small && position < len(input) && input[position] == '0' {
		base = 8
		hasHexDigits := position+2 < len(input) && catalogVectorDigit(input[position+2]) < 16
		if hasHexDigits && (input[position+1] == 'x' || input[position+1] == 'X') {
			base = 16
			position += 2
		}
	}
	start := position
	var value uint64
	overflow := false
	for position < len(input) {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, position, false, err
			}
		}
		digit := catalogVectorDigit(input[position])
		if digit >= base {
			break
		}
		if value > (math.MaxUint64-digit)/base {
			overflow = true
		}
		value = value*base + digit
		position++
	}
	if start == position {
		return 0, position, false, invalidInput()
	}
	if small {
		maximum := uint64(math.MaxInt16)
		if negative {
			maximum++
		}
		if overflow || value > maximum {
			return 0, position, false, sqlError("22003", "smallint out of range")
		}
		if position < len(input) && input[position] != ' ' {
			return 0, position, false, invalidInput()
		}
		result := int64(value)
		if negative {
			result = -result
		}
		return result, position, true, nil
	}
	if negative {
		value = -value
	}
	if overflow || (value != uint64(uint32(value)) && value != uint64(int64(int32(value)))) {
		return 0, position, false, sqlError("22003", "oid out of range")
	}
	return int64(uint32(value)), position, true, nil
}

func catalogVectorDigit(value byte) uint64 {
	switch {
	case value >= '0' && value <= '9':
		return uint64(value - '0')
	case value >= 'a' && value <= 'f':
		return uint64(value-'a') + 10
	case value >= 'A' && value <= 'F':
		return uint64(value-'A') + 10
	default:
		return 16
	}
}

func formatCatalogVector(ctx context.Context, value model.Value, small bool, budget CallBudget) (model.Value, error) {
	if err := validateCatalogVector(ctx, value, small, catalogVectorShape); err != nil {
		return model.Value{}, err
	}
	var scratch [20]byte
	size := int64(max(0, len(value.ArrayVal)-1))
	for i, item := range value.ArrayVal {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		size += int64(len(strconv.AppendInt(scratch[:0], item.IntVal, 10)))
	}
	if err := outputBudget(size, catalogVectorLimit(budget)); err != nil {
		return model.Value{}, err
	}
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(uint64(size))
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	var output strings.Builder
	output.Grow(int(size))
	for i, item := range value.ArrayVal {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		if i != 0 {
			output.WriteByte(' ')
		}
		output.Write(strconv.AppendInt(scratch[:0], item.IntVal, 10))
	}
	return model.StringVal(output.String()), nil
}

func sendCatalogVector(ctx context.Context, value model.Value, small bool, budget CallBudget) (model.Value, error) {
	if err := validateCatalogVector(ctx, value, small, catalogVectorSendShape); err != nil {
		return model.Value{}, err
	}
	width, element := 4, uint32(26)
	if small {
		width, element = 2, 21
	}
	size := int64(12 + 8*len(value.Dimensions))
	var hasNull uint32
	for i, item := range value.ArrayVal {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		size += 4
		if item.IsNull {
			hasNull = 1
		} else {
			size += int64(width)
		}
	}
	if err := outputBudget(size, catalogVectorLimit(budget)); err != nil {
		return model.Value{}, err
	}
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(uint64(size))
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	output := make([]byte, int(size))
	binary.BigEndian.PutUint32(output, uint32(len(value.Dimensions)))
	binary.BigEndian.PutUint32(output[4:], hasNull)
	binary.BigEndian.PutUint32(output[8:], element)
	position := 12
	for _, dimension := range value.Dimensions {
		binary.BigEndian.PutUint32(output[position:], uint32(dimension.Length))
		binary.BigEndian.PutUint32(output[position+4:], uint32(dimension.LowerBound))
		position += 8
	}
	for i, item := range value.ArrayVal {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		if item.IsNull {
			binary.BigEndian.PutUint32(output[position:], math.MaxUint32)
			position += 4
			continue
		}
		binary.BigEndian.PutUint32(output[position:], uint32(width))
		position += 4
		if small {
			binary.BigEndian.PutUint16(output[position:], uint16(item.IntVal))
		} else {
			binary.BigEndian.PutUint32(output[position:], uint32(item.IntVal))
		}
		position += width
	}
	return model.BytesVal(output), nil
}

// Feed the existing PostgreSQL hash mixer directly from the declared OID lanes,
// avoiding an otherwise unnecessary temporary packed byte array.
func hashCatalogVector(ctx context.Context, values []model.Value, seed uint64) (uint64, error) {
	if len(values) > math.MaxInt32/4 {
		return 0, sqlError("54000", "hash input exceeds PostgreSQL length limit")
	}
	a := uint32(0x9e3779b9) + uint32(len(values)*4) + 3923095
	b, c := a, a
	if seed != 0 {
		a += uint32(seed >> 32)
		b += uint32(seed)
		a, b, c = pgHashMix(a, b, c)
	}
	for len(values) >= 3 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		end := min(len(values)/3, 256) * 3
		for i := 0; i < end; i += 3 {
			a += uint32(values[i].IntVal)
			b += uint32(values[i+1].IntVal)
			c += uint32(values[i+2].IntVal)
			a, b, c = pgHashMix(a, b, c)
		}
		values = values[end:]
	}
	if len(values) > 0 {
		a += uint32(values[0].IntVal)
	}
	if len(values) > 1 {
		b += uint32(values[1].IntVal)
	}
	b, c = pgHashFinal(a, b, c)
	return uint64(b)<<32 | uint64(c), ctx.Err()
}
