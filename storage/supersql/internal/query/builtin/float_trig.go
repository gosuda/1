package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const floatTrigRadiansPerDegree float64 = 0.0174532925199432957692

func compileFloatTrig(signature Signature) scalarKernel {
	id := signature.ID
	switch id {
	case 1610:
		return func(_ context.Context, _ []model.Value, callBudget CallBudget) (model.Value, error) {
			return model.Float64Val(math.Pi), nil
		}
	case 1603, 2734:
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			y, x := args[0].FloatVal, args[1].FloatVal
			if math.IsNaN(y) || math.IsNaN(x) {
				return model.Float64Val(math.NaN()), nil
			}
			result := math.Atan2(y, x)
			if id == 2734 {
				result = (result / floatTrigDegrees.atanOne) * 45
			}
			if math.IsInf(result, 0) {
				return model.Value{}, sqlError("22003", "value out of range: overflow")
			}
			return model.Float64Val(result), nil
		}
	case 1600, 1601, 1602, 1604, 1605, 1606, 1607, 1608, 1609,
		2462, 2463, 2464, 2465, 2466, 2467,
		2731, 2732, 2733, 2735, 2736, 2737, 2738:
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			x := args[0].FloatVal
			if math.IsNaN(x) {
				return model.Float64Val(math.NaN()), nil
			}
			switch id {
			case 1600, 1601, 2467, 2731, 2732:
				if x < -1 || x > 1 {
					return model.Value{}, sqlError("22003", "input is out of range")
				}
			case 1604, 1605, 1606, 1607, 2735, 2736, 2737, 2738:
				if math.IsInf(x, 0) {
					return model.Value{}, sqlError("22003", "input is out of range")
				}
			case 2466:
				if x < 1 {
					return model.Value{}, sqlError("22003", "input is out of range")
				}
			}

			var result float64
			switch id {
			case 1600:
				result = math.Asin(x)
			case 1601:
				result = math.Acos(x)
			case 1602:
				result = math.Atan(x)
			case 1604:
				result = floatTrigSin(x)
			case 1605:
				result = floatTrigCos(x)
			case 1606:
				result = math.Tan(x)
			case 1607:
				result = 1 / math.Tan(x)
			case 1608:
				result = x / floatTrigRadiansPerDegree
			case 1609:
				result = x * floatTrigRadiansPerDegree
			case 2462:
				if math.Abs(x) > 709 {
					result = math.Copysign(floatTrigHalfExp(math.Abs(x)), x)
				} else {
					result = math.Sinh(x)
				}
			case 2463:
				if math.Abs(x) > 709 {
					result = floatTrigHalfExp(math.Abs(x))
				} else {
					result = math.Cosh(x)
				}
			case 2464:
				result = math.Tanh(x)
			case 2465:
				result = math.Asinh(x)
			case 2466:
				result = math.Acosh(x)
			case 2467:
				result = math.Atanh(x)
			case 2731:
				if x >= 0 {
					result = floatTrigDegrees.asinQ1(x)
				} else {
					result = -floatTrigDegrees.asinQ1(-x)
				}
			case 2732:
				if x >= 0 {
					result = floatTrigDegrees.acosQ1(x)
				} else {
					result = 90 + floatTrigDegrees.asinQ1(-x)
				}
			case 2733:
				result = (math.Atan(x) / floatTrigDegrees.atanOne) * 45
			case 2735, 2736, 2737, 2738:
				result = floatTrigDegreePeriodic(x, id)
			}

			switch id {
			case 1606, 1607, 2462, 2465, 2466, 2467, 2737, 2738:
				// Poles and hyperbolic overflow produce infinity, not SQL errors.
			case 2463:
				if result == 0 {
					return model.Value{}, sqlError("22003", "value out of range: underflow")
				}
			case 1608, 1609:
				if math.IsInf(result, 0) && !math.IsInf(x, 0) {
					return model.Value{}, sqlError("22003", "value out of range: overflow")
				}
				if result == 0 && x != 0 {
					return model.Value{}, sqlError("22003", "value out of range: underflow")
				}
			default:
				if math.IsInf(result, 0) {
					return model.Value{}, sqlError("22003", "value out of range: overflow")
				}
			}
			return model.Float64Val(result), nil
		}
	default:
		return nil
	}
}

