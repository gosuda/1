package builtin

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/decimalint"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type NumericStatistic uint8

const (
	NumericVariancePopulation NumericStatistic = iota
	NumericVarianceSample
	NumericStddevPopulation
	NumericStddevSample
)

// NumericMomentsWorkBytes is an enforced local allocation cap. Aggregate owners
// admit this workspace before invoking moments, independently of retained groups.
const NumericMomentsWorkBytes uint64 = 4 << 20

// NumericMoments owns its coefficient workspace. Its zero value is ready to use;
// it must not be copied after use. Results never borrow coefficients.
type NumericMoments struct {
	count                  int64
	sum, sumSquares        *decimalint.Int
	sumScale, squaresScale int
	nonfinite              bool
	work                   *decimalint.Workspace
}

func (m *NumericMoments) Advance(ctx context.Context, value model.Value, stateBytes uint64) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if value.Type != model.TypeInt64 && value.Type != model.TypeNumeric {
		return sqlError("42804", "numeric statistics require integer or numeric input")
	}
	if err := numericMomentsStateBudget(m.sum, m.sumSquares, m.work != nil, stateBytes); err != nil {
		return err
	}
	if value.IsNull {
		return nil
	}
	if m.count == math.MaxInt64 {
		return numericResourceError()
	}
	created := m.work == nil
	if created {
		m.work, err = decimalint.NewWorkspace(ctx, nil, NumericMomentsWorkBytes)
		if err != nil {
			return numericAllocationError(err)
		}
	}
	mark := m.work.Checkpoint()
	var keepSum, keepSquares *decimalint.Int
	defer func() {
		if err != nil {
			keepSum = nil
			keepSquares = nil
		}
		err = numericAllocationError(errors.Join(err, m.work.ReleaseAfter(mark, keepSum, keepSquares)))
		if err != nil && created {
			err = errors.Join(err, m.work.Close())
			m.work = nil
		}
	}()
	var input decimal
	if value.Type == model.TypeInt64 {
		input.coefficient, err = m.work.FromInt64(ctx, value.IntVal)
	} else {
		input, err = parseNumeric(ctx, m.work, value.StrVal)
	}
	if err != nil {
		return err
	}
	if input.scale > numericMaxScale {
		return numericOverflow()
	}
	if input.special != numericFinite {
		if err := numericMomentsStateBudget(m.sum, m.sumSquares, true, stateBytes); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		m.count++
		m.nonfinite = true
		return nil
	}
	sumInput, squaresInput := m.sum, m.sumSquares
	if sumInput == nil {
		sumInput, err = m.work.FromInt64(ctx, 0)
		if err != nil {
			return err
		}
	}
	if squaresInput == nil {
		squaresInput, err = m.work.FromInt64(ctx, 0)
		if err != nil {
			return err
		}
	}
	square, err := numericMomentsProduct(ctx, m.work, input, input)
	if err != nil {
		return err
	}
	sum, err := numericArithmetic(ctx, m.work, decimal{coefficient: sumInput, scale: m.sumScale}, input, opPl)
	if err != nil {
		return err
	}
	squares, err := numericArithmetic(ctx, m.work, decimal{coefficient: squaresInput, scale: m.squaresScale}, square, opPl)
	if err != nil {
		return err
	}
	if err := numericMomentsStateBudget(sum.coefficient, squares.coefficient, true, stateBytes); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := errors.Join(m.sum.Close(), m.sumSquares.Close()); err != nil {
		return err
	}
	m.sum, m.sumSquares = sum.coefficient, squares.coefficient
	m.sumScale, m.squaresScale = sum.scale, squares.scale
	keepSum, keepSquares = m.sum, m.sumSquares
	m.count++
	return nil
}

func (m *NumericMoments) Result(ctx context.Context, statistic NumericStatistic, outputBytes int64) (result model.Value, err error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if statistic > NumericStddevSample {
		return model.Value{}, sqlError("XX000", "invalid numeric statistic")
	}
	sample := statistic == NumericVarianceSample || statistic == NumericStddevSample
	if m.count == 0 || sample && m.count <= 1 {
		return model.NullVal(model.TypeNumeric), nil
	}
	if m.nonfinite {
		return numericValue(ctx, decimal{special: numericNaN}, outputBytes)
	}
	mark := m.work.Checkpoint()
	defer func() { err = numericAllocationError(errors.Join(err, m.work.ReleaseAfter(mark))) }()
	coefficient, err := m.work.FromInt64(ctx, m.count)
	if err != nil {
		return model.Value{}, err
	}
	n := decimal{coefficient: coefficient}
	sum := decimal{coefficient: m.sum, scale: m.sumScale}
	squares := decimal{coefficient: m.sumSquares, scale: m.squaresScale}
	sumSquared, err := numericMomentsProduct(ctx, m.work, sum, sum)
	if err != nil {
		return model.Value{}, err
	}
	scaledSquares, err := numericMomentsProduct(ctx, m.work, n, squares)
	if err != nil {
		return model.Value{}, err
	}
	numerator, err := numericArithmetic(ctx, m.work, scaledSquares, sumSquared, opMi)
	if err != nil {
		return model.Value{}, err
	}
	if numerator.coefficient.Sign() <= 0 {
		zero, err := m.work.FromInt64(ctx, 0)
		if err != nil {
			return model.Value{}, err
		}
		return numericValue(ctx, decimal{coefficient: zero}, m.outputBudget(outputBytes))
	}
	factor := n
	if sample {
		factor.coefficient, err = m.work.FromInt64(ctx, m.count-1)
		if err != nil {
			return model.Value{}, err
		}
	}
	denominator, err := numericMomentsProduct(ctx, m.work, n, factor)
	if err != nil {
		return model.Value{}, err
	}
	quotient, err := numericDivide(ctx, m.work, numerator, denominator, false)
	if err != nil {
		return model.Value{}, err
	}
	if statistic == NumericStddevPopulation || statistic == NumericStddevSample {
		quotient, err = numericSqrt(ctx, m.work, quotient, quotient.scale)
		if err != nil {
			return model.Value{}, err
		}
	}
	return numericValue(ctx, quotient, m.outputBudget(outputBytes))
}

func (m *NumericMoments) outputBudget(outputBytes int64) int64 {
	return min(outputBytes, int64(NumericMomentsWorkBytes-m.work.UsedBytes()))
}
func (m *NumericMoments) Close() error {
	if m == nil {
		return nil
	}
	work := m.work
	*m = NumericMoments{}
	if work != nil {
		return work.Close()
	}
	return nil
}

func numericMomentsProduct(ctx context.Context, work *decimalint.Workspace, left, right decimal) (decimal, error) {
	if err := numericWork(ctx, numericDigits(left.coefficient)+numericDigits(right.coefficient)); err != nil {
		return decimal{}, err
	}
	coefficient, err := work.Mul(ctx, left.coefficient, right.coefficient)
	return decimal{coefficient: coefficient, scale: left.scale + right.scale}, err
}

func numericMomentsStateBudget(sum, squares *decimalint.Int, workspace bool, budget uint64) error {
	size := uint64(unsafe.Sizeof(NumericMoments{})) + sum.OwnedBytes() + squares.OwnedBytes()
	if workspace {
		size += uint64(unsafe.Sizeof(decimalint.Workspace{}))
	}
	if size > budget {
		return numericResourceError()
	}
	return nil
}
