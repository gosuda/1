package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func timestampAge(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
	left, right := args[0].IntVal, args[1].IntVal
	if !finiteTimestamp(left) || !finiteTimestamp(right) {
		return timestampDifference(ctx, args, budget)
	}
	leftDays, leftMicros := splitTimestamp(left)
	rightDays, rightMicros := splitTimestamp(right)
	if leftDays < -dateEpochJulian || rightDays < -dateEpochJulian {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	return timestampAgeParts(leftDays, leftMicros, rightDays, rightMicros, left < right)
}

// negative follows source timestamp order, which can oppose local clock order
// during a timezone fold.
func timestampAgeParts(leftDays, leftMicros, rightDays, rightMicros int64, negative bool) (model.Value, error) {
	if negative {
		leftDays, rightDays = rightDays, leftDays
		leftMicros, rightMicros = rightMicros, leftMicros
	}
	laterYear, laterMonth, laterDay := dateCalendar(leftDays)
	earlierYear, earlierMonth, earlierDay := dateCalendar(rightDays)
	months := (laterYear-earlierYear)*12 + laterMonth - earlierMonth
	days := laterDay - earlierDay
	micros := leftMicros - rightMicros
	if micros < 0 {
		micros += intervalMicrosPerDay
		days--
	}
	// PostgreSQL borrows from the earlier operand's month, not the month
	// preceding the later operand. Unequal month lengths make this observable.
	for days < 0 {
		days += timestampMonthDays(earlierYear, earlierMonth)
		months--
	}
	if negative {
		months, days, micros = -months, -days, -micros
	}
	if months < math.MinInt32 || months > math.MaxInt32 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	return intervalValue(model.Interval{Months: int32(months), Days: int32(days), Microseconds: micros}), nil
}
