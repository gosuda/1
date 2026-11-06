package builtin

import (
	"context"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimestampBin(signature Signature) scalarKernel {
	if signature.ID != 6177 && signature.ID != 6178 {
		return nil
	}
	return func(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		stride, value, origin := args[0].IntervalVal, args[1].IntVal, args[2].IntVal
		if !finiteTimestamp(value) {
			return timestampValue(value), nil
		}
		if !finiteTimestamp(origin) {
			return model.Value{}, sqlError("22008", "origin out of range")
		}
		if intervalInfinity(stride) != 0 {
			return model.Value{}, sqlError("22008", "timestamps cannot be binned into infinite intervals")
		}
		if stride.Months != 0 {
			return model.Value{}, sqlError("0A000", "timestamps cannot be binned into intervals containing months or years")
		}
		duration := int64(0)
		if !intervalInputMultiplyAdd(&duration, int64(stride.Days), intervalMicrosPerDay) || !intervalInputAdd(&duration, stride.Microseconds) {
			return model.Value{}, intervalArithmeticOutOfRange()
		}
		if duration <= 0 {
			return model.Value{}, sqlError("22008", "stride must be greater than zero")
		}
		difference, ok := subtractTimestamp(value, origin)
		if !ok {
			return model.Value{}, intervalArithmeticOutOfRange()
		}
		remainder := difference % duration
		result := origin + (difference - remainder)
		if remainder < 0 {
			result, ok = subtractTimestamp(result, duration)
			if !ok || !validTimestamp(result) {
				return model.Value{}, sqlError("22008", "timestamp out of range")
			}
		}
		return timestampValue(result), nil
	}
}
