package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileIntervalArithmetic(signature Signature) scalarKernel {
	switch signature.ID {
	case 3464:
		return makeInterval
	case 1162, 1163, 1164, 1165, 1166, 1167, 1315:
		op := opCmp
		switch signature.ID {
		case 1162:
			op = opEq
		case 1163:
			op = opNe
		case 1164:
			op = opLt
		case 1165:
			op = opLe
		case 1166:
			op = opGe
		case 1167:
			op = opGt
		}
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			return comparisonResult(compareInterval(args[0].IntervalVal, args[1].IntervalVal), op), nil
		}
	case 1168:
		return intervalNegate
	case 1169:
		return intervalAdd
	case 1170:
		return intervalSubtract
	case 1618:
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			return intervalScale(args[0].IntervalVal, args[1].FloatVal, false)
		}
	case 1624:
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			return intervalScale(args[1].IntervalVal, args[0].FloatVal, false)
		}
	case 1326:
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			return intervalScale(args[0].IntervalVal, args[1].FloatVal, true)
		}
	case 1175:
		return intervalJustifyHours
	case 1295:
		return intervalJustifyDays
	case 2711:
		return intervalJustify
	case 1390:
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			return model.BoolVal(intervalInfinity(args[0].IntervalVal) == 0), nil
		}
	default:
		return nil
	}
}

func intervalArithmeticOutOfRange() error {
	return sqlError("22008", "interval out of range")
}

func intervalNegate(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	value := args[0].IntervalVal
	if infinity := intervalInfinity(value); infinity != 0 {
		return intervalValue(intervalInfinityValue(-infinity)), nil
	}
	if value.Months == math.MinInt32 || value.Days == math.MinInt32 || value.Microseconds == math.MinInt64 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result := model.Interval{Months: -value.Months, Days: -value.Days, Microseconds: -value.Microseconds}
	if intervalInfinity(result) != 0 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	return intervalValue(result), nil
}

func intervalAdd(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	left, right := args[0].IntervalVal, args[1].IntervalVal
	a, b := intervalInfinity(left), intervalInfinity(right)
	if a != 0 {
		if b == -a {
			return model.Value{}, intervalArithmeticOutOfRange()
		}
		return intervalValue(left), nil
	}
	if b != 0 {
		return intervalValue(right), nil
	}
	months := int64(left.Months) + int64(right.Months)
	days := int64(left.Days) + int64(right.Days)
	micros := left.Microseconds + right.Microseconds
	monthOverflow := months < math.MinInt32 || months > math.MaxInt32
	dayOverflow := days < math.MinInt32 || days > math.MaxInt32
	positiveOverflow := right.Microseconds > 0 && micros < left.Microseconds
	negativeOverflow := right.Microseconds < 0 && micros > left.Microseconds
	if monthOverflow || dayOverflow || positiveOverflow || negativeOverflow {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result := model.Interval{Months: int32(months), Days: int32(days), Microseconds: micros}
	if intervalInfinity(result) != 0 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	return intervalValue(result), nil
}

func intervalSubtract(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	left, right := args[0].IntervalVal, args[1].IntervalVal
	a, b := intervalInfinity(left), intervalInfinity(right)
	if a != 0 {
		if b == a {
			return model.Value{}, intervalArithmeticOutOfRange()
		}
		return intervalValue(left), nil
	}
	if b != 0 {
		return intervalValue(intervalInfinityValue(-b)), nil
	}
	months := int64(left.Months) - int64(right.Months)
	days := int64(left.Days) - int64(right.Days)
	micros := left.Microseconds - right.Microseconds
	monthOverflow := months < math.MinInt32 || months > math.MaxInt32
	dayOverflow := days < math.MinInt32 || days > math.MaxInt32
	positiveOverflow := right.Microseconds > 0 && micros > left.Microseconds
	negativeOverflow := right.Microseconds < 0 && micros < left.Microseconds
	if monthOverflow || dayOverflow || positiveOverflow || negativeOverflow {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result := model.Interval{Months: int32(months), Days: int32(days), Microseconds: micros}
	if intervalInfinity(result) != 0 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	return intervalValue(result), nil
}

func intervalScale(value model.Interval, factor float64, divide bool) (model.Value, error) {
	if divide && factor == 0 {
		return model.Value{}, sqlError("22012", "division by zero")
	}
	if math.IsNaN(factor) {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	if infinity := intervalInfinity(value); infinity != 0 {
		undefinedDivision := divide && math.IsInf(factor, 0)
		undefinedProduct := !divide && factor == 0
		if undefinedDivision || undefinedProduct {
			return model.Value{}, intervalArithmeticOutOfRange()
		}
		if factor < 0 {
			infinity = -infinity
		}
		return intervalValue(intervalInfinityValue(infinity)), nil
	}
	if !divide && math.IsInf(factor, 0) {
		sign := compareInterval(value, model.Interval{})
		if sign == 0 {
			return model.Value{}, intervalArithmeticOutOfRange()
		}
		if factor < 0 {
			sign = -sign
		}
		return intervalValue(intervalInfinityValue(sign)), nil
	}

	var months, days, micros float64
	if divide {
		months = float64(value.Months) / factor
		days = float64(value.Days) / factor
		micros = float64(value.Microseconds) / factor
	} else {
		months = float64(value.Months) * factor
		days = float64(value.Days) * factor
		micros = float64(value.Microseconds) * factor
	}
	validMonths := months >= -0x1p31 && months < 0x1p31
	validDays := days >= -0x1p31 && days < 0x1p31
	if !validMonths || !validDays {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result := model.Interval{Months: int32(months), Days: int32(days)}
	// The pinned PG18.4 arm64 build contracts each product/remainder expression.
	// Use explicit FMA so Go compiler contraction cannot change the result.
	monthFraction := months - float64(result.Months)
	dayFraction := days - float64(result.Days)
	if !divide {
		monthFraction = math.FMA(float64(value.Months), factor, -float64(result.Months))
		dayFraction = math.FMA(float64(value.Days), factor, -float64(result.Days))
	}
	monthRemainderDays := monthFraction * float64(intervalDaysPerMonth)
	monthRemainderDays = math.RoundToEven(monthRemainderDays*1e6) / 1e6
	seconds := (dayFraction + monthRemainderDays - float64(int32(monthRemainderDays))) * 86400
	seconds = math.RoundToEven(seconds*1e6) / 1e6
	if math.Abs(seconds) >= 86400 {
		wholeDays := int32(seconds / 86400)
		newDays := int64(result.Days) + int64(wholeDays)
		if newDays < math.MinInt32 || newDays > math.MaxInt32 {
			return model.Value{}, intervalArithmeticOutOfRange()
		}
		result.Days = int32(newDays)
		seconds -= float64(wholeDays) * 86400
	}
	newDays := int64(result.Days) + int64(int32(monthRemainderDays))
	if newDays < math.MinInt32 || newDays > math.MaxInt32 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result.Days = int32(newDays)
	if divide {
		micros = math.FMA(seconds, float64(intervalMicrosPerSecond), micros)
	} else {
		micros = math.FMA(float64(value.Microseconds), factor, float64(seconds*float64(intervalMicrosPerSecond)))
	}
	micros = math.RoundToEven(micros)
	if !(micros >= -0x1p63 && micros < 0x1p63) {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result.Microseconds = int64(micros)
	if intervalInfinity(result) != 0 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	return intervalValue(result), nil
}
