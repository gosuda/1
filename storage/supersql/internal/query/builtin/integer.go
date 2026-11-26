package builtin

import (
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func integerLimits(id model.TypeID) (int64, int64) {
	switch id {
	case 21:
		return math.MinInt16, math.MaxInt16
	case 23:
		return math.MinInt32, math.MaxInt32
	default:
		return math.MinInt64, math.MaxInt64
	}
}
func integerOverflow(id model.TypeID) error {
	switch id {
	case 21:
		return sqlError("22003", "smallint out of range")
	case 23:
		return sqlError("22003", "integer out of range")
	default:
		return sqlError("22003", "bigint out of range")
	}
}
func integerRange(value int64, id model.TypeID) error {
	low, high := integerLimits(id)
	if value < low || value > high {
		return integerOverflow(id)
	}
	return nil
}

func integerUnary(value int64, id model.TypeID, op primitiveOp) (model.Value, error) {
	if op == opBitNot {
		return model.Int64Val(^value), nil
	}
	if op == opUm || (op == opAbs && value < 0) {
		low, _ := integerLimits(id)
		if value == low {
			return model.Value{}, integerOverflow(id)
		}
		value = -value
	}
	return model.Int64Val(value), nil
}

func integerArithmetic(left, right int64, id model.TypeID, op primitiveOp) (model.Value, error) {
	var result int64
	switch op {
	case opGCD, opLCM:
		return integerGCDLCM(left, right, id, op)
	case opBitAnd:
		result = left & right
	case opBitOr:
		result = left | right
	case opBitXor:
		result = left ^ right
	case opShiftLeft, opShiftRight:
		return model.Int64Val(integerShift(left, right, id, op)), nil
	case opPl:
		result = left + right
		if (left^result)&(right^result) < 0 {
			return model.Value{}, integerOverflow(id)
		}
	case opMi:
		result = left - right
		if (left^right)&(left^result) < 0 {
			return model.Value{}, integerOverflow(id)
		}
	case opMul:
		result = left * right
		if left == math.MinInt64 && right == -1 {
			return model.Value{}, integerOverflow(id)
		}
		if right == math.MinInt64 && left == -1 {
			return model.Value{}, integerOverflow(id)
		}
		if right != 0 && result/right != left {
			return model.Value{}, integerOverflow(id)
		}
	case opDiv, opMod:
		if right == 0 {
			return model.Value{}, sqlError("22012", "division by zero")
		}
		if op == opMod {
			// PostgreSQL permits MIN % -1 even though MIN / -1 overflows.
			if right == -1 {
				return model.Int64Val(0), nil
			}
			result = left % right
		} else {
			if left == math.MinInt64 && right == -1 {
				return model.Value{}, integerOverflow(id)
			}
			result = left / right
		}
	default:
		return model.Value{}, sqlError("XX000", "invalid integer kernel operation")
	}
	if err := integerRange(result, id); err != nil {
		return model.Value{}, err
	}
	return model.Int64Val(result), nil
}

func integerShift(value, count int64, id model.TypeID, op primitiveOp) int64 {
	// PostgreSQL uses native shifts; int2 is promoted to int32 before shifting.
	mask := uint64(31)
	if id == 20 {
		mask = 63
	}
	shift := uint64(count) & mask
	if op == opShiftRight {
		return value >> shift
	}
	value <<= shift
	switch id {
	case 21:
		return int64(int16(value))
	case 23:
		return int64(int32(value))
	default:
		return value
	}
}
