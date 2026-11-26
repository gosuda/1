package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimestampCalendar(signature Signature) scalarKernel {
	switch signature.ID {
	case 2020:
		return truncateTimestamp
	case 2058:
		return timestampAge
	default:
		return nil
	}
}

func truncateTimestamp(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
	unit, supported, err := timestampTruncUnit(args[0].StrVal, "timestamp without time zone")
	if err != nil {
		return model.Value{}, err
	}
	value := args[1].IntVal
	if !finiteTimestamp(value) {
		if !supported {
			return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp without time zone", true)
		}
		return timestampValue(value), nil
	}
	days, micros := splitTimestamp(value)
	if days < -dateEpochJulian {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	if !supported {
		return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp without time zone", true)
	}
	days, micros, err = truncateTimestampParts(unit, days, micros)
	if err != nil {
		return model.Value{}, err
	}
	result, err := composeTimestamp(days, micros)
	if err != nil {
		return model.Value{}, err
	}
	return timestampValue(result), nil
}

func timestampTruncUnit(text, typeName string) (intervalInputUnit, bool, error) {
	unit := intervalFieldUnit(text, false)
	switch unit {
	case intervalInputWeek, intervalInputMillennium, intervalInputCentury, intervalInputDecade,
		intervalInputYear, intervalFieldQuarter, intervalInputMonth, intervalInputDay,
		intervalInputHour, intervalInputMinute, intervalInputSecond, intervalInputMillisecond, intervalInputMicrosecond:
		return unit, true, nil
	case intervalInputUnsupported:
		return unit, false, nil
	default:
		return unit, false, temporalFieldError(text, typeName, false)
	}
}

func truncateTimestampParts(unit intervalInputUnit, days, micros int64) (int64, int64, error) {
	year, month, day := dateCalendar(days)
	switch unit {
	case intervalInputWeek:
		days -= ((days+dateEpochJulian)%7 + 7) % 7
		micros = 0
	case intervalInputMillennium, intervalInputCentury, intervalInputDecade, intervalInputYear, intervalFieldQuarter, intervalInputMonth:
		switch unit {
		case intervalInputMillennium:
			if year > 0 {
				year = (year+999)/1000*1000 - 999
			} else {
				year = -(1000-year)/1000*1000 + 1
			}
		case intervalInputCentury:
			if year > 0 {
				year = (year+99)/100*100 - 99
			} else {
				year = -(100-year)/100*100 + 1
			}
		case intervalInputDecade:
			if year > 0 {
				year = year / 10 * 10
			} else {
				year = -(9 - year) / 10 * 10
			}
		}
		if unit == intervalFieldQuarter {
			month = (month-1)/3*3 + 1
		} else if unit != intervalInputMonth {
			month = 1
		}
		day = 1
		if !validTimestampCalendar(year, month) {
			return 0, 0, sqlError("22008", "timestamp out of range")
		}
		days = dateFieldJulianDay(year, month, day) - dateEpochJulian
		micros = 0
	case intervalInputDay:
		micros = 0
	case intervalInputHour:
		micros = micros / intervalMicrosPerHour * intervalMicrosPerHour
	case intervalInputMinute:
		micros = micros / intervalMicrosPerMinute * intervalMicrosPerMinute
	case intervalInputSecond:
		micros = micros / intervalMicrosPerSecond * intervalMicrosPerSecond
	case intervalInputMillisecond:
		micros = micros / 1000 * 1000
	}
	return days, micros, nil
}
