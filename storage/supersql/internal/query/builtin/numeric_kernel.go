package builtin

import (
	"context"
	"errors"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/decimalint"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileNumeric(signature Signature, op primitiveOp) scalarKernel {
	return func(ctx context.Context, args []model.Value, budget CallBudget) (result model.Value, err error) {
		if op == opCast && signature.Arguments[0] != 1700 {
			return numericFromPrimitive(ctx, args[0], signature.Arguments[0], budget)
		}
		work, err := budget.NewWorkspace(ctx)
		if err != nil {
			return model.Value{}, numericAllocationError(err)
		}
		defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
		left, err := parseNumeric(ctx, work, args[0].StrVal)
		if err != nil {
			return model.Value{}, err
		}
		if op == opInput || op == opTypmod {
			index := 1
			if op == opInput {
				index = 2
			}
			left, err = numericTypmod(ctx, work, left, args[index].IntVal)
			if err != nil {
				return model.Value{}, err
			}
			return numericValue(ctx, left, budget.OutputLimit(work))
		}
		if left.special == numericFinite && left.scale > numericMaxScale {
			return model.Value{}, numericOverflow()
		}
		switch op {
		case opOutput:
			text, err := numericText(ctx, left, budget.OutputLimit(work))
			if err != nil {
				return model.Value{}, err
			}
			return model.StringVal(text), nil
		case opCast:
			return numericToPrimitive(ctx, work, left, signature.Result, budget)
		case opUm:
			left, err = numericNegate(ctx, work, left)
			if err != nil {
				return model.Value{}, err
			}
			return numericValue(ctx, left, budget.OutputLimit(work))
		case opUp:
			return numericValue(ctx, left, budget.OutputLimit(work))
		case opAbs:
			if numericSign(left) < 0 {
				left, err = numericNegate(ctx, work, left)
				if err != nil {
					return model.Value{}, err
				}
			}
			return numericValue(ctx, left, budget.OutputLimit(work))
		case opSign:
			if left.special != numericNaN {
				coefficient, err := work.FromInt64(ctx, int64(numericSign(left)))
				if err != nil {
					return model.Value{}, err
				}
				left = decimal{coefficient: coefficient}
			}
			return numericValue(ctx, left, budget.OutputLimit(work))
		case opSqrt:
			scale := 0
			if left.special == numericFinite {
				weight, _ := numericLeading(left)
				scale = min(max(16-(weight*2+1), left.scale, 0), 1000)
			}
			value, err := numericSqrt(ctx, work, left, scale)
			if err != nil {
				return model.Value{}, err
			}
			return numericValue(ctx, value, budget.OutputLimit(work))
		case opCeil, opFloor:
			value, err := numericIntegral(ctx, work, left, op)
			if err != nil {
				return model.Value{}, err
			}
			return numericValue(ctx, value, budget.OutputLimit(work))
		case opRound, opTrunc:
			scale := 0
			if len(args) == 2 {
				scale = int(args[1].IntVal)
			}
			lower := -numericMaxIntegerDigits
			if op == opRound {
				lower--
			}
			scale = min(max(scale, lower), numericMaxScale)
			value, err := numericRescale(ctx, work, left, scale, op == opRound)
			if err != nil {
				return model.Value{}, err
			}
			return numericValue(ctx, value, budget.OutputLimit(work))
		}
		right, err := parseNumeric(ctx, work, args[1].StrVal)
		if err != nil {
			return model.Value{}, err
		}
		if right.special == numericFinite && right.scale > numericMaxScale {
			return model.Value{}, numericOverflow()
		}
		if isComparison(op) {
			order, err := numericCompare(ctx, left, right)
			if err != nil {
				return model.Value{}, err
			}
			return comparisonResult(order, op), nil
		}
		value, err := numericArithmetic(ctx, work, left, right, op)
		if err != nil {
			return model.Value{}, err
		}
		return numericValue(ctx, value, budget.OutputLimit(work))
	}
}

func numericAllocationError(err error) error {
	if errors.Is(err, decimalint.ErrLimit) {
		return &model.Error{Code: "54000", Message: "numeric workspace limit exceeded", Cause: err}
	}
	return err
}