func floatTrigHalfExp(x float64) float64 {
	// exp(x) overflows before exp(x)/2, the large-argument sinh/cosh limit.
	e := math.Exp(x / 2)
	return (e * 0.5) * e
}

type floatTrigDegreeConstants struct {
	sinThirty     float64
	oneMinusCos60 float64
	asinHalf      float64
	acosHalf      float64
	atanOne       float64
	tan45         float64
	cot45         float64
}

// Calibrate with the same math functions used at runtime. This state is immutable
// after package initialization; the ratios enforce PostgreSQL's exact landmarks.
var floatTrigDegrees = floatTrigInitDegrees()

func floatTrigInitDegrees() floatTrigDegreeConstants {
	thirty, sixty := float64(30), float64(60)
	c := floatTrigDegreeConstants{
		sinThirty:     math.Sin(thirty * floatTrigRadiansPerDegree),
		oneMinusCos60: 1 - math.Cos(sixty*floatTrigRadiansPerDegree),
		asinHalf:      math.Asin(0.5),
		acosHalf:      math.Acos(0.5),
		atanOne:       math.Atan(1),
	}
	sin45, cos45 := c.sinQ1(45), c.cosQ1(45)
	c.tan45, c.cot45 = sin45/cos45, cos45/sin45
	return c
}

func (c *floatTrigDegreeConstants) asinQ1(x float64) float64 {
	if x <= 0.5 {
		return (math.Asin(x) / c.asinHalf) * 30
	}
	return 90 - float64((math.Acos(x)/c.acosHalf)*60)
}

func (c *floatTrigDegreeConstants) acosQ1(x float64) float64 {
	if x <= 0.5 {
		return 90 - float64((math.Asin(x)/c.asinHalf)*30)
	}
	return (math.Acos(x) / c.acosHalf) * 60
}

func (c *floatTrigDegreeConstants) sin0To30(x float64) float64 {
	return (math.Sin(x*floatTrigRadiansPerDegree) / c.sinThirty) / 2
}

func (c *floatTrigDegreeConstants) cos0To60(x float64) float64 {
	return 1 - ((1-math.Cos(x*floatTrigRadiansPerDegree))/c.oneMinusCos60)/2
}

func (c *floatTrigDegreeConstants) sinQ1(x float64) float64 {
	if x <= 30 {
		return c.sin0To30(x)
	}
	return c.cos0To60(90 - x)
}

func (c *floatTrigDegreeConstants) cosQ1(x float64) float64 {
	if x <= 60 {
		return c.cos0To60(x)
	}
	return c.sin0To30(90 - x)
}

func floatTrigDegreePeriodic(x float64, id model.FunctionID) float64 {
	x = math.Mod(x, 360)
	sign := 1.0
	if x < 0 {
		x = -x
		if id != 2736 {
			sign = -sign
		}
	}
	if x > 180 {
		x = 360 - x
		if id != 2736 {
			sign = -sign
		}
	}
	if x > 90 {
		x = 180 - x
		if id != 2735 {
			sign = -sign
		}
	}

	switch id {
	case 2735:
		return sign * floatTrigDegrees.sinQ1(x)
	case 2736:
		return sign * floatTrigDegrees.cosQ1(x)
	default:
		sin, cos := floatTrigDegrees.sinQ1(x), floatTrigDegrees.cosQ1(x)
		var result float64
		if id == 2737 {
			result = sign * ((sin / cos) / floatTrigDegrees.tan45)
		} else {
			result = sign * ((cos / sin) / floatTrigDegrees.cot45)
		}
		// PostgreSQL canonicalizes zero only for tand/cotd, not sind.
		if result == 0 {
			return 0
		}
		return result
	}
}
