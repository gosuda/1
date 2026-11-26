package builtin

import (
	"context"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// The longest verbose output has 1 prefix byte, field widths 16+8+17+18+9+16,
// and 4 bytes for " ago". The first field and month cannot have minus signs.
const intervalOutputMaxBytes = 89

func compileIntervalOutput(signature Signature, options ResolveOptions) scalarKernel {
	if signature.ID != 1161 || !options.IntervalStyle.valid() {
		return nil
	}
	style := options.IntervalStyle
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		var scratch [intervalOutputMaxBytes]byte
		output := appendIntervalOutput(scratch[:0], args[0].IntervalVal, style)
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if err := outputBudget(int64(len(output)), budget); err != nil {
			return model.Value{}, err
		}
		return model.StringVal(string(output)), nil
	}
}

type intervalOutputFields struct {
	year, month, day, hour, minute, second, microsecond int64
}

func appendIntervalOutput(output []byte, value model.Interval, style IntervalStyle) []byte {
	switch intervalInfinity(value) {
	case -1:
		return append(output, "-infinity"...)
	case 1:
		return append(output, "infinity"...)
	}
	// Divide the time before taking magnitudes, and widen days before negation.
	// Every decomposed component is strictly greater than MinInt64.
	fields := intervalOutputFields{
		year:        int64(value.Months) / 12,
		month:       int64(value.Months) % 12,
		day:         int64(value.Days),
		hour:        value.Microseconds / intervalMicrosPerHour,
		minute:      value.Microseconds % intervalMicrosPerHour / intervalMicrosPerMinute,
		second:      value.Microseconds % intervalMicrosPerMinute / intervalMicrosPerSecond,
		microsecond: value.Microseconds % intervalMicrosPerSecond,
	}
	switch style {
	case IntervalSQLStandard:
		return appendIntervalSQLStandard(output, fields)
	case IntervalISO8601:
		return appendIntervalISO8601(output, fields)
	case IntervalPostgresVerbose:
		return appendIntervalVerbose(output, fields)
	default:
		return appendIntervalPostgres(output, fields)
	}
}

func appendIntervalOutputMagnitude(output []byte, value int64, pad bool) []byte {
	magnitude := uint64(value)
	if value < 0 {
		magnitude = -magnitude
	}
	if pad && magnitude < 10 {
		output = append(output, '0')
	}
	return strconv.AppendUint(output, magnitude, 10)
}

func appendIntervalOutputSeconds(output []byte, second, microsecond int64, pad bool) []byte {
	output = appendIntervalOutputMagnitude(output, second, pad)
	if microsecond == 0 {
		return output
	}
	if microsecond < 0 {
		microsecond = -microsecond
	}
	output = append(output, '.')
	for divisor := int64(100_000); divisor > 0; divisor /= 10 {
		output = append(output, byte('0'+microsecond/divisor))
		microsecond %= divisor
		if microsecond == 0 {
			break
		}
	}
	return output
}

func appendIntervalOutputClock(output []byte, fields intervalOutputFields, padHours bool) []byte {
	output = appendIntervalOutputMagnitude(output, fields.hour, padHours)
	output = append(output, ':')
	output = appendIntervalOutputMagnitude(output, fields.minute, true)
	output = append(output, ':')
	return appendIntervalOutputSeconds(output, fields.second, fields.microsecond, true)
}

func appendIntervalSQLStandard(output []byte, fields intervalOutputFields) []byte {
	negative := fields.year < 0 || fields.month < 0 || fields.day < 0 || fields.hour < 0 || fields.minute < 0 || fields.second < 0 || fields.microsecond < 0
	positive := fields.year > 0 || fields.month > 0 || fields.day > 0 || fields.hour > 0 || fields.minute > 0 || fields.second > 0 || fields.microsecond > 0
	if !negative && !positive {
		return append(output, '0')
	}
	hasYearMonth := fields.year != 0 || fields.month != 0
	hasTime := fields.hour != 0 || fields.minute != 0 || fields.second != 0 || fields.microsecond != 0
	standard := !(negative && positive) && !(hasYearMonth && (fields.day != 0 || hasTime))
	if !standard {
		output = appendIntervalOutputSign(output, fields.year < 0 || fields.month < 0)
		output = appendIntervalOutputMagnitude(output, fields.year, false)
		output = append(output, '-')
		output = appendIntervalOutputMagnitude(output, fields.month, false)
		output = append(output, ' ')
		output = appendIntervalOutputSign(output, fields.day < 0)
		output = appendIntervalOutputMagnitude(output, fields.day, false)
		output = append(output, ' ')
		output = appendIntervalOutputSign(output, fields.hour < 0 || fields.minute < 0 || fields.second < 0 || fields.microsecond < 0)
		return appendIntervalOutputClock(output, fields, false)
	}
	if negative {
		output = append(output, '-')
	}
	if hasYearMonth {
		output = appendIntervalOutputMagnitude(output, fields.year, false)
		output = append(output, '-')
		return appendIntervalOutputMagnitude(output, fields.month, false)
	}
	if fields.day != 0 {
		output = appendIntervalOutputMagnitude(output, fields.day, false)
		output = append(output, ' ')
	}
	return appendIntervalOutputClock(output, fields, false)
}

