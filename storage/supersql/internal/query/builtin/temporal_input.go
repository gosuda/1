package builtin

import (
	"context"
	"math"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTemporalInput(signature Signature, options ResolveOptions) (scalarKernel, uint64, error) {
	session, borrowed, err := resolveTimeZoneContext(options.TimeZone)
	if err != nil {
		return nil, 0, err
	}
	retained := uint64(unsafe.Sizeof(session))
	if borrowed {
		session.rule.StandardName = cloneTimezoneToken(session.rule.StandardName)
		session.rule.DaylightName = cloneTimezoneToken(session.rule.DaylightName)
		retained += uint64(len(session.rule.StandardName) + len(session.rule.DaylightName))
	}
	options.TimeZone.Name = strings.Clone(options.TimeZone.Name)
	retained += uint64(len(options.TimeZone.Name))
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		fields, err := parseTemporalInputMode(ctx, args[0].StrVal, options, signature.ID == 1143)
		if err != nil {
			return model.Value{}, err
		}
		originalKind := fields.Kind
		if fields.Kind >= temporalInputNow && fields.Kind <= temporalInputTomorrow {
			now, e := temporalTransactionTimestamp(ctx, budget)
			if e != nil {
				return model.Value{}, e
			}
			days, micros, _ := session.localTimestampParts(now)
			if fields.Kind == temporalInputNow {
				fields.TimeMicros = micros
				fields.HasTime = true
			} else if fields.Kind == temporalInputYesterday {
				days--
			} else if fields.Kind == temporalInputTomorrow {
				days++
			}
			y, m, d := dateCalendar(days)
			fields.Year = y
			fields.Month = int(m)
			fields.Day = int(d)
			fields.HasDate = true
			if originalKind == temporalInputNow && signature.ID == 1150 {
				return temporalInputPrecision(ctx, timestampValue(now), args, budget, false)
			}
		}
		switch signature.ID {
		case 1084:
			switch originalKind {
			case temporalInputPositiveInfinity:
				return dateValue(math.MaxInt32), nil
			case temporalInputNegativeInfinity:
				return dateValue(math.MinInt32), nil
			case temporalInputEpoch:
				return dateValue(-10957), nil
			}
			if !fields.HasDate {
				return model.Value{}, temporalBadFormat()
			}
			if !validTimestampCalendar(fields.Year, int64(fields.Month)) {
				return model.Value{}, sqlError("22008", "date out of range")
			}
			days := dateFieldJulianDay(fields.Year, int64(fields.Month), int64(fields.Day)) - dateEpochJulian
			if !validDate(days) {
				return model.Value{}, sqlError("22008", "date out of range")
			}
			return dateValue(days), nil
		case 1143:
			if !fields.HasTime || originalKind >= temporalInputEpoch && originalKind != temporalInputAllBalls {
				return model.Value{}, temporalBadFormat()
			}
			if err := validateTemporalTimeZone(ctx, fields, session, budget); err != nil {
				return model.Value{}, err
			}
			return temporalInputPrecision(ctx, timestampValue(fields.TimeMicros), args, budget, true)
		default:
			value, err := temporalTimestampFields(ctx, fields, originalKind, signature.ID == 1150, session)
			if err != nil {
				return model.Value{}, err
			}
			return temporalInputPrecision(ctx, value, args, budget, false)
		}
	}, retained, nil
}
func temporalInputPrecision(ctx context.Context, value model.Value, args []model.Value, budget CallBudget, timeOnly bool) (model.Value, error) {
	precision := args[2].IntVal
	if timeOnly {
		return timestampValue(roundTimePrecision(value.IntVal, precision)), nil
	}
	values := [2]model.Value{value, model.Int64Val(precision)}
	return timestampScale(ctx, values[:], budget)
}
func temporalInputOffset(ctx context.Context, fields temporalInputFields, session namedTimeZone, days int64) (int64, error) {
	if fields.HasZone {
		offset, status := decodeNumericTimezone(fields.Zone)
		if status == intervalInputOK {
			return offset, nil
		}
		if status == intervalInputOverflow {
			return 0, sqlError("22009", "time zone displacement out of range")
		}
	}
	zone := session
	dynamic := false
	if fields.HasZone {
		var ok bool
		zone, dynamic, ok = timezoneLookup(fields.Zone, session)
		if !ok {
			return 0, sqlError("22023", "time zone not recognized")
		}
	}
	seconds := days*86400 + fields.TimeMicros/intervalMicrosPerSecond + timezoneUnixEpoch
	offset := zone.localOffset(seconds)
	if dynamic {
		abbreviation, found, err := zone.abbreviationOffset(ctx, fields.Zone, seconds-int64(offset))
		if err != nil {
			return 0, err
		}
		if found {
			offset = abbreviation
		}
	}
	return int64(offset), nil
}

func validateTemporalTimeZone(ctx context.Context, fields temporalInputFields, session namedTimeZone, budget CallBudget) error {
	if !fields.HasZone {
		return nil
	}
	if _, status := decodeNumericTimezone(fields.Zone); status == intervalInputOK {
		return nil
	}
	zone, dynamic, ok := timezoneLookup(fields.Zone, session)
	if !ok {
		return sqlError("22023", "time zone not recognized")
	}
	_, fixed := timetzFixedZoneOffset(zone)
	if !dynamic && !fixed && !fields.HasDate {
		return temporalBadFormat()
	}
	if !dynamic || fields.HasDate {
		return nil
	}
	now, err := temporalTransactionTimestamp(ctx, budget)
	if err != nil {
		return err
	}
	days, _, _ := session.localTimestampParts(now)
	_, err = temporalInputOffset(ctx, fields, session, days)
	return err
}

func temporalTimestampFields(ctx context.Context, fields temporalInputFields, kind temporalInputKind, withZone bool, session namedTimeZone) (model.Value, error) {
	switch kind {
	case temporalInputPositiveInfinity:
		return timestampValue(math.MaxInt64), nil
	case temporalInputNegativeInfinity:
		return timestampValue(math.MinInt64), nil
	case temporalInputEpoch:
		return timestampValue(-timezoneUnixEpoch * intervalMicrosPerSecond), nil
	}
	if !fields.HasDate {
		return model.Value{}, temporalBadFormat()
	}
	if !validTimestampCalendar(fields.Year, int64(fields.Month)) {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	days := dateFieldJulianDay(fields.Year, int64(fields.Month), int64(fields.Day)) - dateEpochJulian
	micros := fields.TimeMicros
	if withZone {
		offset, err := temporalInputOffset(ctx, fields, session, days)
		if err != nil {
			return model.Value{}, err
		}
		micros -= offset * intervalMicrosPerSecond
	}
	// Offset application precedes validation of the final timestamp domain.
	carry := timezoneFloor(micros, intervalMicrosPerDay)
	days += carry
	micros -= carry * intervalMicrosPerDay
	stamp, err := composeTimestamp(days, micros)
	if err != nil {
		return model.Value{}, err
	}
	return timestampValue(stamp), nil
}
