package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimetzFields(numeric bool) scalarKernel {
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		unit := timestamptzFieldUnit(args[0].StrVal)
		switch unit {
		case intervalInputUnknown, intervalInputIgnore, intervalInputAgo, intervalFieldReserved:
			return model.Value{}, temporalFieldError(args[0].StrVal, "time with time zone", false)
		}
		micros, west, err := DecodeTimetz(args[1].BytesVal)
		if err != nil {
			return model.Value{}, err
		}
		seconds, fraction := micros/intervalMicrosPerSecond%60, micros%intervalMicrosPerSecond
		integer, remainder, scale := int64(0), int64(0), 0
		switch unit {
		case timestampFieldZone:
			integer = -int64(west)
		case timestampFieldZoneHour:
			integer = -int64(west) / 3600
		case timestampFieldZoneMinute:
			integer = -int64(west) / 60 % 60
		case intervalInputMicrosecond:
			integer = seconds*intervalMicrosPerSecond + fraction
		case intervalInputMillisecond:
			if !numeric {
				return model.Float64Val(float64(seconds)*1000 + float64(fraction)/1000), nil
			}
			integer, remainder, scale = seconds*1000+fraction/1000, fraction%1000, 3
		case intervalInputSecond:
			if !numeric {
				return model.Float64Val(float64(seconds) + float64(fraction)/1e6), nil
			}
			integer, remainder, scale = seconds, fraction, 6
		case intervalInputMinute:
			integer = micros / intervalMicrosPerMinute % 60
		case intervalInputHour:
			integer = micros / intervalMicrosPerHour
		case intervalFieldEpoch:
			if !numeric {
				return model.Float64Val(float64(micros)/1e6 + float64(west)), nil
			}
			epoch := micros + int64(west)*intervalMicrosPerSecond
			integer, remainder, scale = epoch/intervalMicrosPerSecond, epoch%intervalMicrosPerSecond, 6
		default:
			return model.Value{}, temporalFieldError(args[0].StrVal, "time with time zone", true)
		}
		if !numeric {
			return model.Float64Val(float64(integer)), nil
		}
		return intervalNumericField(ctx, integer, remainder, scale, budget.OutputBytes)
	}
}
