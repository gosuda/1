package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func timestampMonthDays(year, month int64) int64 {
	switch month {
	case 4, 6, 9, 11:
		return 30
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	default:
		return 31
	}
}

func makeTimestamp(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
	year, month, day := args[0].IntVal, args[1].IntVal, args[2].IntVal
	if year == 0 || year == math.MinInt32 {
		return model.Value{}, sqlError("22008", "date field value out of range")
	}
	if year < 0 {
		year++
	}
	if month < 1 || month > 12 || day < 1 || day > timestampMonthDays(year, month) {
		return model.Value{}, sqlError("22008", "date field value out of range")
	}
	if !validTimestampCalendar(year, month) {
		return model.Value{}, sqlError("22008", "date out of range")
	}
	hour, minute := args[3].IntVal, args[4].IntVal
	seconds := math.RoundToEven(args[5].FloatVal * float64(intervalMicrosPerSecond))
	invalidHour := hour < 0 || hour > 24
	invalidMinute := minute < 0 || minute >= 60
	invalidSecond := math.IsNaN(seconds) || seconds < 0 || seconds > float64(intervalMicrosPerMinute)
	if invalidHour || invalidMinute || invalidSecond {
		return model.Value{}, sqlError("22008", "time field value out of range")
	}
	micros := hour*intervalMicrosPerHour + minute*intervalMicrosPerMinute + int64(seconds)
	if micros > intervalMicrosPerDay {
		return model.Value{}, sqlError("22008", "time field value out of range")
	}
	days := dateFieldJulianDay(year, month, day) - dateEpochJulian
	days += micros / intervalMicrosPerDay
	micros %= intervalMicrosPerDay
	value, err := composeTimestamp(days, micros)
	if err != nil {
		return model.Value{}, err
	}
	return timestampValue(value), nil
}
