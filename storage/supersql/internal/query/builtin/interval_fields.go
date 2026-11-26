package builtin

import (
	"context"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	intervalFieldQuarter intervalInputUnit = intervalInputUnknown + 1 + iota
	intervalFieldEpoch
	intervalFieldReserved
)

func intervalFieldUnit(text string, extract bool) intervalInputUnit {
	lower := intervalInputKeyword(text)
	length := min(len(text), len(lower))
	for i := range length {
		if lower[i] == 0 {
			length = i
			break
		}
	}
	word := string(lower[:length])
	switch word {
	case "qtr", "quarter":
		return intervalFieldQuarter
	case "mm":
		if extract {
			return intervalInputMinute
		}
		return intervalInputUnknown
	case "epoch":
		if extract {
			return intervalFieldEpoch
		}
		return intervalInputUnknown
	case "infinity", "+infinity", "-infinity", "allballs", "now", "today", "tomorrow", "yesterday":
		if extract {
			return intervalFieldReserved
		}
		return intervalInputUnknown
	case "dow", "doy", "isodow", "isoyear", "j", "jd", "julian":
		if extract {
			return intervalInputUnsupported
		}
		return intervalInputUnknown
	}
	return intervalInputUnitKeyword(word)
}

func compileIntervalField(signature Signature) scalarKernel {
	if signature.ID == 1218 {
		return truncateInterval
	}
	numeric := signature.ID == 6204
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		unit := intervalFieldUnit(args[0].StrVal, true)
		value := args[1].IntervalVal
		if infinity := intervalInfinity(value); infinity != 0 {
			return intervalInfiniteField(unit, infinity, numeric, budget)
		}
		if unit == intervalFieldReserved {
			return model.Value{}, sqlError("22023", "unit not recognized for type interval")
		}
		if unit == intervalFieldEpoch {
			if !numeric {
				result := float64(value.Microseconds) / 1e6
				result = math.FMA(365.25*86400, float64(value.Months/12), result)
				result = math.FMA(30*86400, float64(value.Months%12), result)
				result = math.FMA(86400, float64(value.Days), result)
				return model.Float64Val(result), nil
			}
			seconds := (1461*int64(value.Months/12) + 120*int64(value.Months%12) + 4*int64(value.Days)) * 21600
			return intervalNumericField(ctx, seconds+value.Microseconds/1e6, value.Microseconds%1e6, 6, budget)
		}
		result, err := intervalIntegralField(unit, value)
		if err != nil {
			return model.Value{}, err
		}
		switch unit {
		case intervalInputSecond:
			if numeric {
				return intervalNumericField(ctx, result/1e6, result%1e6, 6, budget)
			}
			return model.Float64Val(float64(result/1e6) + float64(result%1e6)/1e6), nil
		case intervalInputMillisecond:
			if numeric {
				return intervalNumericField(ctx, result/1000, result%1000, 3, budget)
			}
			return model.Float64Val(float64(result/1e6)*1000 + float64(result%1e6)/1000), nil
		}
		if numeric {
			return intervalNumericField(ctx, result, 0, 0, budget)
		}
		return model.Float64Val(float64(result)), nil
	}
}

// unsupportedIntervalUnit reports the datetime_fields profile exclusion for an
// interval field unit the embedded interval kernels do not implement.
func unsupportedIntervalUnit() error {
	return &model.Error{Code: "0A000", Message: "unit not supported for type interval", FeatureID: featureDatetimeFields}
}

func intervalIntegralField(unit intervalInputUnit, value model.Interval) (int64, error) {
	switch unit {
	case intervalInputMicrosecond, intervalInputMillisecond, intervalInputSecond:
		return value.Microseconds % intervalMicrosPerMinute, nil
	case intervalInputMinute:
		return value.Microseconds % intervalMicrosPerHour / intervalMicrosPerMinute, nil
	case intervalInputHour:
		return value.Microseconds / intervalMicrosPerHour, nil
	case intervalInputDay:
		return int64(value.Days), nil
	case intervalInputWeek:
		return int64(value.Days / 7), nil
	case intervalInputMonth:
		return int64(value.Months % 12), nil
	case intervalFieldQuarter:
		// PostgreSQL negates the month component at int32 width.
		if value.Months < 0 {
			return -int64((-value.Months%12)/3 + 1), nil
		}
		return int64(value.Months%12)/3 + 1, nil
	case intervalInputYear:
		return int64(value.Months / 12), nil
	case intervalInputDecade:
		return int64(value.Months / 120), nil
	case intervalInputCentury:
		return int64(value.Months / 1200), nil
	case intervalInputMillennium:
		return int64(value.Months / 12000), nil
	case intervalInputUnsupported:
		return 0, unsupportedIntervalUnit()
	default:
		return 0, sqlError("22023", "unit not recognized for type interval")
	}
}

