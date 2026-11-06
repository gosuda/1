package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// IntervalTypeModifier returns the packed PostgreSQL modifier and any precision
// reduction warning. Modifiers are the parser's field mask and optional precision.
func IntervalTypeModifier(modifiers []int32) (int32, *model.Error, error) {
	if len(modifiers) == 0 || len(modifiers) > 2 {
		return 0, nil, sqlError("22023", "invalid INTERVAL type modifier")
	}
	fields := modifiers[0]
	if !validIntervalRange(fields) {
		return 0, nil, sqlError("22023", "invalid INTERVAL type modifier")
	}
	if len(modifiers) == 1 {
		if fields == intervalFullRange {
			return -1, nil, nil
		}
		return fields<<16 | intervalFullPrecision, nil, nil
	}
	precision := modifiers[1]
	if precision < 0 {
		return 0, nil, sqlError("22023", "INTERVAL precision must not be negative")
	}
	var warning *model.Error
	if precision > 6 {
		precision = 6
		warning = &model.Error{Code: "22023", Message: "INTERVAL precision reduced to maximum allowed, 6"}
	}
	return fields<<16 | precision, warning, nil
}

func validIntervalRange(fields int32) bool {
	switch fields {
	case intervalYearMask, intervalMonthMask, intervalDayMask, intervalHourMask, intervalMinuteMask, intervalSecondMask,
		intervalYearMask | intervalMonthMask,
		intervalDayMask | intervalHourMask,
		intervalDayMask | intervalHourMask | intervalMinuteMask,
		intervalDayMask | intervalHourMask | intervalMinuteMask | intervalSecondMask,
		intervalHourMask | intervalMinuteMask,
		intervalHourMask | intervalMinuteMask | intervalSecondMask,
		intervalMinuteMask | intervalSecondMask, intervalFullRange:
		return true
	default:
		return false
	}
}

func applyIntervalTypmod(value model.Interval, typmod int32) (model.Interval, error) {
	if intervalInfinity(value) != 0 || typmod < 0 {
		return value, nil
	}
	fields, precision := (typmod>>16)&intervalFullRange, typmod&intervalFullPrecision
	switch fields {
	case intervalFullRange, intervalSecondMask,
		intervalDayMask | intervalHourMask | intervalMinuteMask | intervalSecondMask,
		intervalHourMask | intervalMinuteMask | intervalSecondMask,
		intervalMinuteMask | intervalSecondMask:
	case intervalYearMask:
		value.Months = value.Months / 12 * 12
		value.Days = 0
		value.Microseconds = 0
	case intervalMonthMask, intervalYearMask | intervalMonthMask:
		value.Days = 0
		value.Microseconds = 0
	case intervalDayMask:
		value.Microseconds = 0
	case intervalHourMask, intervalDayMask | intervalHourMask:
		value.Microseconds = value.Microseconds / intervalMicrosPerHour * intervalMicrosPerHour
	case intervalMinuteMask, intervalDayMask | intervalHourMask | intervalMinuteMask, intervalHourMask | intervalMinuteMask:
		value.Microseconds = value.Microseconds / intervalMicrosPerMinute * intervalMicrosPerMinute
	default:
		return model.Interval{}, sqlError("XX000", "unrecognized interval typmod")
	}
	if precision == intervalFullPrecision {
		return value, nil
	}
	if precision > 6 {
		return model.Interval{}, sqlError("22023", "interval precision must be between 0 and 6")
	}
	scales := [7]int64{1_000_000, 100_000, 10_000, 1000, 100, 10, 1}
	scale := scales[precision]
	offset := scale / 2
	if value.Microseconds < 0 {
		offset = -offset
	}
	if offset > 0 && value.Microseconds > math.MaxInt64-offset {
		return model.Interval{}, sqlError("22008", "interval out of range")
	}
	if offset < 0 && value.Microseconds < math.MinInt64-offset {
		return model.Interval{}, sqlError("22008", "interval out of range")
	}
	value.Microseconds += offset
	value.Microseconds -= value.Microseconds % scale
	return value, nil
}

func intervalTypmod(_ context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	value, err := applyIntervalTypmod(arguments[0].IntervalVal, int32(arguments[1].IntVal))
	if err != nil {
		return model.Value{}, err
	}
	return intervalValue(value), nil
}