func numericTemporary(budget CallBudget, live, bytes uint64) (columnar.Allocation, error) {
	if live > budget.MemoryBytes || bytes > budget.MemoryBytes-live {
		return nil, numericResourceError()
	}
	if bytes == 0 || budget.Allocator == nil {
		return nil, nil
	}
	return budget.Allocator.Reserve(bytes)
}

func numericFromPrimitive(ctx context.Context, value model.Value, source model.TypeID, budget CallBudget) (result model.Value, err error) {
	var formatted []byte
	var buffer [32]byte
	if isFloat(source) && !math.IsNaN(value.FloatVal) && !math.IsInf(value.FloatVal, 0) {
		precision := 15
		if source == 700 {
			precision = 6
		}
		formatted = strconv.AppendFloat(buffer[:0], value.FloatVal, 'g', precision, 64)
	}
	claim, err := numericTemporary(budget, 0, uint64(len(formatted)))
	if err != nil {
		return model.Value{}, err
	}
	if claim != nil {
		defer claim.Release()
	}
	budget.MemoryBytes -= uint64(len(formatted))
	work, err := budget.NewWorkspace(ctx)
	if err != nil {
		return model.Value{}, numericAllocationError(err)
	}
	defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
	if isInteger(source) {
		coefficient, err := work.FromInt64(ctx, value.IntVal)
		if err != nil {
			return model.Value{}, err
		}
		return numericValue(ctx, decimal{coefficient: coefficient}, budget.OutputLimit(work))
	}
	if !isFloat(source) {
		return model.Value{}, sqlError("XX000", "invalid numeric cast source")
	}
	if math.IsNaN(value.FloatVal) {
		return numericValue(ctx, decimal{special: numericNaN}, budget.OutputLimit(work))
	}
	if math.IsInf(value.FloatVal, 0) {
		kind := numericPositiveInfinity
		if value.FloatVal < 0 {
			kind = numericNegativeInfinity
		}
		return numericValue(ctx, decimal{special: kind}, budget.OutputLimit(work))
	}
	valueDecimal, err := parseNumeric(ctx, work, string(formatted))
	if err != nil {
		return model.Value{}, err
	}
	return numericValue(ctx, valueDecimal, budget.OutputLimit(work))
}

func numericToPrimitive(ctx context.Context, work *decimalint.Workspace, value decimal, target model.TypeID, budget CallBudget) (model.Value, error) {
	if isInteger(target) {
		if value.special != numericFinite {
			return model.Value{}, integerOverflow(target)
		}
		rounded, err := numericRescale(ctx, work, value, 0, true)
		if err != nil {
			return model.Value{}, err
		}
		integer, ok := rounded.coefficient.Int64()
		if !ok {
			return model.Value{}, integerOverflow(target)
		}
		if err := integerRange(integer, target); err != nil {
			return model.Value{}, err
		}
		return model.Int64Val(integer), nil
	}
	if !isFloat(target) {
		return model.Value{}, sqlError("XX000", "invalid numeric cast target")
	}
	if value.special != numericFinite {
		result := math.NaN()
		if value.special == numericPositiveInfinity {
			result = math.Inf(1)
		}
		if value.special == numericNegativeInfinity {
			result = math.Inf(-1)
		}
		return model.Float64Val(result), nil
	}
	size, err := numericTextSize(value)
	if err != nil {
		return model.Value{}, err
	}
	claim, err := numericTemporary(budget, work.UsedBytes(), uint64(size))
	if err != nil {
		return model.Value{}, err
	}
	if claim != nil {
		defer claim.Release()
	}
	text, err := numericText(ctx, value, size)
	if err != nil {
		return model.Value{}, err
	}
	bits := 64
	if target == 700 {
		bits = 32
	}
	result, err := strconv.ParseFloat(text, bits)
	underflow := result == 0 && value.coefficient.Sign() != 0
	if err != nil || math.IsInf(result, 0) || underflow {
		return model.Value{}, sqlError("22003", "numeric value out of range for floating-point type")
	}
	return model.Float64Val(result), nil
}
