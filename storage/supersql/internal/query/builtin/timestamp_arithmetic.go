package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func timestampDifference(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
	left, right := args[0].IntVal, args[1].IntVal
	if !finiteTimestamp(left) || !finiteTimestamp(right) {
		if left == right {
			return model.Value{}, intervalArithmeticOutOfRange()
		}
		sign := 1
		if left == math.MinInt64 || right == math.MaxInt64 {
			sign = -1
		}
		return intervalValue(intervalInfinityValue(sign)), nil
	}
	micros, ok := subtractTimestamp(left, right)
	if !ok {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	return intervalValue(model.Interval{Days: int32(micros / intervalMicrosPerDay), Microseconds: micros % intervalMicrosPerDay}), nil
}

func timestampAddInterval(value int64, span model.Interval) (model.Value, error) {
	if infinity := intervalInfinity(span); infinity != 0 {
		negativeConflict := infinity < 0 && value == math.MaxInt64
		positiveConflict := infinity > 0 && value == math.MinInt64
		if negativeConflict || positiveConflict {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		if infinity < 0 {
			return timestampValue(math.MinInt64), nil
		}
		return timestampValue(math.MaxInt64), nil
	}
	if !finiteTimestamp(value) {
		return timestampValue(value), nil
	}
	if span.Months != 0 {
		days, micros := splitTimestamp(value)
		if days < -dateEpochJulian {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		shifted, err := shiftTimestampMonths(days, span.Months)
		if err != nil {
			return model.Value{}, err
		}
		value, err = composeTimestamp(shifted, micros)
		if err != nil {
			return model.Value{}, err
		}
	}
	if span.Days != 0 {
		days, micros := splitTimestamp(value)
		if days < -dateEpochJulian {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		julian := days + dateEpochJulian + int64(span.Days)
		if julian < 0 || julian > math.MaxInt32 {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		var err error
		value, err = composeTimestamp(julian-dateEpochJulian, micros)
		if err != nil {
			return model.Value{}, err
		}
	}
	if !intervalInputAdd(&value, span.Microseconds) || !validTimestamp(value) {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	return timestampValue(value), nil
}

func shiftTimestampMonths(days int64, months int32) (int64, error) {
	year, month, day := dateCalendar(days)
	month += int64(months)
	if month < math.MinInt32 || month > math.MaxInt32 {
		return 0, sqlError("22008", "timestamp out of range")
	}
	if month > 12 {
		year += (month - 1) / 12
		month = (month-1)%12 + 1
	} else if month < 1 {
		year += month/12 - 1
		month = month%12 + 12
	}
	day = min(day, timestampMonthDays(year, month))
	if !validTimestampCalendar(year, month) {
		return 0, sqlError("22008", "timestamp out of range")
	}
	return dateFieldJulianDay(year, month, day) - dateEpochJulian, nil
}
