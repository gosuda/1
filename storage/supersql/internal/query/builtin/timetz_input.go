package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func timetzInput(ctx context.Context, text string, precision int64, options ResolveOptions, session namedTimeZone, budget CallBudget) (model.Value, error) {
	fields, err := parseTemporalInputMode(ctx, text, options, true)
	if err != nil {
		return model.Value{}, err
	}
	switch fields.Kind {
	case temporalInputCivil, temporalInputAllBalls:
	case temporalInputNow:
		now, err := temporalTransactionTimestamp(ctx, budget)
		if err != nil {
			return model.Value{}, err
		}
		_, micros, zone := session.localTimestampParts(now)
		if !fields.HasZone {
			return timetzResult(ctx, roundTimePrecision(micros, precision), -zone.offset, budget)
		}
		fields.TimeMicros = micros
		fields.HasTime = true
	default:
		return model.Value{}, temporalBadFormat()
	}
	if !fields.HasTime {
		return model.Value{}, temporalBadFormat()
	}
	var days int64
	if fields.HasDate {
		if !validTimestampCalendar(fields.Year, int64(fields.Month)) {
			return model.Value{}, temporalOverflow()
		}
		days = dateFieldJulianDay(fields.Year, int64(fields.Month), int64(fields.Day)) - dateEpochJulian
	}
	if fields.HasZone {
		offset, status := decodeNumericTimezone(fields.Zone)
		if status == intervalInputOK {
			return timetzResult(ctx, roundTimePrecision(fields.TimeMicros, precision), -int32(offset), budget)
		}
		if status == intervalInputOverflow {
			return model.Value{}, sqlError("22009", "time zone displacement out of range")
		}
		zone, dynamic, ok := timezoneLookup(fields.Zone, session)
		if !ok {
			return model.Value{}, sqlError("22023", "time zone not recognized")
		}
		if !dynamic {
			if offset, fixed := timetzFixedZoneOffset(zone); fixed {
				return timetzResult(ctx, roundTimePrecision(fields.TimeMicros, precision), -offset, budget)
			}
			if !fields.HasDate {
				return model.Value{}, temporalBadFormat()
			}
		}
	}
	if !fields.HasDate {
		now, err := temporalTransactionTimestamp(ctx, budget)
		if err != nil {
			return model.Value{}, err
		}
		days, _, _ = session.localTimestampParts(now)
	}
	offset, err := temporalInputOffset(ctx, fields, session, days)
	if err != nil {
		return model.Value{}, err
	}
	return timetzResult(ctx, roundTimePrecision(fields.TimeMicros, precision), -int32(offset), budget)
}

func timetzFixedZoneOffset(zone namedTimeZone) (int32, bool) {
	if zone.record != nil {
		offset := zone.record.typeAt(0).offset
		for i := uint16(1); i < zone.record.typeCount; i++ {
			if zone.record.typeAt(uint8(i)).offset != offset {
				return 0, false
			}
		}
		return offset, true
	}
	if zone.posix {
		return zone.rule.StandardOffset, !zone.rule.Daylight || zone.rule.StandardOffset == zone.rule.DaylightOffset
	}
	return zone.offset, true
}
