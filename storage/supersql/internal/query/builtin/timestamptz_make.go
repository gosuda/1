package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileMakeTimestamptz(signature Signature, session namedTimeZone) scalarKernel {
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		value, err := makeTimestamp(ctx, args, budget)
		if err != nil {
			return model.Value{}, err
		}
		if signature.ID == 3462 {
			return session.convertTimestamp(value.IntVal, false)
		}
		name := timezoneArgumentName(args[6].StrVal)
		if len(name) > 0 && intervalInputDigit(name[0]) {
			return model.Value{}, sqlError("22023", "numeric time zone requires a leading sign")
		}
		offset, status := decodeNumericTimezone(name)
		switch status {
		case intervalInputOverflow:
			return model.Value{}, sqlError("22023", "numeric time zone out of range")
		case intervalInputBadFormat:
			return convertNamedTimezone(ctx, value.IntVal, name, false, session)
		}
		result, ok := subtractTimestamp(value.IntVal, offset*intervalMicrosPerSecond)
		if !ok || !validTimestamp(result) {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		return timestampValue(result), ctx.Err()
	}
}

// DecodeTimezone uses ISO signs for numeric offsets, unlike POSIX zone names.
func decodeNumericTimezone(text string) (int64, intervalInputStatus) {
	if len(text) == 0 || (text[0] != '+' && text[0] != '-') {
		return 0, intervalInputBadFormat
	}
	hours, n, status := timezoneInputInteger(text[1:])
	if status != intervalInputOK {
		return 0, status
	}
	position := n + 1
	var minutes, seconds int64
	if position < len(text) && text[position] == ':' {
		minutes, n, status = timezoneInputInteger(text[position+1:])
		if status != intervalInputOK {
			return 0, status
		}
		position += n + 1
		if position < len(text) && text[position] == ':' {
			seconds, n, status = timezoneInputInteger(text[position+1:])
			if status != intervalInputOK {
				return 0, status
			}
			position += n + 1
		}
	} else if position == len(text) && len(text) > 3 {
		minutes = hours % 100
		hours /= 100
	}
	invalidHour := hours < 0 || hours > 15
	invalidMinute := minutes < 0 || minutes > 59
	invalidSecond := seconds < 0 || seconds > 59
	if invalidHour || invalidMinute || invalidSecond {
		return 0, intervalInputOverflow
	}
	if position != len(text) {
		return 0, intervalInputBadFormat
	}
	offset := hours*3600 + minutes*60 + seconds
	if text[0] == '-' {
		offset = -offset
	}
	return offset, intervalInputOK
}

func timezoneInputInteger(text string) (int64, int, intervalInputStatus) {
	trimmed := strings.TrimLeft(text, " \t\n\v\f\r")
	value, n, status := intervalInputInteger(trimmed, false, 32)
	if n == 0 {
		return value, 0, status
	}
	return value, len(text) - len(trimmed) + n, status
}
