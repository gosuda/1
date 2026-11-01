package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type intervalInputStatus uint8

const (
	intervalInputOK intervalInputStatus = iota
	intervalInputBadFormat
	intervalInputOverflow
)

// Years remain separate until itmin2interval's final checked month conversion.
type intervalInputParts struct {
	years    int32
	months   int32
	days     int32
	micros   int64
	infinity int
}

func compileIntervalInput(_ Signature, options ResolveOptions) scalarKernel {
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		typmod := int32(args[2].IntVal)
		fieldRange := intervalFullRange
		if typmod >= 0 {
			fieldRange = typmod >> 16 & intervalFullRange
		}
		var fields intervalInputFields
		status, err := fields.scan(ctx, args[0].StrVal)
		if err != nil {
			return model.Value{}, err
		}
		var parts intervalInputParts
		if status == intervalInputOK {
			status = parts.decode(&fields, fieldRange, options.IntervalStyle)
		}
		if status == intervalInputBadFormat {
			parts = intervalInputParts{}
			status, err = parts.decodeISO(ctx, args[0].StrVal)
			if err != nil {
				return model.Value{}, err
			}
		}
		if status == intervalInputBadFormat {
			return model.Value{}, sqlError("22007", "invalid input syntax for type interval")
		}
		if status == intervalInputOverflow {
			return model.Value{}, sqlError("22015", "interval field value out of range")
		}
		value := model.Interval{}
		if parts.infinity != 0 {
			value = intervalInfinityValue(parts.infinity)
		} else {
			months := int64(parts.years)*12 + int64(parts.months)
			if months < math.MinInt32 || months > math.MaxInt32 {
				return model.Value{}, sqlError("22008", "interval out of range")
			}
			value = model.Interval{Months: int32(months), Days: parts.days, Microseconds: parts.micros}
		}
		value, err = applyIntervalTypmod(value, typmod)
		if err != nil {
			return model.Value{}, err
		}
		return intervalValue(value), nil
	}
}

func intervalInputAdd(sum *int64, value int64) bool {
	result := *sum + value
	positiveOverflow := value > 0 && result < *sum
	negativeOverflow := value < 0 && result > *sum
	if positiveOverflow || negativeOverflow {
		return false
	}
	*sum = result
	return true
}

func intervalInputMultiplyAdd(sum *int64, value, scale int64) bool {
	if value > math.MaxInt64/scale || value < math.MinInt64/scale {
		return false
	}
	return intervalInputAdd(sum, value*scale)
}

func intervalInputAdd32(sum *int32, value int64, scale int64) bool {
	if value < math.MinInt32 || value > math.MaxInt32 {
		return false
	}
	value *= scale
	if value < math.MinInt32 || value > math.MaxInt32 {
		return false
	}
	value += int64(*sum)
	if value < math.MinInt32 || value > math.MaxInt32 {
		return false
	}
	*sum = int32(value)
	return true
}

func (parts *intervalInputParts) fractionalMicros(fraction float64, scale int64) bool {
	fraction *= float64(scale)
	micros := int64(fraction)
	fraction -= float64(micros)
	// DecodeInterval rounds exact half-microseconds toward zero, unlike time fields.
	if fraction > 0.5 {
		micros++
	} else if fraction < -0.5 {
		micros--
	}
	return intervalInputAdd(&parts.micros, micros)
}

func (parts *intervalInputParts) fractionalDays(fraction float64, scale int64) bool {
	fraction *= float64(scale)
	days := int64(fraction)
	return intervalInputAdd32(&parts.days, days, 1) && parts.fractionalMicros(fraction-float64(days), intervalMicrosPerDay)
}

func (parts *intervalInputParts) fractionalYears(fraction float64, scale int64) bool {
	months := math.RoundToEven(fraction * float64(scale) * 12)
	return intervalInputAdd32(&parts.months, int64(months), 1)
}

func (parts *intervalInputParts) addMicros(value int64, fraction float64, scale int64) bool {
	return intervalInputMultiplyAdd(&parts.micros, value, scale) && parts.fractionalMicros(fraction, scale)
}

func (parts *intervalInputParts) addUnit(unit intervalInputUnit, value int64, fraction float64) bool {
	switch unit {
	case intervalInputMicrosecond:
		return parts.addMicros(value, fraction, 1)
	case intervalInputMillisecond:
		return parts.addMicros(value, fraction, 1000)
	case intervalInputSecond:
		return parts.addMicros(value, fraction, intervalMicrosPerSecond)
	case intervalInputMinute:
		return parts.addMicros(value, fraction, intervalMicrosPerMinute)
	case intervalInputHour:
		return parts.addMicros(value, fraction, intervalMicrosPerHour)
	case intervalInputDay:
		return intervalInputAdd32(&parts.days, value, 1) && parts.fractionalMicros(fraction, intervalMicrosPerDay)
	case intervalInputWeek:
		return intervalInputAdd32(&parts.days, value, 7) && parts.fractionalDays(fraction, 7)
	case intervalInputMonth:
		return intervalInputAdd32(&parts.months, value, 1) && parts.fractionalDays(fraction, intervalDaysPerMonth)
	case intervalInputYear, intervalInputDecade, intervalInputCentury, intervalInputMillennium:
		scale := int64(1)
		switch unit {
		case intervalInputDecade:
			scale = 10
		case intervalInputCentury:
			scale = 100
		case intervalInputMillennium:
			scale = 1000
		}
		return intervalInputAdd32(&parts.years, value, scale) && parts.fractionalYears(fraction, scale)
	default:
		return false
	}
}
