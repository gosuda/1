package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// TimestampSeries owns fixed-size timestamp-without-time-zone iteration state.
// Timestamps are SQL microseconds from 2000; the zero value is exhausted.
// Copies iterate independently. Calls on one instance require serialization.
type TimestampSeries struct {
	current, stop int64
	step          model.Interval
	direction     int
}

func NewTimestampSeries(start, stop int64, step model.Interval) (TimestampSeries, error) {
	direction := compareInterval(step, model.Interval{})
	if direction == 0 {
		return TimestampSeries{}, sqlError("22023", "step size cannot equal zero")
	}
	if intervalInfinity(step) != 0 {
		return TimestampSeries{}, sqlError("22023", "step size cannot be infinite")
	}
	return TimestampSeries{current: start, stop: stop, step: step, direction: direction}, nil
}

// Next computes the following timestamp before exposing the current value,
// preserving PostgreSQL's overflow at the final step. It allocates no buffers.
func (series *TimestampSeries) Next(ctx context.Context) (model.Value, bool, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, false, err
	}
	pastEnd := series.direction > 0 && series.current > series.stop
	pastStart := series.direction < 0 && series.current < series.stop
	if series.direction == 0 || pastEnd || pastStart {
		series.direction = 0
		return model.Value{}, false, nil
	}
	next, err := timestampAddInterval(series.current, series.step)
	if err != nil {
		return model.Value{}, false, err
	}
	value := timestampValue(series.current)
	series.current = next.IntVal
	return value, true, nil
}
