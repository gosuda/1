package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileSessionTimezone(signature Signature, zone namedTimeZone) scalarKernel {
	toLocal := signature.ID == 2027 || signature.ID == 6334
	return func(ctx context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		result, err := zone.convertTimestamp(args[0].IntVal, toLocal)
		if err != nil {
			return model.Value{}, err
		}
		return result, ctx.Err()
	}
}

func (zone namedTimeZone) convertTimestamp(value int64, toLocal bool) (model.Value, error) {
	if !finiteTimestamp(value) {
		return timestampValue(value), nil
	}
	seconds := timezoneFloor(value, intervalMicrosPerSecond) + timezoneUnixEpoch
	var offset int32
	if toLocal {
		offset = zone.at(seconds).offset
	} else {
		offset = zone.localOffset(seconds)
	}
	adjustment := int64(offset) * intervalMicrosPerSecond
	if toLocal {
		adjustment = -adjustment
	}
	result, ok := subtractTimestamp(value, adjustment)
	if !ok || !validTimestamp(result) {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	return timestampValue(result), nil
}

func compileDateTimeTimezone(zone namedTimeZone) scalarKernel {
	return func(ctx context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		value, err := timestampFromDate(args[0].IntVal)
		if err != nil {
			return model.Value{}, err
		}
		value, err = timestampAddClock(value, args[1].IntVal)
		if err != nil {
			return model.Value{}, err
		}
		result, err := zone.convertTimestamp(value, false)
		if err != nil {
			return model.Value{}, err
		}
		return result, ctx.Err()
	}
}
