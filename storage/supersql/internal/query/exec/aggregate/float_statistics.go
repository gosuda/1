package aggregate

import (
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (op operation) statistical() bool {
	return op >= variancePopulation && op <= stddevSample
}

// PostgreSQL float[48]_accum uses Youngs-Cramer moments, including for AVG.
// Keep explicit product rounding rather than allowing fused multiply-adds.
func advanceFloatStatistics(state *transition, value float64) error {
	previous := state.value.FloatVal
	if state.value.IsNull {
		previous = 0
	}
	n := state.floatCount + 1
	sum := previous + value
	squares := state.squares
	if state.floatCount > 0 {
		difference := float64(value*n) - sum
		squared := float64(difference * difference)
		squares += squared / float64(n*state.floatCount)
		if math.IsInf(sum, 0) || math.IsInf(squares, 0) {
			if !math.IsInf(previous, 0) && !math.IsInf(value, 0) {
				return overflow()
			}
			squares = math.NaN()
		}
	} else if math.IsNaN(value) || math.IsInf(value, 0) {
		squares = math.NaN()
	}
	state.count++
	state.floatCount, state.squares = n, squares
	state.value = model.Float64Val(sum)
	return nil
}

func finishFloatStatistics(op operation, state *transition) model.Value {
	denominator := state.floatCount
	if op == varianceSample || op == stddevSample {
		denominator--
	}
	if denominator <= 0 {
		return model.NullVal(model.TypeFloat64)
	}
	value := state.squares / denominator
	if op == stddevPopulation || op == stddevSample {
		value = math.Sqrt(value)
	}
	return model.Float64Val(value)
}
