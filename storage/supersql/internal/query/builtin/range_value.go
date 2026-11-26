package builtin

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type rangeBoundSide uint8

const (
	rangeLower rangeBoundSide = iota
	rangeUpper
)

// Numeric text in a decoded bound borrows the immutable input payload. It must
// not escape a synchronous call except through ownedRangeBoundValue or encoding.
type rangeBound struct {
	integer   int64
	floating  float64
	numeric   string
	bytes     []byte
	interval  model.Interval
	infinite  bool
	inclusive bool
	side      rangeBoundSide
}
type rangeDatum struct {
	lower, upper rangeBound
	empty        bool
}

const (
	rangeEmptyFlag          byte = 1
	rangeLowerInclusiveFlag byte = 2
	rangeUpperInclusiveFlag byte = 4
	rangeLowerInfiniteFlag  byte = 8
	rangeUpperInfiniteFlag  byte = 16
)

func invalidRangeValue() error { return sqlError("22P03", "invalid binary range value") }

// Range payloads contain flags followed by finite bounds in lower/upper order.
// Fixed-width bounds use canonical big-endian fields. Variable-width bounds use
// uint32 lengths and borrowed payload slices. Multiranges prefix each range with
// uint32 length and prefix the collection with uint32 count.
func decodeRange(ctx context.Context, family RangeFamilyInfo, value model.Value) (rangeDatum, error) {
	if err := ctx.Err(); err != nil {
		return rangeDatum{}, err
	}
	if value.Type != model.TypeBytes {
		return rangeDatum{}, invalidRangeValue()
	}
	return decodeRangeBytes(family, value.BytesVal)
}
func decodeRangeBytes(family RangeFamilyInfo, data []byte) (rangeDatum, error) {
	result := rangeDatum{lower: rangeBound{side: rangeLower}, upper: rangeBound{side: rangeUpper}}
	if len(data) == 0 || data[0]&^byte(31) != 0 {
		return result, invalidRangeValue()
	}
	flags := data[0]
	if flags&rangeEmptyFlag != 0 {
		if flags != rangeEmptyFlag || len(data) != 1 {
			return result, invalidRangeValue()
		}
		result.empty = true
		return result, nil
	}
	result.lower.infinite = flags&rangeLowerInfiniteFlag != 0
	result.upper.infinite = flags&rangeUpperInfiniteFlag != 0
	result.lower.inclusive = flags&rangeLowerInclusiveFlag != 0
	result.upper.inclusive = flags&rangeUpperInclusiveFlag != 0
	invalidLower := result.lower.infinite && result.lower.inclusive
	invalidUpper := result.upper.infinite && result.upper.inclusive
	if invalidLower || invalidUpper {
		return result, invalidRangeValue()
	}
	offset := 1
	for _, bound := range []*rangeBound{&result.lower, &result.upper} {
		if bound.infinite {
			continue
		}
		switch family.subtypeStorage() {
		case model.TypeNumeric, model.TypeString, model.TypeUUID:
			if len(data)-offset < 4 {
				return result, invalidRangeValue()
			}
			size := uint64(binary.BigEndian.Uint32(data[offset:]))
			offset += 4
			if size > uint64(len(data)-offset) {
				return result, invalidRangeValue()
			}
			text := data[offset : offset+int(size)]
			bound.numeric = unsafe.String(unsafe.SliceData(text), len(text))
			offset += int(size)
		case model.TypeBytes, model.TypeJSONB:
			if len(data)-offset < 4 {
				return result, invalidRangeValue()
			}
			size := uint64(binary.BigEndian.Uint32(data[offset:]))
			offset += 4
			if size > uint64(len(data)-offset) {
				return result, invalidRangeValue()
			}
			bound.bytes = data[offset : offset+int(size)]
			offset += int(size)
		case model.TypeFloat64:
			if len(data)-offset < 8 {
				return result, invalidRangeValue()
			}
			bound.floating = math.Float64frombits(binary.BigEndian.Uint64(data[offset:]))
			offset += 8
		case model.TypeInterval:
			if len(data)-offset < 16 {
				return result, invalidRangeValue()
			}
			bound.interval.Months = int32(binary.BigEndian.Uint32(data[offset:]))
			bound.interval.Days = int32(binary.BigEndian.Uint32(data[offset+4:]))
			bound.interval.Microseconds = int64(binary.BigEndian.Uint64(data[offset+8:]))
			offset += 16
		case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
			if len(data)-offset < 8 {
				return result, invalidRangeValue()
			}
			bound.integer = int64(binary.BigEndian.Uint64(data[offset:]))
			offset += 8
			narrow := family.representation() == 23 || family.representation() == 1082
			if narrow && (bound.integer < math.MinInt32 || bound.integer > math.MaxInt32) {
				return result, invalidRangeValue()
			}
		default:
			return result, invalidRangeValue()
		}
	}
	if offset != len(data) {
		return result, invalidRangeValue()
	}
	return result, nil
}

