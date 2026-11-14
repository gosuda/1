package aggregate

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func numericStatisticsBytes(maxRecord uint64) uint64 {
	return maxRecord*8 + uint64(unsafe.Sizeof(builtin.NumericMoments{}))
}

func (c *cursor) advanceNumericStatistics(ctx context.Context, state *transition, value model.Value) error {
	moments := state.moments
	if moments == nil {
		moments = new(builtin.NumericMoments)
	}
	if err := moments.Advance(ctx, value, numericStatisticsBytes(c.options.MaxRecordBytes)); err != nil {
		return err
	}
	state.moments = moments
	state.count++
	return nil
}

func (c *cursor) finishStatistics(ctx context.Context, call compiledCall, state *transition) (model.Value, error) {
	if call.output.ID != 1700 {
		return finishFloatStatistics(call.op, state), nil
	}
	if state.moments == nil {
		return model.NullVal(model.TypeNumeric), nil
	}
	var statistic builtin.NumericStatistic
	switch call.op {
	case variancePopulation:
		statistic = builtin.NumericVariancePopulation
	case varianceSample:
		statistic = builtin.NumericVarianceSample
	case stddevPopulation:
		statistic = builtin.NumericStddevPopulation
	case stddevSample:
		statistic = builtin.NumericStddevSample
	default:
		return model.Value{}, runtime.ErrInvalidConfig
	}
	return state.moments.Result(ctx, statistic, int64(c.options.MaxRecordBytes))
}

func (g *group) releaseNumeric() error {
	var err error
	for i := range g.states {
		err = errors.Join(err, g.states[i].moments.Close())
		g.states[i].moments = nil
	}
	return err
}
