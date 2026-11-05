package builtin

import (
	"context"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimeFields(signature Signature) scalarKernel {
	if signature.ID != 1385 && signature.ID != 6200 {
		return nil
	}
	numeric := signature.ID == 6200
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		unit := dateFieldUnit(args[0].StrVal)
		switch unit {
		case intervalInputUnknown, intervalInputIgnore, intervalInputAgo, intervalFieldReserved:
			return model.Value{}, temporalFieldError(args[0].StrVal, "time without time zone", false)
		}
		value := args[1].IntVal
		if value < 0 || value > intervalMicrosPerDay {
			return model.Value{}, sqlError("22008", "time out of range")
		}
		seconds := value / intervalMicrosPerSecond % 60
		fraction := value % intervalMicrosPerSecond
		integer, remainder, scale := int64(0), int64(0), 0
		switch unit {
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
			integer = value / intervalMicrosPerMinute % 60
		case intervalInputHour:
			integer = value / intervalMicrosPerHour
		case intervalFieldEpoch:
			if !numeric {
				return model.Float64Val(float64(value) / 1e6), nil
			}
			integer, remainder, scale = value/intervalMicrosPerSecond, fraction, 6
		default:
			return model.Value{}, temporalFieldError(args[0].StrVal, "time without time zone", true)
		}
		if !numeric {
			return model.Float64Val(float64(integer)), nil
		}
		return intervalNumericField(ctx, integer, remainder, scale, budget.OutputBytes)
	}
}
