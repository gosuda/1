package builtin

import (
	"context"
	"strconv"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func temporalTransactionTimestamp(ctx context.Context, budget CallBudget) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if budget.Temporal == nil {
		return 0, sqlError("55000", "SQL temporal runtime is unavailable")
	}
	value, err := budget.Temporal.TransactionTimestamp(ctx)
	if err != nil {
		return 0, err
	}
	if !validTimestamp(int64(value)) {
		return 0, sqlError("22008", "SQL clock timestamp out of range")
	}
	return int64(value), ctx.Err()
}
func compileTemporalClock(signature Signature, options ResolveOptions) (scalarKernel, uint64, error) {
	var session namedTimeZone
	retained := uint64(0)
	if signature.ID == 274 {
		var borrowed bool
		var err error
		session, borrowed, err = resolveTimeZoneContext(options.TimeZone)
		if err != nil {
			return nil, 0, err
		}
		retained = uint64(unsafe.Sizeof(session))
		if borrowed {
			session.rule.StandardName = cloneTimezoneToken(session.rule.StandardName)
			session.rule.DaylightName = cloneTimezoneToken(session.rule.DaylightName)
			retained += uint64(len(session.rule.StandardName) + len(session.rule.DaylightName))
		}
	}
	return func(ctx context.Context, _ []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if budget.Temporal == nil {
			return model.Value{}, sqlError("55000", "SQL temporal runtime is unavailable")
		}
		var stamp model.SQLTimestamp
		var err error
		switch signature.ID {
		case 1299, 2647:
			stamp, err = budget.Temporal.TransactionTimestamp(ctx)
		case 2648:
			stamp, err = budget.Temporal.StatementTimestamp(ctx)
		default:
			stamp, err = budget.Temporal.ClockTimestamp(ctx)
		}
		if err != nil {
			return model.Value{}, err
		}
		if !validTimestamp(int64(stamp)) {
			return model.Value{}, sqlError("22008", "SQL clock timestamp out of range")
		}
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if signature.ID != 274 {
			return timestampValue(int64(stamp)), nil
		}
		days, micros, typ := session.localTimestampParts(int64(stamp))
		year, month, day := dateCalendar(days)
		const weekdays = "SunMonTueWedThuFriSat"
		const months = "JanFebMarAprMayJunJulAugSepOctNovDec"
		weekday := (days + dateEpochJulian + 1) % 7
		if weekday < 0 {
			weekday += 7
		}
		var scratch [320]byte
		output := scratch[:0]
		output = append(output, weekdays[weekday*3:weekday*3+3]...)
		output = append(output, ' ')
		output = append(output, months[(month-1)*3:month*3]...)
		output = append(output, ' ')
		output = appendIntervalOutputMagnitude(output, day, true)
		output = append(output, ' ')
		output = appendIntervalOutputMagnitude(output, micros/intervalMicrosPerHour, true)
		output = append(output, ':')
		output = appendIntervalOutputMagnitude(output, micros%intervalMicrosPerHour/intervalMicrosPerMinute, true)
		output = append(output, ':')
		output = appendIntervalOutputMagnitude(output, micros%intervalMicrosPerMinute/intervalMicrosPerSecond, true)
		output = append(output, '.')
		fraction := micros % intervalMicrosPerSecond
		for divisor := int64(100000); divisor > 0; divisor /= 10 {
			output = append(output, byte('0'+fraction/divisor))
			fraction %= divisor
		}
		output = append(output, ' ')
		if year >= 0 {
			for width := int64(1000); width > year && width > 1; width /= 10 {
				output = append(output, '0')
			}
		}
		output = strconv.AppendInt(output, year, 10)
		output = append(output, ' ')
		output = append(output, typ.name...)
		if err := outputBudget(int64(len(output)), budget.OutputBytes); err != nil {
			return model.Value{}, err
		}
		if uint64(len(output)) > budget.MemoryBytes {
			return model.Value{}, sqlError("54000", "builtin invocation memory budget exceeded")
		}
		if budget.Allocator != nil {
			claim, err := budget.Allocator.Reserve(uint64(len(output)))
			if err != nil {
				return model.Value{}, err
			}
			defer claim.Release()
		}
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		return model.StringVal(string(output)), nil
	}, retained, nil
}
