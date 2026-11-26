package builtin

import "math"

func geoPointAdd(a, b geoPoint) (geoPoint, error) {
	x, err := geoAdd(a.x, b.x)
	if err != nil {
		return geoPoint{}, err
	}
	y, err := geoAdd(a.y, b.y)
	return geoPoint{x, y}, err
}

func geoPointSub(a, b geoPoint) (geoPoint, error) {
	x, err := geoSub(a.x, b.x)
	if err != nil {
		return geoPoint{}, err
	}
	y, err := geoSub(a.y, b.y)
	return geoPoint{x, y}, err
}

func geoPointMul(a, b geoPoint) (geoPoint, error) {
	xx, err := geoMul(a.x, b.x)
	if err != nil {
		return geoPoint{}, err
	}
	yy, err := geoMul(a.y, b.y)
	if err != nil {
		return geoPoint{}, err
	}
	x, err := geoSub(xx, yy)
	if err != nil {
		return geoPoint{}, err
	}
	xy, err := geoMul(a.x, b.y)
	if err != nil {
		return geoPoint{}, err
	}
	yx, err := geoMul(a.y, b.x)
	if err != nil {
		return geoPoint{}, err
	}
	y, err := geoAdd(xy, yx)
	return geoPoint{x, y}, err
}

func geoPointDiv(a, b geoPoint) (geoPoint, error) {
	xx, err := geoMul(b.x, b.x)
	if err != nil {
		return geoPoint{}, err
	}
	yy, err := geoMul(b.y, b.y)
	if err != nil {
		return geoPoint{}, err
	}
	denominator, err := geoAdd(xx, yy)
	if err != nil {
		return geoPoint{}, err
	}
	xx, err = geoMul(a.x, b.x)
	if err != nil {
		return geoPoint{}, err
	}
	yy, err = geoMul(a.y, b.y)
	if err != nil {
		return geoPoint{}, err
	}
	x, err := geoAdd(xx, yy)
	if err != nil {
		return geoPoint{}, err
	}
	x, err = geoDiv(x, denominator)
	if err != nil {
		return geoPoint{}, err
	}
	yx, err := geoMul(a.y, b.x)
	if err != nil {
		return geoPoint{}, err
	}
	xy, err := geoMul(a.x, b.y)
	if err != nil {
		return geoPoint{}, err
	}
	y, err := geoSub(yx, xy)
	if err != nil {
		return geoPoint{}, err
	}
	y, err = geoDiv(y, denominator)
	return geoPoint{x, y}, err
}

func geoPointSlope(a, b geoPoint) (float64, error) {
	if geoFPEq(a.x, b.x) {
		return math.Inf(1), nil
	}
	if geoFPEq(a.y, b.y) {
		return 0, nil
	}
	y, err := geoSub(a.y, b.y)
	if err != nil {
		return 0, err
	}
	x, err := geoSub(a.x, b.x)
	if err != nil {
		return 0, err
	}
	return geoDiv(y, x)
}

func geoPointInverseSlope(a, b geoPoint) (float64, error) {
	if geoFPEq(a.x, b.x) {
		return 0, nil
	}
	if geoFPEq(a.y, b.y) {
		return math.Inf(1), nil
	}
	x, err := geoSub(a.x, b.x)
	if err != nil {
		return 0, err
	}
	y, err := geoSub(b.y, a.y)
	if err != nil {
		return 0, err
	}
	return geoDiv(x, y)
}

func geoBoxCenter(box geoBox) (geoPoint, error) {
	x, err := geoAdd(box.high.x, box.low.x)
	if err != nil {
		return geoPoint{}, err
	}
	x, err = geoDiv(x, 2)
	if err != nil {
		return geoPoint{}, err
	}
	y, err := geoAdd(box.high.y, box.low.y)
	if err != nil {
		return geoPoint{}, err
	}
	y, err = geoDiv(y, 2)
	return geoPoint{x, y}, err
}

func geoBoxArea(box geoBox) (float64, error) {
	width, err := geoSub(box.high.x, box.low.x)
	if err != nil {
		return 0, err
	}
	height, err := geoSub(box.high.y, box.low.y)
	if err != nil {
		return 0, err
	}
	return geoMul(width, height)
}

func geoBoxContainsPoint(box geoBox, p geoPoint) bool {
	return box.high.x >= p.x && box.low.x <= p.x && box.high.y >= p.y && box.low.y <= p.y
}

func geoBoxesOverlap(a, b geoBox) bool {
	return geoFPLe(a.low.x, b.high.x) && geoFPLe(b.low.x, a.high.x) &&
		geoFPLe(a.low.y, b.high.y) && geoFPLe(b.low.y, a.high.y)
}

func geoBoxContainsBox(a, b geoBox) bool {
	return geoFPGe(a.high.x, b.high.x) && geoFPLe(a.low.x, b.low.x) &&
		geoFPGe(a.high.y, b.high.y) && geoFPLe(a.low.y, b.low.y)
}

func geoClosestPointBox(box geoBox, p geoPoint) (geoPoint, float64, error) {
	if geoBoxContainsPoint(box, p) {
		return p, 0, nil
	}
	upperLeft := geoPoint{box.low.x, box.high.y}
	lowerRight := geoPoint{box.high.x, box.low.y}
	edges := [4]geoSegment{
		{box.low, upperLeft}, {box.high, upperLeft},
		{box.low, lowerRight}, {box.high, lowerRight},
	}
	closest, distance, err := geoSegmentClosestPoint(edges[0], p)
	if err != nil {
		return geoPoint{}, 0, err
	}
	for _, edge := range edges[1:] {
		candidate, d, err := geoSegmentClosestPoint(edge, p)
		if err != nil {
			return geoPoint{}, 0, err
		}
		if compareFloat(d, distance) < 0 {
			closest, distance = candidate, d
		}
	}
	return closest, distance, nil
}

// PostgreSQL chooses the second operand on ties, including signed zero.
func geoBoxFloatMin(a, b float64) float64 {
	if compareFloat(a, b) < 0 {
		return a
	}
	return b
}

func geoBoxFloatMax(a, b float64) float64 {
	if compareFloat(a, b) > 0 {
		return a
	}
	return b
}
