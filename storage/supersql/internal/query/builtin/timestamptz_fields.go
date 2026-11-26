package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	timestampFieldZone intervalInputUnit = dateFieldJulian + 1 + iota
	timestampFieldZoneHour
	timestampFieldZoneMinute
)

func timestamptzFieldUnit(text string) intervalInputUnit {
	unit := dateFieldUnit(text)
	if unit != intervalInputUnsupported {
		return unit
	}
	word := intervalInputKeyword(text)
	for index, c := range word {
		if c == 0 {
			clear(word[index:])
			break
		}
	}
	switch string(word[:]) {
	case "timezone\x00\x00":
		return timestampFieldZone
	case "timezone_h":
		return timestampFieldZoneHour
	case "timezone_m":
		return timestampFieldZoneMinute
	}
	return unit
}

func compileTimestamptzFields(signature Signature, zone namedTimeZone) scalarKernel {
	numeric := signature.ID == 6203
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		unit := timestamptzFieldUnit(args[0].StrVal)
		switch unit {
		case intervalInputUnknown, intervalInputIgnore, intervalInputAgo:
			return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp with time zone", false)
		case intervalFieldReserved:
			return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp with time zone", true)
		}
		value := args[1].IntVal
		if !finiteTimestamp(value) {
			switch unit {
			case intervalInputMicrosecond, intervalInputMillisecond, intervalInputSecond, intervalInputMinute, intervalInputHour, intervalInputDay, intervalInputMonth, intervalFieldQuarter, intervalInputWeek, dateFieldDOW, dateFieldISODOW, dateFieldDOY, intervalInputUnsupported, timestampFieldZone, timestampFieldZoneHour, timestampFieldZoneMinute:
				storage := model.TypeFloat64
				if numeric {
					storage = model.TypeNumeric
				}
				return model.NullVal(storage), nil
			}
			sign := 1
			if value < 0 {
				sign = -1
			}
			return intervalInfiniteField(intervalFieldEpoch, sign, numeric, budget.OutputBytes)
		}
		if unit == intervalFieldEpoch {
			return timestampEpochField(ctx, value, numeric, budget)
		}
		if value < timestampMinimum {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		if unit == intervalInputUnsupported {
			return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp with time zone", true)
		}
		days, micros, typ := zone.localTimestampParts(value)
		switch unit {
		case timestampFieldZone, timestampFieldZoneHour, timestampFieldZoneMinute:
			result := int64(typ.offset)
			if unit == timestampFieldZoneHour {
				result /= 3600
			} else if unit == timestampFieldZoneMinute {
				result = result / 60 % 60
			}
			if numeric {
				return intervalNumericField(ctx, result, 0, 0, budget.OutputBytes)
			}
			return model.Float64Val(float64(result)), nil
		}
		return timestampCalendarField(ctx, unit, days, micros, numeric, budget)
	}
}
