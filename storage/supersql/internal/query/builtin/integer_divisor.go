package builtin

import "github.com/gosuda/supersql-alpha/internal/model"

func integerGCDLCM(left, right int64, id model.TypeID, op primitiveOp) (model.Value, error) {
	if op == opLCM && (left == 0 || right == 0) {
		return model.Int64Val(0), nil
	}

	// Unsigned magnitudes preserve abs(MinInt64) until the result range check.
	a, b := uint64(left), uint64(right)
	if left < 0 {
		a = -a
	}
	if right < 0 {
		b = -b
	}
	gcd, remainder := a, b
	if gcd < remainder {
		gcd, remainder = remainder, gcd
	}
	for remainder != 0 {
		gcd, remainder = remainder, gcd%remainder
	}

	_, high := integerLimits(id)
	limit := uint64(high)
	switch op {
	case opGCD:
		if gcd > limit {
			return model.Value{}, integerOverflow(id)
		}
		return model.Int64Val(int64(gcd)), nil
	case opLCM:
		quotient := a / gcd
		if quotient > limit/b {
			return model.Value{}, integerOverflow(id)
		}
		return model.Int64Val(int64(quotient * b)), nil
	default:
		return model.Value{}, sqlError("XX000", "invalid integer kernel operation")
	}
}
