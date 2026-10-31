package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// IntervalAggregate retains finite sums separately from infinite inputs. Its zero
// value is empty; callers serialize access and omit SQL NULL inputs.
// It owns no buffers and supports reset by assigning the zero value.
type IntervalAggregate struct {
	sum        model.Interval
	count      int64
	infinities uint8
}

func (state *IntervalAggregate) Advance(value model.Interval) error {
	switch intervalInfinity(value) {
	case -1:
		state.infinities |= 1
		return nil
	case 1:
		state.infinities |= 2
		return nil
	}
	if state.count == math.MaxInt64 {
		return sqlError("22003", "aggregate count out of range")
	}
	args := [2]model.Value{intervalValue(state.sum), intervalValue(value)}
	result, err := intervalAdd(context.Background(), args[:], CallBudget{})
	if err != nil {
		return err
	}
	state.sum = result.IntervalVal
	state.count++
	return nil
}

func (state *IntervalAggregate) Sum() (model.Value, error) {
	switch state.infinities {
	case 1:
		return intervalValue(intervalInfinityValue(-1)), nil
	case 2:
		return intervalValue(intervalInfinityValue(1)), nil
	case 3:
		return model.Value{}, intervalArithmeticOutOfRange()
	}
	if state.count == 0 {
		return model.NullVal(model.TypeInterval), nil
	}
	return intervalValue(state.sum), nil
}

func (state *IntervalAggregate) Average() (model.Value, error) {
	result, err := state.Sum()
	if err != nil || result.IsNull || state.infinities != 0 {
		return result, err
	}
	return intervalScale(state.sum, float64(state.count), true)
}
