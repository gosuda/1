package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimestamptzTrunc(signature Signature, session namedTimeZone) scalarKernel {
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
		unit, supported, err := timestampTruncUnit(args[0].StrVal, "timestamp with time zone")
		if err != nil {
			return model.Value{}, err
		}
		value := args[1].IntVal
		if !finiteTimestamp(value) {
			if !supported {
				return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp with time zone", true)
			}
			return timestampValue(value), nil
		}
		if value < timestampMinimum {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		if !supported {
			return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp with time zone", true)
		}
		days, micros, typ := zone.localTimestampParts(value)
		days, micros, err = truncateTimestampParts(unit, days, micros)
		if err != nil {
			return model.Value{}, err
		}
		var result int64
		switch unit {
		case intervalInputHour, intervalInputMinute, intervalInputSecond, intervalInputMillisecond, intervalInputMicrosecond:
			// Retain the input side of a fold instead of resolving the local clock again.
			result, err = composeTimestampOffset(days, micros, typ.offset)
		default:
			result, err = zone.composeLocalTimestamp(days, micros)
		}
		if err != nil {
			return model.Value{}, err
		}
		return timestampValue(result), ctx.Err()
	}
}