func rangeBoundFromValue(family RangeFamilyInfo, value model.Value, side rangeBoundSide, inclusive bool) (rangeBound, error) {
	bound := rangeBound{side: side, inclusive: inclusive}
	if value.Type != family.subtypeStorage() {
		return bound, sqlError("42804", "range bound storage differs from subtype")
	}
	if value.IsNull {
		bound.infinite = true
		bound.inclusive = false
		return bound, nil
	}
	switch family.subtypeStorage() {
	case model.TypeNumeric, model.TypeString, model.TypeUUID:
		bound.numeric = value.StrVal
	case model.TypeBytes, model.TypeJSONB:
		bound.bytes = value.BytesVal
	case model.TypeFloat64:
		bound.floating = value.FloatVal
	case model.TypeInterval:
		bound.interval = value.IntervalVal
	default:
		bound.integer = value.IntVal
	}
	return bound, nil
}
func borrowRangeBoundValue(family RangeFamilyInfo, bound rangeBound) model.Value {
	value := model.Value{Type: family.subtypeStorage(), IsNull: bound.infinite}
	switch family.subtypeStorage() {
	case model.TypeNumeric, model.TypeString, model.TypeUUID:
		value.StrVal = bound.numeric
	case model.TypeBytes, model.TypeJSONB:
		value.BytesVal = bound.bytes
	case model.TypeFloat64:
		value.FloatVal = bound.floating
	case model.TypeInterval:
		value.IntervalVal = bound.interval
	default:
		value.IntVal = bound.integer
	}
	return value
}
func ownedRangeBoundValue(ctx context.Context, family RangeFamilyInfo, bound rangeBound, budget CallBudget) (model.Value, error) {
	value := borrowRangeBoundValue(family, bound)
	if value.IsNull || (!family.textStorage() && value.Type != model.TypeBytes && value.Type != model.TypeJSONB) {
		return value, ctx.Err()
	}
	if budget.OutputBytes < 0 {
		return model.Value{}, sqlError("22023", "negative range bound output budget")
	}
	owned, _, err := model.CopyValue(ctx, value, min(budget.MemoryBytes, uint64(budget.OutputBytes)))
	if err != nil {
		return model.Value{}, rangeBoundResourceError(err)
	}
	return owned, nil
}

// rangeBoundResourceError reports a bound copy that exceeded the caller's byte
// budget through the SQLSTATE the range functions use for resource limits.
func rangeBoundResourceError(err error) error {
	if errors.Is(err, model.ErrValueBudget) || errors.Is(err, model.ErrValueDepth) {
		return &model.Error{Code: "54000", Message: "range bound exceeds the output byte budget", Cause: err}
	}
	return err
}

