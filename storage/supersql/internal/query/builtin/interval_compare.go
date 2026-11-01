package builtin

import (
	"math/bits"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type intervalSpan struct {
	high int64
	low  uint64
}

func intervalComparisonSpan(value model.Interval) intervalSpan {
	days := int64(value.Months)*intervalDaysPerMonth + int64(value.Days)
	high, low := bits.Mul64(uint64(days), uint64(intervalMicrosPerDay))
	// Correct the unsigned product and addition for their signed operands.
	if days < 0 {
		high -= uint64(intervalMicrosPerDay)
	}
	low, carry := bits.Add64(low, uint64(value.Microseconds), 0)
	high, _ = bits.Add64(high, uint64(value.Microseconds>>63), carry)
	return intervalSpan{high: int64(high), low: low}
}

func compareInterval(left, right model.Interval) int {
	a, b := intervalComparisonSpan(left), intervalComparisonSpan(right)
	if a.high < b.high || (a.high == b.high && a.low < b.low) {
		return -1
	}
	if a.high > b.high || (a.high == b.high && a.low > b.low) {
		return 1
	}
	return 0
}
