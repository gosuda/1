package builtin

import (
	"math"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// SQL timestamps use microseconds since 2000-01-01, never wall-clock units.
const timestampMinimum int64 = -211813488000000000
const timestampEnd int64 = 9223371331200000000

func finiteTimestamp(value int64) bool { return value != math.MinInt64 && value != math.MaxInt64 }
func validTimestamp(value int64) bool  { return value >= timestampMinimum && value < timestampEnd }
func timestampValue(value int64) model.Value {
	return model.Value{Type: model.TypeTimestamp, IntVal: value}
}

func splitTimestamp(value int64) (days, microseconds int64) {
	days, microseconds = value/intervalMicrosPerDay, value%intervalMicrosPerDay
	if microseconds < 0 {
		days--
		microseconds += intervalMicrosPerDay
	}
	return
}

func composeTimestamp(days, microseconds int64) (int64, error) {
	if days < timestampMinimum/intervalMicrosPerDay || days > timestampEnd/intervalMicrosPerDay {
		return 0, sqlError("22008", "timestamp out of range")
	}
	base := days * intervalMicrosPerDay
	if microseconds > 0 && base > math.MaxInt64-microseconds {
		return 0, sqlError("22008", "timestamp out of range")
	}
	if microseconds < 0 && base < math.MinInt64-microseconds {
		return 0, sqlError("22008", "timestamp out of range")
	}
	result := base + microseconds
	if !validTimestamp(result) {
		return 0, sqlError("22008", "timestamp out of range")
	}
	return result, nil
}

func subtractTimestamp(left, right int64) (int64, bool) {
	result := left - right
	positiveOverflow := right > 0 && result > left
	negativeOverflow := right < 0 && result < left
	return result, !positiveOverflow && !negativeOverflow
}
