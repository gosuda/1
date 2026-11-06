package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func intervalJustifyHours(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	result := args[0].IntervalVal
	if intervalInfinity(result) != 0 {
		return intervalValue(result), nil
	}
	wholeDays := result.Microseconds / intervalMicrosPerDay
	result.Microseconds %= intervalMicrosPerDay
	days := int64(result.Days) + wholeDays
	if days < math.MinInt32 || days > math.MaxInt32 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result.Days = int32(days)
	if result.Days > 0 && result.Microseconds < 0 {
		result.Microseconds += intervalMicrosPerDay
		result.Days--
	} else if result.Days < 0 && result.Microseconds > 0 {
		result.Microseconds -= intervalMicrosPerDay
		result.Days++
	}
	return intervalValue(result), nil
}

func intervalJustifyDays(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	result := args[0].IntervalVal
	if intervalInfinity(result) != 0 {
		return intervalValue(result), nil
	}
	wholeMonths := int64(result.Days) / intervalDaysPerMonth
	result.Days = int32(int64(result.Days) % intervalDaysPerMonth)
	months := int64(result.Months) + wholeMonths
	if months < math.MinInt32 || months > math.MaxInt32 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result.Months = int32(months)
	if result.Months > 0 && result.Days < 0 {
		result.Days += int32(intervalDaysPerMonth)
		result.Months--
	} else if result.Months < 0 && result.Days > 0 {
		result.Days -= int32(intervalDaysPerMonth)
		result.Months++
	}
	return intervalValue(result), nil
}

func intervalJustify(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	result := args[0].IntervalVal
	if intervalInfinity(result) != 0 {
		return intervalValue(result), nil
	}
	// Reduce same-sign days before adding whole days from time, as PostgreSQL does.
	positiveDayTime := result.Days > 0 && result.Microseconds > 0
	negativeDayTime := result.Days < 0 && result.Microseconds < 0
	if positiveDayTime || negativeDayTime {
		wholeMonths := int64(result.Days) / intervalDaysPerMonth
		result.Days = int32(int64(result.Days) % intervalDaysPerMonth)
		months := int64(result.Months) + wholeMonths
		if months < math.MinInt32 || months > math.MaxInt32 {
			return model.Value{}, intervalArithmeticOutOfRange()
		}
		result.Months = int32(months)
	}
	wholeDays := result.Microseconds / intervalMicrosPerDay
	result.Microseconds %= intervalMicrosPerDay
	days := int64(result.Days) + wholeDays
	months := int64(result.Months) + days/intervalDaysPerMonth
	if months < math.MinInt32 || months > math.MaxInt32 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result.Months = int32(months)
	result.Days = int32(days % intervalDaysPerMonth)
	negativeDayTime = result.Days < 0 || (result.Days == 0 && result.Microseconds < 0)
	positiveDayTime = result.Days > 0 || (result.Days == 0 && result.Microseconds > 0)
	if result.Months > 0 && negativeDayTime {
		result.Days += int32(intervalDaysPerMonth)
		result.Months--
	} else if result.Months < 0 && positiveDayTime {
		result.Days -= int32(intervalDaysPerMonth)
		result.Months++
	}
	if result.Days > 0 && result.Microseconds < 0 {
		result.Microseconds += intervalMicrosPerDay
		result.Days--
	} else if result.Days < 0 && result.Microseconds > 0 {
		result.Microseconds -= intervalMicrosPerDay
		result.Days++
	}
	return intervalValue(result), nil
}
