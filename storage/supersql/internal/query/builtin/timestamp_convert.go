package builtin

import (
	"cmp"
	"context"
	"encoding/binary"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimestamp(signature Signature) scalarKernel {
	var kernel scalarKernel
	switch signature.ID {
	case 1026, 2070:
		kernel = func(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
			return timestampAtIntervalZone(args[1].IntVal, args[0].IntervalVal, signature.ID == 1026)
		}
	case 1158:
		kernel = timestampFromUnix
	case 3461:
		kernel = makeTimestamp
	case 1961, 1967:
		kernel = timestampScale
	case 2024, 2025, 1272, 1296, 2029, 1316:
		kernel = func(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
			value := args[0].IntVal
			switch signature.ID {
			case 2029:
				if value == math.MinInt64 {
					return dateValue(math.MinInt32), nil
				}
				if value == math.MaxInt64 {
					return dateValue(math.MaxInt32), nil
				}
				days, _ := splitTimestamp(value)
				if days < -dateEpochJulian {
					return model.Value{}, sqlError("22008", "timestamp out of range")
				}
				return dateValue(days), nil
			case 1316:
				if !finiteTimestamp(value) {
					return model.NullVal(model.TypeTimestamp), nil
				}
				days, micros := splitTimestamp(value)
				if days < -dateEpochJulian {
					return model.Value{}, sqlError("22008", "timestamp out of range")
				}
				return timeValue(micros), nil
			}
			var micros int64
			if signature.ID == 1296 {
				value, micros = args[1].IntVal, value
			} else if len(args) == 2 {
				micros = args[1].IntVal
			}
			result, err := timestampFromDate(value)
			if err != nil {
				return model.Value{}, err
			}
			if len(args) == 2 {
				result, err = timestampAddClock(result, micros)
				if err != nil {
					return model.Value{}, err
				}
			}
			return timestampValue(result), nil
		}
	case 1188, 2031:
		kernel = timestampDifference
	case 2032, 2033, 2548:
		kernel = func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
			value, span := args[0].IntVal, args[1].IntervalVal
			if signature.ID == 2548 {
				value, span = args[1].IntVal, args[0].IntervalVal
			}
			if signature.ID == 2033 {
				negated, err := intervalNegate(ctx, args[1:2], budget)
				if err != nil {
					return model.Value{}, err
				}
				span = negated.IntervalVal
			}
			return timestampAddInterval(value, span)
		}
	case 1152, 1153, 1154, 1155, 1156, 1157, 1195, 1196, 1314, 1389, 2035, 2036, 2045, 2048, 2052, 2053, 2054, 2055, 2056, 2057, 2475, 2477:
		kernel = func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
			left := args[0].IntVal
			if signature.ID == 1389 || signature.ID == 2048 {
				return model.BoolVal(finiteTimestamp(left)), nil
			}
			if signature.ID == 2475 || signature.ID == 2477 {
				if err := outputBudget(8, budget.OutputBytes); err != nil {
					return model.Value{}, err
				}
				result := make([]byte, 8)
				binary.BigEndian.PutUint64(result, uint64(left))
				return model.BytesVal(result), ctx.Err()
			}
			right := args[1].IntVal
			switch signature.ID {
			case 1195, 2035:
				return timestampValue(min(left, right)), nil
			case 1196, 2036:
				return timestampValue(max(left, right)), nil
			case 1314, 2045:
				return model.Int64Val(int64(cmp.Compare(left, right))), nil
			case 1152, 2052:
				return model.BoolVal(left == right), nil
			case 1153, 2053:
				return model.BoolVal(left != right), nil
			case 1154, 2054:
				return model.BoolVal(left < right), nil
			case 1155, 2055:
				return model.BoolVal(left <= right), nil
			case 1156, 2056:
				return model.BoolVal(left >= right), nil
			default:
				return model.BoolVal(left > right), nil
			}
		}
	default:
		return nil
	}
	return kernel
}

func timestampScale(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
	value, precision := args[0].IntVal, args[1].IntVal
	if !finiteTimestamp(value) || precision == -1 || precision == 6 {
		return timestampValue(value), nil
	}
	if precision < 0 || precision > 6 {
		return model.Value{}, sqlError("22023", "timestamp precision must be between 0 and 6")
	}
	scale := int64(1)
	for remaining := int64(6) - precision; remaining > 0; remaining-- {
		scale *= 10
	}
	negative := value < 0
	if negative {
		value = -value
	}
	if !intervalInputAdd(&value, scale/2) {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	value = value / scale * scale
	if negative {
		value = -value
	}
	return timestampValue(value), nil
}

func timestampFromUnix(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
	seconds := args[0].FloatVal
	if math.IsNaN(seconds) {
		return model.Value{}, sqlError("22008", "timestamp cannot be NaN")
	}
	if math.IsInf(seconds, -1) {
		return timestampValue(math.MinInt64), nil
	}
	if math.IsInf(seconds, 1) {
		return timestampValue(math.MaxInt64), nil
	}
	const epochSeconds int64 = 946684800
	const minimumSeconds = timestampMinimum/1000000 + epochSeconds
	const endSeconds = timestampEnd/1000000 + epochSeconds
	if seconds < float64(minimumSeconds) || seconds >= float64(endSeconds) {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	seconds = float64(seconds - float64(epochSeconds))
	microseconds := math.RoundToEven(seconds * 1000000)
	value := int64(microseconds)
	if !validTimestamp(value) {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	return timestampValue(value), nil
}

func timestampAtIntervalZone(value int64, zone model.Interval, toLocal bool) (model.Value, error) {
	if !finiteTimestamp(value) {
		return timestampValue(value), nil
	}
	if intervalInfinity(zone) != 0 {
		return model.Value{}, sqlError("22023", "interval time zone must be finite")
	}
	if zone.Months != 0 || zone.Days != 0 {
		return model.Value{}, sqlError("22023", "interval time zone must not include months or days")
	}
	seconds := zone.Microseconds / intervalMicrosPerSecond
	if toLocal {
		seconds = -seconds
	}
	// PostgreSQL narrows the signed whole-second offset to its int32 tz field.
	offset := int64(int32(seconds)) * intervalMicrosPerSecond
	result, ok := subtractTimestamp(value, offset)
	if !ok || !validTimestamp(result) {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	return timestampValue(result), nil
}

func timestampFromDate(days int64) (int64, error) {
	if !validDateDatum(days) {
		return 0, sqlError("22008", "date out of range")
	}
	if days == math.MinInt32 {
		return math.MinInt64, nil
	}
	if days == math.MaxInt32 {
		return math.MaxInt64, nil
	}
	if days >= timestampEnd/intervalMicrosPerDay {
		return 0, sqlError("22008", "date out of range for timestamp")
	}
	return days * intervalMicrosPerDay, nil
}

func timestampAddClock(value, micros int64) (int64, error) {
	if !finiteTimestamp(value) {
		return value, nil
	}
	value += micros
	if !validTimestamp(value) {
		return 0, sqlError("22008", "timestamp out of range")
	}
	return value, nil
}
