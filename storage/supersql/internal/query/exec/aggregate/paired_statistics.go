package aggregate

import (
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type pairedMoments struct {
	n, sx, sxx, sy, syy, sxy float64
}

func (op operation) pairedMoments() bool {
	return op >= regrSxx && op <= correlation
}

// Match PostgreSQL float8_regr_accum, including intermediate rounding and
// overflow checks on all moments even when a finalizer uses only one of them.
func advancePairedStatistics(state *transition, y, x float64) error {
	previous := state.paired
	next := *previous
	next.n++
	next.sx += x
	next.sy += y
	if previous.n > 0 {
		tmpX := float64(x*next.n) - next.sx
		tmpY := float64(y*next.n) - next.sy
		scale := 1.0 / float64(next.n*previous.n)
		next.sxx += float64(float64(tmpX*tmpX) * scale)
		next.syy += float64(float64(tmpY*tmpY) * scale)
		next.sxy += float64(float64(tmpX*tmpY) * scale)
		if math.IsInf(next.sx, 0) || math.IsInf(next.sxx, 0) || math.IsInf(next.sy, 0) || math.IsInf(next.syy, 0) || math.IsInf(next.sxy, 0) {
			xOverflow := (math.IsInf(next.sx, 0) || math.IsInf(next.sxx, 0)) && !math.IsInf(previous.sx, 0) && !math.IsInf(x, 0)
			yOverflow := (math.IsInf(next.sy, 0) || math.IsInf(next.syy, 0)) && !math.IsInf(previous.sy, 0) && !math.IsInf(y, 0)
			xyOverflow := math.IsInf(next.sxy, 0) && !math.IsInf(previous.sx, 0) && !math.IsInf(x, 0) && !math.IsInf(previous.sy, 0) && !math.IsInf(y, 0)
			if xOverflow || yOverflow || xyOverflow {
				return overflow()
			}
			if math.IsInf(next.sxx, 0) {
				next.sxx = math.NaN()
			}
			if math.IsInf(next.syy, 0) {
				next.syy = math.NaN()
			}
			if math.IsInf(next.sxy, 0) {
				next.sxy = math.NaN()
			}
		}
	} else {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			next.sxx, next.sxy = math.NaN(), math.NaN()
		}
		if math.IsNaN(y) || math.IsInf(y, 0) {
			next.syy, next.sxy = math.NaN(), math.NaN()
		}
	}
	*state.paired = next
	state.count++
	return nil
}

func finishPairedStatistics(op operation, state *pairedMoments) model.Value {
	if state.n < 1 {
		return model.NullVal(model.TypeFloat64)
	}
	var value float64
	switch op {
	case regrSxx:
		value = state.sxx
	case regrSyy:
		value = state.syy
	case regrSxy:
		value = state.sxy
	case regrAvgx:
		value = state.sx / state.n
	case regrAvgy:
		value = state.sy / state.n
	case covarPopulation:
		value = state.sxy / state.n
	case covarSample:
		if state.n < 2 {
			return model.NullVal(model.TypeFloat64)
		}
		value = state.sxy / (state.n - 1)
	case correlation:
		if state.sxx == 0 || state.syy == 0 {
			return model.NullVal(model.TypeFloat64)
		}
		value = state.sxy / math.Sqrt(float64(state.sxx*state.syy))
	case regrR2:
		if state.sxx == 0 {
			return model.NullVal(model.TypeFloat64)
		}
		if state.syy == 0 {
			return model.Float64Val(1)
		}
		value = float64(state.sxy*state.sxy) / float64(state.sxx*state.syy)
	case regrSlope:
		if state.sxx == 0 {
			return model.NullVal(model.TypeFloat64)
		}
		value = state.sxy / state.sxx
	case regrIntercept:
		if state.sxx == 0 {
			return model.NullVal(model.TypeFloat64)
		}
		value = (state.sy - float64(float64(state.sx*state.sxy)/state.sxx)) / state.n
	}
	return model.Float64Val(value)
}
