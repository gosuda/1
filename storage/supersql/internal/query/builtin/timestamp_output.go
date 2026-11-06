package builtin

import (
	"context"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Postgres BC output needs 3+1+3+1+2+1+15+1+4+3 bytes; AD years
// need at most six digits and omit the three-byte era suffix.
const timestampOutputMaxBytes = 34

func compileTimestampOutput(signature Signature, options ResolveOptions) scalarKernel {
	if signature.ID != 1313 {
		return nil
	}
	style, order := options.DateStyle, options.DateOrder
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		value := args[0].IntVal
		// timestamp2tm accepts all nonnegative Julian days, including values
		// rounded past timestampEnd; int64 microseconds cannot reach INT_MAX days.
		if finiteTimestamp(value) && value < timestampMinimum {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		var scratch [timestampOutputMaxBytes]byte
		output := scratch[:0]
		switch value {
		case math.MinInt64:
			output = append(output, "-infinity"...)
		case math.MaxInt64:
			output = append(output, "infinity"...)
		default:
			output = appendTimestampOutput(output, value, style, order)
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

func appendTimestampOutput(output []byte, value int64, style DateStyle, order DateOrder) []byte {
	days, micros := splitTimestamp(value)
	output, bc := appendTimestampFields(output, days, micros, style, order)
	if bc {
		output = append(output, " BC"...)
	}
	return output
}

func appendTimestampFields(output []byte, days, micros int64, style DateStyle, order DateOrder) ([]byte, bool) {
	year, month, day := dateCalendar(days)
	bc := year <= 0
	if bc {
		year = 1 - year
	}
	clock := intervalOutputFields{
		hour:        micros / intervalMicrosPerHour,
		minute:      micros % intervalMicrosPerHour / intervalMicrosPerMinute,
		second:      micros % intervalMicrosPerMinute / intervalMicrosPerSecond,
		microsecond: micros % intervalMicrosPerSecond,
	}
	if style == DatePostgres {
		const weekdays = "SunMonTueWedThuFriSat"
		const months = "JanFebMarAprMayJunJulAugSepOctNovDec"
		weekday := (days + dateEpochJulian + 1) % 7
		if weekday < 0 {
			weekday += 7
		}
		output = append(output, weekdays[weekday*3:weekday*3+3]...)
		output = append(output, ' ')
		if order == DateDMY {
			output = appendIntervalOutputMagnitude(output, day, true)
			output = append(output, ' ')
			output = append(output, months[(month-1)*3:month*3]...)
		} else {
			output = append(output, months[(month-1)*3:month*3]...)
			output = append(output, ' ')
			output = appendIntervalOutputMagnitude(output, day, true)
		}
		output = append(output, ' ')
		output = appendIntervalOutputClock(output, clock, true)
		output = append(output, ' ')
		output = appendTimestampOutputYear(output, year)
	} else {
		if style == DateISO {
			output = appendTimestampOutputYear(output, year)
			output = append(output, '-')
			output = appendIntervalOutputMagnitude(output, month, true)
			output = append(output, '-')
			output = appendIntervalOutputMagnitude(output, day, true)
		} else {
			first, second, separator := month, day, byte('/')
			if order == DateDMY || style == DateGerman {
				first, second = day, month
			}
			if style == DateGerman {
				separator = '.'
			}
			output = appendIntervalOutputMagnitude(output, first, true)
			output = append(output, separator)
			output = appendIntervalOutputMagnitude(output, second, true)
			output = append(output, separator)
			output = appendTimestampOutputYear(output, year)
		}
		output = append(output, ' ')
		output = appendIntervalOutputClock(output, clock, true)
	}
	return output, bc
}

func appendTimestampOutputYear(output []byte, year int64) []byte {
	for threshold := int64(10); threshold <= 1000; threshold *= 10 {
		if year < threshold {
			output = append(output, '0')
		}
	}
	return strconv.AppendInt(output, year, 10)
}