func rangeEncodedSize(family RangeFamilyInfo, value rangeDatum) (uint64, error) {
	size := uint64(1)
	if value.empty {
		return size, nil
	}
	for _, bound := range [2]rangeBound{value.lower, value.upper} {
		if bound.infinite {
			continue
		}
		switch family.subtypeStorage() {
		case model.TypeNumeric, model.TypeString, model.TypeUUID:
			if family.subtypeStorage() == model.TypeNumeric && len(bound.numeric) == 0 {
				return 0, invalidRangeValue()
			}
			if uint64(len(bound.numeric)) > math.MaxUint32 {
				return 0, sqlError("54000", "range bound exceeds encoding limit")
			}
			size += 4 + uint64(len(bound.numeric))
		case model.TypeBytes, model.TypeJSONB:
			if uint64(len(bound.bytes)) > math.MaxUint32 {
				return 0, sqlError("54000", "range bound exceeds encoding limit")
			}
			size += 4 + uint64(len(bound.bytes))
		case model.TypeInterval:
			size += 16
		case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean, model.TypeFloat64:
			size += 8
		default:
			return 0, sqlError("42804", "range subtype storage is unsupported")
		}
	}
	return size, nil
}
func appendRangeBytes(ctx context.Context, output []byte, family RangeFamilyInfo, value rangeDatum) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if value.empty {
		return append(output, rangeEmptyFlag), nil
	}
	flags := byte(0)
	if value.lower.infinite {
		flags |= rangeLowerInfiniteFlag
	} else if value.lower.inclusive {
		flags |= rangeLowerInclusiveFlag
	}
	if value.upper.infinite {
		flags |= rangeUpperInfiniteFlag
	} else if value.upper.inclusive {
		flags |= rangeUpperInclusiveFlag
	}
	output = append(output, flags)
	for _, bound := range [2]rangeBound{value.lower, value.upper} {
		if bound.infinite {
			continue
		}
		switch family.subtypeStorage() {
		case model.TypeNumeric, model.TypeString, model.TypeUUID:
			output = binary.BigEndian.AppendUint32(output, uint32(len(bound.numeric)))
			for start := 0; start < len(bound.numeric); start += 4096 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				output = append(output, bound.numeric[start:min(start+4096, len(bound.numeric))]...)
			}
		case model.TypeBytes, model.TypeJSONB:
			output = binary.BigEndian.AppendUint32(output, uint32(len(bound.bytes)))
			for start := 0; start < len(bound.bytes); start += 4096 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				output = append(output, bound.bytes[start:min(start+4096, len(bound.bytes))]...)
			}
		case model.TypeFloat64:
			output = binary.BigEndian.AppendUint64(output, math.Float64bits(bound.floating))
		case model.TypeInterval:
			output = binary.BigEndian.AppendUint32(output, uint32(bound.interval.Months))
			output = binary.BigEndian.AppendUint32(output, uint32(bound.interval.Days))
			output = binary.BigEndian.AppendUint64(output, uint64(bound.interval.Microseconds))
		default:
			output = binary.BigEndian.AppendUint64(output, uint64(bound.integer))
		}
	}
	return output, ctx.Err()
}
func encodeRange(ctx context.Context, family RangeFamilyInfo, value rangeDatum, budget CallBudget) (model.Value, error) {
	size, err := rangeEncodedSize(family, value)
	if err != nil {
		return model.Value{}, err
	}
	buffer, err := newByteResult(ctx, size, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	if _, err := appendRangeBytes(ctx, buffer.data[:0], family, value); err != nil {
		return model.Value{}, err
	}
	return buffer.value(), nil
}

type multirangeCursor struct {
	family    RangeFamilyInfo
	data      []byte
	offset    int
	remaining uint32
}

func newMultirangeCursor(family RangeFamilyInfo, value model.Value) (multirangeCursor, error) {
	cursor := multirangeCursor{family: family, data: value.BytesVal, offset: 4}
	if value.Type != model.TypeBytes || len(value.BytesVal) < 4 {
		return cursor, invalidRangeValue()
	}
	cursor.remaining = binary.BigEndian.Uint32(value.BytesVal)
	if cursor.remaining > math.MaxInt32 || uint64(cursor.remaining) > uint64(len(value.BytesVal)-4)/5 {
		return cursor, invalidRangeValue()
	}
	return cursor, nil
}
func (cursor *multirangeCursor) next(ctx context.Context) (rangeDatum, bool, error) {
	if err := ctx.Err(); err != nil {
		return rangeDatum{}, false, err
	}
	if cursor.remaining == 0 {
		if cursor.offset != len(cursor.data) {
			return rangeDatum{}, false, invalidRangeValue()
		}
		return rangeDatum{}, false, nil
	}
	if len(cursor.data)-cursor.offset < 4 {
		return rangeDatum{}, false, invalidRangeValue()
	}
	size := uint64(binary.BigEndian.Uint32(cursor.data[cursor.offset:]))
	cursor.offset += 4
	if size == 0 || size > uint64(len(cursor.data)-cursor.offset) {
		return rangeDatum{}, false, invalidRangeValue()
	}
	value, err := decodeRangeBytes(cursor.family, cursor.data[cursor.offset:cursor.offset+int(size)])
	if err != nil || value.empty {
		return rangeDatum{}, false, invalidRangeValue()
	}
	cursor.offset += int(size)
	cursor.remaining--
	return value, true, nil
}

// ValidateRangeValue checks the physical envelope without inferring subtype
// identities or normalizing user-provided payloads.
func ValidateRangeValue(ctx context.Context, id model.TypeID, data []byte) error {
	family, known := LookupRangeFamily(id)
	if !known {
		return invalidRangeValue()
	}
	value := model.BytesVal(data)
	if id == family.Range {
		_, err := decodeRange(ctx, family, value)
		return err
	}
	cursor, err := newMultirangeCursor(family, value)
	if err != nil {
		return err
	}
	for {
		_, more, err := cursor.next(ctx)
		if err != nil || !more {
			return err
		}
	}
}
