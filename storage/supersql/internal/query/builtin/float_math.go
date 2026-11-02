package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileFloatCore(signature Signature) scalarKernel {
	switch signature.ID {
	case 230, 1344:
		return floatCoreSqrt
	case 231, 1345:
		return floatCoreCbrt
	case 232, 1346, 1368:
		return floatCorePower
	case 233, 1347:
		return floatCoreExp
	case 234, 1341:
		return floatCoreLogarithm(math.Log)
	case 1194, 1339, 1340:
		return floatCoreLogarithm(math.Log10)
	default:
		return nil
	}
}

func floatCoreSqrt(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	input := args[0].FloatVal
	if input < 0 {
		return model.Value{}, sqlError("2201F", "cannot take square root of a negative number")
	}
	result := math.Sqrt(input)
	if math.IsInf(result, 0) && !math.IsInf(input, 0) {
		return model.Value{}, sqlError("22003", "value out of range: overflow")
	}
	if result == 0 && input != 0 {
		return model.Value{}, sqlError("22003", "value out of range: underflow")
	}
	return model.Float64Val(result), nil
}

func floatCoreCbrt(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	input := args[0].FloatVal
	result := math.Cbrt(input)
	if math.IsInf(result, 0) && !math.IsInf(input, 0) {
		return model.Value{}, sqlError("22003", "value out of range: overflow")
	}
	if result == 0 && input != 0 {
		return model.Value{}, sqlError("22003", "value out of range: underflow")
	}
	return model.Float64Val(result), nil
}

func floatCorePower(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	base, exponent := args[0].FloatVal, args[1].FloatVal
	if math.IsNaN(base) {
		if exponent == 0 {
			return model.Float64Val(1), nil
		}
		return model.Float64Val(math.NaN()), nil
	}
	if math.IsNaN(exponent) {
		if base == 1 {
			return model.Float64Val(1), nil
		}
		return model.Float64Val(math.NaN()), nil
	}
	if base == 0 && exponent < 0 {
		return model.Value{}, sqlError("2201F", "zero raised to a negative power is undefined")
	}
	if base < 0 && math.Floor(exponent) != exponent {
		return model.Value{}, sqlError("2201F", "a negative number raised to a non-integer power yields a complex result")
	}

	if math.IsInf(exponent, 0) {
		absoluteBase := math.Abs(base)
		if absoluteBase == 1 {
			return model.Float64Val(1), nil
		}
		growsWithPositiveExponent := exponent > 0 && absoluteBase > 1
		growsWithNegativeExponent := exponent < 0 && absoluteBase < 1
		if growsWithPositiveExponent || growsWithNegativeExponent {
			return model.Float64Val(math.Inf(1)), nil
		}
		return model.Float64Val(0), nil
	}
	if math.IsInf(base, 0) {
		if exponent == 0 {
			return model.Float64Val(1), nil
		}
		result := 0.0
		if exponent > 0 {
			result = math.Inf(1)
		}
		if base < 0 {
			// Testing the half-exponent avoids integer conversion at float8 magnitudes.
			halfExponent := exponent / 2
			if math.Floor(halfExponent) != halfExponent {
				result = math.Copysign(result, -1)
			}
		}
		return model.Float64Val(result), nil
	}

	result := math.Pow(base, exponent)
	if math.IsInf(result, 0) {
		return model.Value{}, sqlError("22003", "value out of range: overflow")
	}
	if result == 0 && base != 0 {
		return model.Value{}, sqlError("22003", "value out of range: underflow")
	}
	return model.Float64Val(result), nil
}

func floatCoreExp(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	input := args[0].FloatVal
	if math.IsNaN(input) || math.IsInf(input, 1) {
		return model.Float64Val(input), nil
	}
	if math.IsInf(input, -1) {
		return model.Float64Val(0), nil
	}
	var result float64
	if input < -1022*math.Ln2 {
		// Round once into the subnormal range instead of losing the last bit
		// while Exp scales a normal intermediate by a large negative exponent.
		half := math.Exp(input / 2)
		result = half * half
	} else {
		result = math.Exp(input)
	}
	if math.IsInf(result, 0) {
		return model.Value{}, sqlError("22003", "value out of range: overflow")
	}
	if result == 0 {
		return model.Value{}, sqlError("22003", "value out of range: underflow")
	}
	return model.Float64Val(result), nil
}

func floatCoreLogarithm(logarithm func(float64) float64) scalarKernel {
	return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		input := args[0].FloatVal
		if input == 0 {
			return model.Value{}, sqlError("2201E", "cannot take logarithm of zero")
		}
		if input < 0 {
			return model.Value{}, sqlError("2201E", "cannot take logarithm of a negative number")
		}
		result := logarithm(input)
		if math.IsInf(result, 0) && !math.IsInf(input, 0) {
			return model.Value{}, sqlError("22003", "value out of range: overflow")
		}
		if result == 0 && input != 1 {
			return model.Value{}, sqlError("22003", "value out of range: underflow")
		}
		return model.Float64Val(result), nil
	}
}
