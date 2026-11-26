package builtin

import (
	"encoding/binary"
	"math"
	"math/bits"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func appendIntervalKey(key []byte, value model.Interval, descending bool) []byte {
	span := intervalComparisonSpan(value)
	high, low := uint64(span.high)^(uint64(1)<<63), span.low
	if descending {
		high, low = ^high, ^low
	}
	key = binary.BigEndian.AppendUint64(key, high)
	return binary.BigEndian.AppendUint64(key, low)
}

func (d *keyDecoder) interval(mask byte) (model.Interval, error) {
	high, err := d.number(8, mask)
	if err != nil {
		return model.Interval{}, err
	}
	low, err := d.number(8, mask)
	if err != nil {
		return model.Interval{}, err
	}
	span := intervalSpan{high: int64(high ^ (uint64(1) << 63)), low: low}
	high = uint64(span.high)
	if span.high < 0 {
		low, high = ^low+1, ^high
		if low == 0 {
			high++
		}
	}
	if high >= uint64(intervalMicrosPerDay) {
		return model.Interval{}, storage.ErrInvalidEncoding
	}
	days, _ := bits.Div64(high, low, uint64(intervalMicrosPerDay))
	if days > math.MaxInt64 {
		return model.Interval{}, storage.ErrInvalidEncoding
	}
	signedDays := int64(days)
	if span.high < 0 {
		signedDays = -signedDays
	}
	months := min(max(signedDays/intervalDaysPerMonth, math.MinInt32), math.MaxInt32)
	residualDays := min(max(signedDays-months*intervalDaysPerMonth, math.MinInt32), math.MaxInt32)
	value := model.Interval{Months: int32(months), Days: int32(residualDays)}
	calendar := intervalComparisonSpan(value)
	remainder, borrow := bits.Sub64(span.low, calendar.low, 0)
	upper, _ := bits.Sub64(uint64(span.high), uint64(calendar.high), borrow)
	value.Microseconds = int64(remainder)
	if int64(upper) != value.Microseconds>>63 {
		return model.Interval{}, storage.ErrInvalidEncoding
	}
	return value, nil
}
