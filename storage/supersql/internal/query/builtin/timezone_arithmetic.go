package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimezoneArithmetic(signature Signature, session namedTimeZone) scalarKernel {
	subtract := signature.ID == 1190 || signature.ID == 6223 || signature.ID == 6273
	explicit := signature.ArgumentCount == 3
	return func(ctx context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		zone := session
		if explicit {
			var found bool
			zone, _, found = timezoneLookup(timezoneArgumentName(args[2].StrVal), session)
			if !found {
				return model.Value{}, sqlError("22023", "time zone not recognized")
			}
		}
		value, span := args[0].IntVal, args[1].IntervalVal
		if signature.ID == 2549 {
			value, span = args[1].IntVal, args[0].IntervalVal
		}
		if subtract {
			operand := [1]model.Value{intervalValue(span)}
			negated, err := intervalNegate(ctx, operand[:], CallBudget{})
			if err != nil {
				return model.Value{}, err
			}
			span = negated.IntervalVal
		}
		result, err := zone.addTimestampInterval(value, span)
		if err != nil {
			return model.Value{}, err
		}
		return result, ctx.Err()
	}
}

func (zone namedTimeZone) composeLocalTimestamp(days, micros int64) (int64, error) {
	seconds := days*86400 + timezoneUnixEpoch + micros/intervalMicrosPerSecond
	return composeTimestampOffset(days, micros, zone.localOffset(seconds))
}

func composeTimestampOffset(days, micros int64, offset int32) (int64, error) {
	year, month, _ := dateCalendar(days)
	if !validTimestampCalendar(year, month) {
		return 0, sqlError("22008", "timestamp out of range")
	}
	var local int64
	if !intervalInputMultiplyAdd(&local, days, intervalMicrosPerDay) || !intervalInputAdd(&local, micros) {
		return 0, sqlError("22008", "timestamp out of range")
	}
	result, ok := subtractTimestamp(local, int64(offset)*intervalMicrosPerSecond)
	if !ok || !validTimestamp(result) {
		return 0, sqlError("22008", "timestamp out of range")
	}
	return result, nil
}

func (zone namedTimeZone) addTimestampInterval(value int64, span model.Interval) (model.Value, error) {
	if intervalInfinity(span) != 0 || !finiteTimestamp(value) {
		return timestampAddInterval(value, span)
	}
	if span.Months != 0 {
		if value < timestampMinimum {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		days, micros, _ := zone.localTimestampParts(value)
		shifted, err := shiftTimestampMonths(days, span.Months)
		if err != nil {
			return model.Value{}, err
		}
		value, err = zone.composeLocalTimestamp(shifted, micros)
		if err != nil {
			return model.Value{}, err
		}
	}
	if span.Days != 0 {
		if value < timestampMinimum {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		days, micros, _ := zone.localTimestampParts(value)
		julian := days + dateEpochJulian + int64(span.Days)
		if julian < -1 || julian > math.MaxInt32 {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		var err error
		value, err = zone.composeLocalTimestamp(julian-dateEpochJulian, micros)
		if err != nil {
			return model.Value{}, err
		}
	}
	if !intervalInputAdd(&value, span.Microseconds) || !validTimestamp(value) {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	return timestampValue(value), nil
}
