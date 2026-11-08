package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimestamptzAge(zone namedTimeZone) scalarKernel {
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		left, right := args[0].IntVal, args[1].IntVal
		if !finiteTimestamp(left) || !finiteTimestamp(right) {
			return timestampDifference(ctx, args, budget)
		}
		if left < timestampMinimum || right < timestampMinimum {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		leftDays, leftMicros, _ := zone.localTimestampParts(left)
		rightDays, rightMicros, _ := zone.localTimestampParts(right)
		return timestampAgeParts(leftDays, leftMicros, rightDays, rightMicros, left < right)
	}
}
