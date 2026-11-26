package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const timestamptzOutputMaxBytes = timestampOutputMaxBytes + 11

func compileTimestamptzOutput(zone namedTimeZone, options ResolveOptions) scalarKernel {
	style, order := options.DateStyle, options.DateOrder
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		value := args[0].IntVal
		if finiteTimestamp(value) && value < timestampMinimum {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		var scratch [timestamptzOutputMaxBytes]byte
		output := scratch[:0]
		switch value {
		case math.MinInt64:
			output = append(output, "-infinity"...)
		case math.MaxInt64:
			output = append(output, "infinity"...)
		default:
			days, micros, typ := zone.localTimestampParts(value)
			var bc bool
			output, bc = appendTimestampFields(output, days, micros, style, order)
			if style == DateISO {
				output = appendTimezoneOffset(output, typ.offset)
			} else {
				output = append(output, ' ')
				output = append(output, typ.name[:min(10, len(typ.name))]...)
			}
			if bc {
				output = append(output, " BC"...)
			}
		}
		if err := outputBudget(int64(len(output)), budget.OutputBytes); err != nil {
			return model.Value{}, err
		}
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		return model.StringVal(string(output)), nil
	}
}

func appendTimezoneOffset(output []byte, offset int32) []byte {
	seconds := int64(offset)
	sign := byte('+')
	if seconds < 0 {
		sign = '-'
		seconds = -seconds
	}
	output = append(output, sign)
	output = appendIntervalOutputMagnitude(output, seconds/3600, true)
	if seconds%3600 != 0 {
		output = append(output, ':')
		output = appendIntervalOutputMagnitude(output, seconds/60%60, true)
		if seconds%60 != 0 {
			output = append(output, ':')
			output = appendIntervalOutputMagnitude(output, seconds%60, true)
		}
	}
	return output
}
