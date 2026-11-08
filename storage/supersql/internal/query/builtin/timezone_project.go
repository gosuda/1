package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// localTimestampParts requires a finite timestamp. Splitting before applying the
// offset preserves local display fields outside the timestamp input range.
func (zone namedTimeZone) localTimestampParts(value int64) (days, micros int64, typ timezoneType) {
	typ = zone.at(timezoneFloor(value, intervalMicrosPerSecond) + timezoneUnixEpoch)
	days, micros = splitTimestamp(value)
	micros += int64(typ.offset) * intervalMicrosPerSecond
	carry := timezoneFloor(micros, intervalMicrosPerDay)
	days += carry
	micros -= carry * intervalMicrosPerDay
	return
}

func compileTimezoneProjection(signature Signature, zone namedTimeZone) scalarKernel {
	date := signature.ID == 1178
	return func(ctx context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		value := args[0].IntVal
		if !finiteTimestamp(value) {
			if !date {
				return model.NullVal(model.TypeTimestamp), nil
			}
			if value < 0 {
				return dateValue(math.MinInt32), nil
			}
			return dateValue(math.MaxInt32), nil
		}
		if value < timestampMinimum {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		days, micros, _ := zone.localTimestampParts(value)
		if date {
			return dateValue(days), ctx.Err()
		}
		return timeValue(micros), ctx.Err()
	}
}

func compileDateTimezone(zone namedTimeZone) scalarKernel {
	return func(ctx context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		days := args[0].IntVal
		base, err := timestampFromDate(days)
		if err != nil {
			return model.Value{}, err
		}
		if !finiteTimestamp(base) {
			return timestampValue(base), nil
		}
		year, month, day := dateCalendar(days)
		var offset int32
		if validTimestampCalendar(year, month) {
			const unixEpochJulian = dateEpochJulian - timezoneUnixEpoch/86400
			date := int32(dateFieldJulianDay(year, month, day) - unixEpochJulian)
			offset = zone.localOffset(int64(date) * 86400)
		}
		// PostgreSQL uses GMT for an unrepresentable Julian calendar and wraps
		// the DateADT multiplication before checking the resulting timestamp.
		result := base - int64(offset)*intervalMicrosPerSecond
		if !validTimestamp(result) {
			return model.Value{}, sqlError("22008", "date out of range for timestamp")
		}
		return timestampValue(result), ctx.Err()
	}
}
