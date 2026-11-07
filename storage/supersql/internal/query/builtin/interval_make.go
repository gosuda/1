package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func makeInterval(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	seconds := args[6].FloatVal
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	months := args[0].IntVal * 12
	if months < math.MinInt32 || months > math.MaxInt32 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	months += args[1].IntVal
	if months < math.MinInt32 || months > math.MaxInt32 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	days := args[2].IntVal * 7
	if days < math.MinInt32 || days > math.MaxInt32 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	days += args[3].IntVal
	if days < math.MinInt32 || days > math.MaxInt32 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	micros := args[4].IntVal*intervalMicrosPerHour + args[5].IntVal*intervalMicrosPerMinute
	scaled := seconds * float64(intervalMicrosPerSecond)
	if math.IsInf(scaled, 0) {
		return model.Value{}, sqlError("22003", "value out of range: overflow")
	}
	scaled = math.RoundToEven(scaled)
	if !(scaled >= -0x1p63 && scaled < 0x1p63) {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	if !intervalInputAdd(&micros, int64(scaled)) {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	result := model.Interval{Months: int32(months), Days: int32(days), Microseconds: micros}
	if intervalInfinity(result) != 0 {
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	return intervalValue(result), nil
}
