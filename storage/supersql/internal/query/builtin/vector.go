package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// VectorKernel operates on verified dense registers. Active lanes are unique,
// in range, and outputs do not alias arguments. Resolution happens at compile time.
type VectorKernel func(context.Context, []int, []*columnar.Vector, *columnar.Vector) error

// ValidateTypes checks SQL identities and physical storage before a vector plan
// can bypass the scalar Call boundary's argument validation.
func (c Callable) ValidateTypes(arguments []model.Type, result model.Type) error {
	if c.kernel == nil || len(arguments) != c.ArgumentCount() || result.ID != c.signature.Result || result.Storage != c.resultStorage {
		return sqlError("42804", "builtin result type mismatch")
	}
	for i, argument := range arguments {
		if c.argumentTypes != nil {
			if argument != c.argumentTypes[i] {
				return sqlError("42804", "builtin argument type mismatch")
			}
			continue
		}
		// Shape-only anyarray kernels do not depend on the bound element identity.
		if c.signature.Arguments[i] == 2277 {
			if argument.Storage != model.TypeArray || argument.ID == 0 || argument.ID == 2277 {
				return sqlError("42804", "builtin anyarray argument requires a concrete array type")
			}
			if info, known := LookupType(argument.ID); known && !info.IsArray() {
				return sqlError("42804", "builtin anyarray argument has a non-array SQL identity")
			}
			continue
		}
		if argument.ID != c.signature.Arguments[i] || argument.Storage != c.argumentStorage[i] {
			return sqlError("42804", "builtin argument type mismatch")
		}
	}
	return nil
}

func hasPrimitiveVector(first model.TypeID, op primitiveOp) bool {
	switch op {
	case opEq, opNe, opLt, opLe, opGt, opGe, opCmp:
		return isInteger(first) || isFloat(first) || first == 16
	case opPl, opMi, opMul, opDiv, opUm, opUp, opAbs:
		return isInteger(first) || isFloat(first)
	case opMod, opBitAnd, opBitOr, opBitXor, opBitNot, opShiftLeft, opShiftRight, opGCD, opLCM:
		return isInteger(first)
	case opRound, opTrunc, opCeil, opFloor, opSign:
		return isFloat(first)
	default:
		return false
	}
}

// VectorKernel returns nil for implemented scalar-only functions. Missing
// implementations have already failed Resolve; this is not a registry fallback.
func (c Callable) VectorKernel() VectorKernel {
	sig := c.signature
	op := primitiveOperation(sig.ID)
	first := sig.Arguments[0]
	if c.kernel == nil || !hasPrimitiveVector(first, op) {
		return nil
	}
	if isFloat(first) {
		return func(ctx context.Context, lanes []int, args []*columnar.Vector, out *columnar.Vector) error {
			for i, lane := range lanes {
				if i&255 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				for a, v := range args {
					if v.Nulls[lane/8]&(1<<uint(lane%8)) != 0 {
						continue
					}
					x := v.Float64[lane]
					if sig.Arguments[a] == 700 && !math.IsNaN(x) && float64(float32(x)) != x {
						return sqlError("42804", "float4 argument is not rounded to binary32")
					}
				}
				if vectorNull(args, lane) {
					out.Nulls[lane/8] |= 1 << uint(lane%8)
					continue
				}
				left := args[0].Float64[lane]
				var value model.Value
				var err error
				switch {
				case len(args) == 1:
					value = floatUnary(left, op)
				case isComparison(op):
					value = comparisonResult(compareFloat(left, args[1].Float64[lane]), op)
				default:
					value, err = floatArithmetic(left, args[1].Float64[lane], sig.Result, op)
				}
				if err != nil {
					return err
				}
				storePrimitive(out, lane, value)
			}
			return ctx.Err()
		}
	}
	return func(ctx context.Context, lanes []int, args []*columnar.Vector, out *columnar.Vector) error {
		for i, lane := range lanes {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			for a, v := range args {
				if v.Nulls[lane/8]&(1<<uint(lane%8)) != 0 {
					continue
				}
				if sig.Arguments[a] == 16 {
					if v.Int64[lane] != 0 && v.Int64[lane] != 1 {
						return sqlError("42804", "invalid boolean physical value")
					}
				} else if err := integerRange(v.Int64[lane], sig.Arguments[a]); err != nil {
					return err
				}
			}
			if vectorNull(args, lane) {
				out.Nulls[lane/8] |= 1 << uint(lane%8)
				continue
			}
			left := args[0].Int64[lane]
			var value model.Value
			var err error
			switch {
			case len(args) == 1:
				value, err = integerUnary(left, sig.Result, op)
			case isComparison(op):
				order := 0
				if left < args[1].Int64[lane] {
					order = -1
				} else if left > args[1].Int64[lane] {
					order = 1
				}
				value = comparisonResult(order, op)
			default:
				value, err = integerArithmetic(left, args[1].Int64[lane], sig.Result, op)
			}
			if err != nil {
				return err
			}
			storePrimitive(out, lane, value)
		}
		return ctx.Err()
	}
}

func vectorNull(args []*columnar.Vector, lane int) bool {
	for _, v := range args {
		if v.Nulls[lane/8]&(1<<uint(lane%8)) != 0 {
			return true
		}
	}
	return false
}

func storePrimitive(out *columnar.Vector, lane int, value model.Value) {
	out.Nulls[lane/8] &^= 1 << uint(lane%8)
	if value.Type == model.TypeFloat64 {
		out.Float64[lane] = value.FloatVal
	} else {
		out.Int64[lane] = value.IntVal
	}
}
