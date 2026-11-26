package builtin

import (
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// IntervalStyle selects PostgreSQL's independent interval input/output policy.
// The zero value is the PostgreSQL default.
type IntervalStyle uint8

const (
	IntervalPostgres IntervalStyle = iota
	IntervalPostgresVerbose
	IntervalSQLStandard
	IntervalISO8601
)

const (
	intervalMicrosPerSecond int64 = 1_000_000
	intervalMicrosPerMinute       = 60 * intervalMicrosPerSecond
	intervalMicrosPerHour         = 60 * intervalMicrosPerMinute
	intervalMicrosPerDay          = 24 * intervalMicrosPerHour
	intervalDaysPerMonth    int64 = 30
	intervalYearMask        int32 = 1 << 2
	intervalMonthMask       int32 = 1 << 1
	intervalDayMask         int32 = 1 << 3
	intervalHourMask        int32 = 1 << 10
	intervalMinuteMask      int32 = 1 << 11
	intervalSecondMask      int32 = 1 << 12
	intervalFullRange       int32 = 0x7fff
	intervalFullPrecision   int32 = 0xffff
)

func (style IntervalStyle) valid() bool { return style <= IntervalISO8601 }

func intervalInfinity(value model.Interval) int {
	if value.Months == math.MinInt32 && value.Days == math.MinInt32 && value.Microseconds == math.MinInt64 {
		return -1
	}
	if value.Months == math.MaxInt32 && value.Days == math.MaxInt32 && value.Microseconds == math.MaxInt64 {
		return 1
	}
	return 0
}

func intervalInfinityValue(sign int) model.Interval {
	if sign < 0 {
		return model.Interval{Months: math.MinInt32, Days: math.MinInt32, Microseconds: math.MinInt64}
	}
	return model.Interval{Months: math.MaxInt32, Days: math.MaxInt32, Microseconds: math.MaxInt64}
}

func intervalValue(value model.Interval) model.Value {
	return model.Value{Type: model.TypeInterval, IntervalVal: value}
}