func intervalInfiniteField(unit intervalInputUnit, sign int, numeric bool, budget int64) (model.Value, error) {
	storage := model.TypeFloat64
	if numeric {
		storage = model.TypeNumeric
	}
	switch unit {
	case intervalInputMicrosecond, intervalInputMillisecond, intervalInputSecond, intervalInputMinute, intervalInputWeek, intervalInputMonth, intervalFieldQuarter:
		return model.NullVal(storage), nil
	case intervalInputHour, intervalInputDay, intervalInputYear, intervalInputDecade, intervalInputCentury, intervalInputMillennium, intervalFieldEpoch:
		if !numeric {
			return model.Float64Val(math.Inf(sign)), nil
		}
		text := "Infinity"
		if sign < 0 {
			text = "-Infinity"
		}
		if err := outputBudget(int64(len(text)), budget); err != nil {
			return model.Value{}, err
		}
		return model.Value{Type: model.TypeNumeric, StrVal: text}, nil
	case intervalInputUnsupported, intervalFieldReserved:
		return model.Value{}, unsupportedIntervalUnit()
	default:
		return model.Value{}, sqlError("22023", "unit not recognized for type interval")
	}
}

func intervalNumericField(ctx context.Context, whole, remainder int64, scale int, budget int64) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	divisor := int64(1)
	for range scale {
		divisor *= 10
	}
	if whole > 0 && remainder < 0 {
		whole--
		remainder += divisor
	}
	if whole < 0 && remainder > 0 {
		whole++
		remainder -= divisor
	}
	var scratch [32]byte
	text := scratch[:0]
	if whole < 0 || remainder < 0 {
		text = append(text, '-')
		whole = -whole
		remainder = -remainder
	}
	text = strconv.AppendInt(text, whole, 10)
	if scale != 0 {
		text = append(text, '.')
		for divisor /= 10; divisor > 0; divisor /= 10 {
			text = append(text, byte('0'+remainder/divisor))
			remainder %= divisor
		}
	}
	if err := outputBudget(int64(len(text)), budget); err != nil {
		return model.Value{}, err
	}
	return model.Value{Type: model.TypeNumeric, StrVal: string(text)}, nil
}

func truncateInterval(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	unit := intervalFieldUnit(args[0].StrVal, false)
	value := args[1].IntervalVal
	switch unit {
	case intervalInputYear, intervalInputMonth, intervalInputDay, intervalInputHour, intervalInputMinute, intervalInputSecond, intervalInputMillisecond, intervalInputMicrosecond, intervalInputDecade, intervalInputCentury, intervalInputMillennium, intervalFieldQuarter:
	case intervalInputWeek, intervalInputUnsupported:
		return model.Value{}, unsupportedIntervalUnit()
	default:
		return model.Value{}, sqlError("22023", "unit not recognized for type interval")
	}
	if intervalInfinity(value) != 0 {
		return intervalValue(value), nil
	}
	switch unit {
	case intervalInputMillennium:
		value.Months = value.Months / 12000 * 12000
		value.Days = 0
		value.Microseconds = 0
	case intervalInputCentury:
		value.Months = value.Months / 1200 * 1200
		value.Days = 0
		value.Microseconds = 0
	case intervalInputDecade:
		value.Months = value.Months / 120 * 120
		value.Days = 0
		value.Microseconds = 0
	case intervalInputYear:
		value.Months = value.Months / 12 * 12
		value.Days = 0
		value.Microseconds = 0
	case intervalFieldQuarter:
		value.Months = value.Months / 3 * 3
		value.Days = 0
		value.Microseconds = 0
	case intervalInputMonth:
		value.Days = 0
		value.Microseconds = 0
	case intervalInputDay:
		value.Microseconds = 0
	case intervalInputHour:
		value.Microseconds = value.Microseconds / intervalMicrosPerHour * intervalMicrosPerHour
	case intervalInputMinute:
		value.Microseconds = value.Microseconds / intervalMicrosPerMinute * intervalMicrosPerMinute
	case intervalInputSecond:
		value.Microseconds = value.Microseconds / intervalMicrosPerSecond * intervalMicrosPerSecond
	case intervalInputMillisecond:
		value.Microseconds = value.Microseconds / 1000 * 1000
	}
	return intervalValue(value), nil
}
