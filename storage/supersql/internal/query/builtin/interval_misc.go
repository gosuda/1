package builtin

import (
	"context"
	"encoding/binary"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileIntervalExtremum(signature Signature) scalarKernel {
	return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		order := compareInterval(args[0].IntervalVal, args[1].IntervalVal)
		if signature.ID == 1197 && order < 0 {
			return args[0], nil
		}
		if signature.ID == 1198 && order > 0 {
			return args[0], nil
		}
		return args[1], nil
	}
}

func intervalSend(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	if err := outputBudget(16, budget); err != nil {
		return model.Value{}, err
	}
	value := args[0].IntervalVal
	output := make([]byte, 16)
	binary.BigEndian.PutUint64(output, uint64(value.Microseconds))
	binary.BigEndian.PutUint32(output[8:], uint32(value.Days))
	binary.BigEndian.PutUint32(output[12:], uint32(value.Months))
	return model.BytesVal(output), nil
}