func appendIntervalOutputSign(output []byte, negative bool) []byte {
	if negative {
		return append(output, '-')
	}
	return append(output, '+')
}

func appendIntervalISO8601(output []byte, fields intervalOutputFields) []byte {
	if fields == (intervalOutputFields{}) {
		return append(output, "PT0S"...)
	}
	output = append(output, 'P')
	output = appendIntervalISO8601Part(output, fields.year, 'Y')
	output = appendIntervalISO8601Part(output, fields.month, 'M')
	output = appendIntervalISO8601Part(output, fields.day, 'D')
	if fields.hour != 0 || fields.minute != 0 || fields.second != 0 || fields.microsecond != 0 {
		output = append(output, 'T')
	}
	output = appendIntervalISO8601Part(output, fields.hour, 'H')
	output = appendIntervalISO8601Part(output, fields.minute, 'M')
	if fields.second != 0 || fields.microsecond != 0 {
		if fields.second < 0 || fields.microsecond < 0 {
			output = append(output, '-')
		}
		output = appendIntervalOutputSeconds(output, fields.second, fields.microsecond, false)
		output = append(output, 'S')
	}
	return output
}

func appendIntervalISO8601Part(output []byte, value int64, unit byte) []byte {
	if value == 0 {
		return output
	}
	output = strconv.AppendInt(output, value, 10)
	return append(output, unit)
}

func appendIntervalPostgres(output []byte, fields intervalOutputFields) []byte {
	before := false
	output, before = appendIntervalPostgresPart(output, fields.year, "year", before)
	output, before = appendIntervalPostgresPart(output, fields.month, "mon", before)
	output, before = appendIntervalPostgresPart(output, fields.day, "day", before)
	if len(output) == 0 || fields.hour != 0 || fields.minute != 0 || fields.second != 0 || fields.microsecond != 0 {
		if len(output) != 0 {
			output = append(output, ' ')
		}
		if fields.hour < 0 || fields.minute < 0 || fields.second < 0 || fields.microsecond < 0 {
			output = append(output, '-')
		} else if before {
			output = append(output, '+')
		}
		output = appendIntervalOutputClock(output, fields, true)
	}
	return output
}

func appendIntervalPostgresPart(output []byte, value int64, unit string, before bool) ([]byte, bool) {
	if value == 0 {
		return output, before
	}
	if len(output) != 0 {
		output = append(output, ' ')
	}
	if before && value > 0 {
		output = append(output, '+')
	}
	output = strconv.AppendInt(output, value, 10)
	output = append(output, ' ')
	output = append(output, unit...)
	if value != 1 {
		output = append(output, 's')
	}
	return output, value < 0
}

func appendIntervalVerbose(output []byte, fields intervalOutputFields) []byte {
	output = append(output, '@')
	before := false
	output, before = appendIntervalVerbosePart(output, fields.year, "year", before)
	output, before = appendIntervalVerbosePart(output, fields.month, "mon", before)
	output, before = appendIntervalVerbosePart(output, fields.day, "day", before)
	output, before = appendIntervalVerbosePart(output, fields.hour, "hour", before)
	output, before = appendIntervalVerbosePart(output, fields.minute, "min", before)
	if fields.second != 0 || fields.microsecond != 0 {
		negative := fields.second < 0 || fields.microsecond < 0
		first := len(output) == 1
		output = append(output, ' ')
		if first {
			before = negative
		} else if negative != before {
			output = append(output, '-')
		}
		output = appendIntervalOutputSeconds(output, fields.second, fields.microsecond, false)
		output = append(output, " sec"...)
		if (fields.second != 1 && fields.second != -1) || fields.microsecond != 0 {
			output = append(output, 's')
		}
	}
	if len(output) == 1 {
		output = append(output, " 0"...)
	}
	if before {
		output = append(output, " ago"...)
	}
	return output
}

func appendIntervalVerbosePart(output []byte, value int64, unit string, before bool) ([]byte, bool) {
	if value == 0 {
		return output, before
	}
	if len(output) == 1 {
		before = value < 0
	}
	if before {
		value = -value
	}
	output = append(output, ' ')
	output = strconv.AppendInt(output, value, 10)
	output = append(output, ' ')
	output = append(output, unit...)
	if value != 1 {
		output = append(output, 's')
	}
	return output, before
}
