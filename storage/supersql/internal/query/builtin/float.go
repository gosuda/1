package builtin

import (
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// PostgreSQL 18.4 utils/float.h orders NaN above infinity and equates all NaNs.
func compareFloat(left, right float64) int {
	if math.IsNaN(left) {
		if math.IsNaN(right) {
			return 0
		}
		return 1
	}
	if math.IsNaN(right) || left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func floatUnary(value float64, op primitiveOp) model.Value {
	switch op {
	case opUm:
		value = -value
	case opAbs:
		value = math.Abs(value)
	case opRound:
		value = math.RoundToEven(value)
	case opTrunc:
		value = math.Trunc(value)
	case opCeil:
		value = math.Ceil(value)
	case opFloor:
		value = math.Floor(value)
	case opSign:
		// PostgreSQL float8 sign maps NaN and either zero to positive zero.
		switch {
		case value > 0:
			value = 1
		case value < 0:
			value = -1
		default:
			value = 0
		}
	}
	return model.Float64Val(value)
}

func floatArithmetic(left, right float64, id model.TypeID, op primitiveOp) (model.Value, error) {
	var result float64
	if op == opDiv && right == 0 && !math.IsNaN(left) {
		return model.Value{}, sqlError("22012", "division by zero")
	}
	if id == 700 {
		// Perform binary32 division directly: binary64 intermediate rounding can differ.
		a, b := float32(left), float32(right)
		switch op {
		case opPl:
			result = float64(a + b)
		case opMi:
			result = float64(a - b)
		case opMul:
			result = float64(a * b)
		case opDiv:
			result = float64(a / b)
		}
	} else {
		switch op {
		case opPl:
			result = left + right
		case opMi:
			result = left - right
		case opMul:
			result = left * right
		case opDiv:
			result = left / right
		}
	}
	if math.IsInf(result, 0) && !math.IsInf(left, 0) {
		if op == opDiv || !math.IsInf(right, 0) {
			return model.Value{}, sqlError("22003", "value out of range: overflow")
		}
	}
	if result == 0 && left != 0 {
		productUnderflow := op == opMul && right != 0
		divisionUnderflow := op == opDiv && !math.IsInf(right, 0)
		if productUnderflow || divisionUnderflow {
			return model.Value{}, sqlError("22003", "value out of range: underflow")
		}
	}
	return model.Float64Val(result), nil
}

func narrowFloat(value float64) (model.Value, error) {
	result := float64(float32(value))
	if math.IsInf(result, 0) && !math.IsInf(value, 0) {
		return model.Value{}, sqlError("22003", "value out of range: overflow")
	}
	if result == 0 && value != 0 {
		return model.Value{}, sqlError("22003", "value out of range: underflow")
	}
	return model.Float64Val(result), nil
}

func floatToInteger(value float64, id model.TypeID) (model.Value, error) {
	value = math.RoundToEven(value)
	low, _ := integerLimits(id)
	// The upper bound is exclusive: float64(MaxInt64) is already 2^63.
	if math.IsNaN(value) || value < float64(low) || value >= -float64(low) {
		return model.Value{}, integerOverflow(id)
	}
	return model.Int64Val(int64(value)), nil
}
