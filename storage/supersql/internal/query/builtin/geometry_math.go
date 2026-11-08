package builtin

import (
	"context"
	"math"
)

const geoEpsilon = 1e-6

func geoFPZero(a float64) bool  { return math.Abs(a) <= geoEpsilon }
func geoFPEq(a, b float64) bool { return a == b || math.Abs(a-b) <= geoEpsilon }
func geoFPNe(a, b float64) bool { return a != b && math.Abs(a-b) > geoEpsilon }
func geoFPLt(a, b float64) bool { return a+geoEpsilon < b }
func geoFPLe(a, b float64) bool { return a <= b+geoEpsilon }
func geoFPGt(a, b float64) bool { return a > b+geoEpsilon }
func geoFPGe(a, b float64) bool { return a+geoEpsilon >= b }
func geoPointSame(a, b geoPoint) bool {
	if math.IsNaN(a.x) || math.IsNaN(a.y) || math.IsNaN(b.x) || math.IsNaN(b.y) {
		return compareFloat(a.x, b.x) == 0 && compareFloat(a.y, b.y) == 0
	}
	return geoFPEq(a.x, b.x) && geoFPEq(a.y, b.y)
}

func geoAdd(a, b float64) (float64, error) {
	value, err := floatArithmetic(a, b, 701, opPl)
	return value.FloatVal, err
}
func geoSub(a, b float64) (float64, error) {
	value, err := floatArithmetic(a, b, 701, opMi)
	return value.FloatVal, err
}
func geoMul(a, b float64) (float64, error) {
	value, err := floatArithmetic(a, b, 701, opMul)
	return value.FloatVal, err
}
func geoDiv(a, b float64) (float64, error) {
	value, err := floatArithmetic(a, b, 701, opDiv)
	return value.FloatVal, err
}

// PostgreSQL pg_hypot deliberately uses this scaled expression rather than the
// host libm hypot implementation; overflow and underflow remain SQL errors.
func geoHypot(x, y float64) (float64, error) {
	if math.IsInf(x, 0) || math.IsInf(y, 0) {
		return math.Inf(1), nil
	}
	if math.IsNaN(x) || math.IsNaN(y) {
		return math.Float64frombits(0x7ff8000000000000), nil
	}
	x, y = math.Abs(x), math.Abs(y)
	if x < y {
		x, y = y, x
	}
	if y == 0 {
		return x, nil
	}
	ratio := y / x
	result := x * math.Sqrt(1+ratio*ratio)
	if math.IsInf(result, 0) {
		return 0, sqlError("22003", "value out of range: overflow")
	}
	if result == 0 {
		return 0, sqlError("22003", "value out of range: underflow")
	}
	return result, nil
}

func geoPointDistance(a, b geoPoint) (float64, error) {
	x, err := geoSub(a.x, b.x)
	if err != nil {
		return 0, err
	}
	y, err := geoSub(a.y, b.y)
	if err != nil {
		return 0, err
	}
	return geoHypot(x, y)
}

func geoBoxFromPoints(a, b geoPoint) geoBox {
	box := geoBox{high: b, low: a}
	if compareFloat(a.x, b.x) > 0 {
		box.high.x, box.low.x = a.x, b.x
	}
	if compareFloat(a.y, b.y) > 0 {
		box.high.y, box.low.y = a.y, b.y
	}
	return box
}

func geoPolygonBounds(ctx context.Context, polygon geoPolygon) (geoBox, error) {
	first := polygon.points.at(0)
	box := geoBox{high: first, low: first}
	for i := 1; i < polygon.points.count(); i++ {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return geoBox{}, err
			}
		}
		point := polygon.points.at(i)
		if compareFloat(point.x, box.low.x) < 0 {
			box.low.x = point.x
		}
		if compareFloat(point.x, box.high.x) > 0 {
			box.high.x = point.x
		}
		if compareFloat(point.y, box.low.y) < 0 {
			box.low.y = point.y
		}
		if compareFloat(point.y, box.high.y) > 0 {
			box.high.y = point.y
		}
	}
	return box, ctx.Err()
}

// Circle construction only supplies angles in [0, 2*pi). Preserve the low
// half-pi bits during reduction so near-axis coordinates do not lose precision.
func geoCircleSinCos(angle float64) (float64, float64) {
	reduced, quadrant := floatTrigReducedAngle(angle)
	if math.Abs(reduced) >= floatTrigCancellationRange {
		return math.Sincos(angle)
	}
	sine, cosine := math.Sincos(reduced)
	switch quadrant {
	case 1:
		return cosine, -sine
	case 2:
		return -sine, -cosine
	case 3:
		return -cosine, sine
	default:
		return sine, cosine
	}
}
