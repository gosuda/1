package builtin

import (
	"cmp"
	"math"
)

func geoLineConstruct(point geoPoint, slope float64) (geoLine, error) {
	if math.IsInf(slope, 0) {
		return geoLine{-1, 0, point.x}, nil
	}
	if slope == 0 {
		return geoLine{0, -1, point.y}, nil
	}
	product, err := geoMul(slope, point.x)
	if err != nil {
		return geoLine{}, err
	}
	c, err := geoSub(point.y, product)
	if err != nil {
		return geoLine{}, err
	}
	c = cmp.Or(c, 0)
	return geoLine{slope, -1, c}, nil
}

func geoLineFromPoints(a, b geoPoint) (geoLine, error) {
	if geoPointSame(a, b) {
		return geoLine{}, sqlError("22023", "invalid line specification: must be two distinct points")
	}
	slope, err := geoPointSlope(a, b)
	if err != nil {
		return geoLine{}, err
	}
	return geoLineConstruct(a, slope)
}

func geoLineSlope(line geoLine) (float64, error) {
	if geoFPZero(line.a) {
		return 0, nil
	}
	if geoFPZero(line.b) {
		return math.Inf(1), nil
	}
	return geoDiv(line.a, -line.b)
}

func geoLineInverseSlope(line geoLine) (float64, error) {
	if geoFPZero(line.a) {
		return math.Inf(1), nil
	}
	if geoFPZero(line.b) {
		return 0, nil
	}
	return geoDiv(line.b, line.a)
}

func geoLineIntersection(a, b geoLine) (geoPoint, bool, error) {
	if geoFPZero(a.b) {
		if geoFPZero(b.b) {
			return geoPoint{}, false, nil
		}
		a, b = b, a
	}
	ratio, err := geoDiv(b.b, a.b)
	if err != nil {
		return geoPoint{}, false, err
	}
	scaled, err := geoMul(a.a, ratio)
	if err != nil {
		return geoPoint{}, false, err
	}
	if geoFPEq(b.a, scaled) {
		return geoPoint{}, false, nil
	}
	n1, err := geoMul(a.b, b.c)
	if err != nil {
		return geoPoint{}, false, err
	}
	n2, err := geoMul(b.b, a.c)
	if err != nil {
		return geoPoint{}, false, err
	}
	numerator, err := geoSub(n1, n2)
	if err != nil {
		return geoPoint{}, false, err
	}
	d1, err := geoMul(a.a, b.b)
	if err != nil {
		return geoPoint{}, false, err
	}
	d2, err := geoMul(b.a, a.b)
	if err != nil {
		return geoPoint{}, false, err
	}
	denominator, err := geoSub(d1, d2)
	if err != nil {
		return geoPoint{}, false, err
	}
	x, err := geoDiv(numerator, denominator)
	if err != nil {
		return geoPoint{}, false, err
	}
	ax, err := geoMul(a.a, x)
	if err != nil {
		return geoPoint{}, false, err
	}
	sum, err := geoAdd(ax, a.c)
	if err != nil {
		return geoPoint{}, false, err
	}
	y, err := geoDiv(-sum, a.b)
	if err != nil {
		return geoPoint{}, false, err
	}
	x, y = cmp.Or(x, 0), cmp.Or(y, 0)
	return geoPoint{x, y}, true, nil
}

func geoLinePerpendicular(a, b geoLine) (bool, error) {
	if geoFPZero(a.a) {
		return geoFPZero(b.b), nil
	}
	if geoFPZero(b.a) {
		return geoFPZero(a.b), nil
	}
	if geoFPZero(a.b) {
		return geoFPZero(b.a), nil
	}
	if geoFPZero(b.b) {
		return geoFPZero(a.a), nil
	}
	numerator, err := geoMul(a.a, b.a)
	if err != nil {
		return false, err
	}
	denominator, err := geoMul(a.b, b.b)
	if err != nil {
		return false, err
	}
	ratio, err := geoDiv(numerator, denominator)
	return geoFPEq(ratio, -1), err
}

func geoLineEqual(a, b geoLine) (bool, error) {
	leftNaN := math.IsNaN(a.a) || math.IsNaN(a.b) || math.IsNaN(a.c)
	rightNaN := math.IsNaN(b.a) || math.IsNaN(b.b) || math.IsNaN(b.c)
	if leftNaN || rightNaN {
		return compareFloat(a.a, b.a) == 0 && compareFloat(a.b, b.b) == 0 && compareFloat(a.c, b.c) == 0, nil
	}
	ratio := 1.0
	var err error
	switch {
	case !geoFPZero(b.a):
		ratio, err = geoDiv(a.a, b.a)
	case !geoFPZero(b.b):
		ratio, err = geoDiv(a.b, b.b)
	case !geoFPZero(b.c):
		ratio, err = geoDiv(a.c, b.c)
	}
	if err != nil {
		return false, err
	}
	for _, pair := range [3][2]float64{{a.a, b.a}, {a.b, b.b}, {a.c, b.c}} {
		scaled, err := geoMul(ratio, pair[1])
		if err != nil {
			return false, err
		}
		if !geoFPEq(pair[0], scaled) {
			return false, nil
		}
	}
	return true, nil
}

func geoLineDistance(a, b geoLine) (float64, error) {
	_, intersects, err := geoLineIntersection(a, b)
	if err != nil {
		return 0, err
	}
	if intersects {
		return 0, nil
	}
	ratio := 1.0
	if !geoFPZero(a.a) && !math.IsNaN(a.a) && !geoFPZero(b.a) && !math.IsNaN(b.a) {
		ratio, err = geoDiv(a.a, b.a)
	} else if !geoFPZero(a.b) && !math.IsNaN(a.b) && !geoFPZero(b.b) && !math.IsNaN(b.b) {
		ratio, err = geoDiv(a.b, b.b)
	}
	if err != nil {
		return 0, err
	}
	scaled, err := geoMul(ratio, b.c)
	if err != nil {
		return 0, err
	}
	difference, err := geoSub(a.c, scaled)
	if err != nil {
		return 0, err
	}
	length, err := geoHypot(a.a, a.b)
	if err != nil {
		return 0, err
	}
	return geoDiv(math.Abs(difference), length)
}

func geoLineContainsPoint(line geoLine, p geoPoint) (bool, error) {
	x, err := geoMul(line.a, p.x)
	if err != nil {
		return false, err
	}
	y, err := geoMul(line.b, p.y)
	if err != nil {
		return false, err
	}
	sum, err := geoAdd(x, y)
	if err != nil {
		return false, err
	}
	sum, err = geoAdd(sum, line.c)
	return geoFPZero(sum), err
}

func geoSegmentContainsPoint(segment geoSegment, p geoPoint) (bool, error) {
	a, err := geoPointDistance(p, segment.p0)
	if err != nil {
		return false, err
	}
	b, err := geoPointDistance(p, segment.p1)
	if err != nil {
		return false, err
	}
	length, err := geoPointDistance(segment.p0, segment.p1)
	if err != nil {
		return false, err
	}
	// PostgreSQL deliberately leaves the triangle sum unchecked.
	return geoFPEq(a+b, length), nil
}

func geoSegmentLineIntersection(segment geoSegment, line geoLine) (geoPoint, bool, error) {
	slope, err := geoPointSlope(segment.p0, segment.p1)
	if err != nil {
		return geoPoint{}, false, err
	}
	promoted, err := geoLineConstruct(segment.p0, slope)
	if err != nil {
		return geoPoint{}, false, err
	}
	point, ok, err := geoLineIntersection(promoted, line)
	if err != nil || !ok {
		return geoPoint{}, false, err
	}
	ok, err = geoSegmentContainsPoint(segment, point)
	if err != nil || !ok {
		return geoPoint{}, false, err
	}
	if geoPointSame(segment.p0, point) {
		point = segment.p0
	} else if geoPointSame(segment.p1, point) {
		point = segment.p1
	}
	return point, true, nil
}

func geoSegmentIntersection(a, b geoSegment) (geoPoint, bool, error) {
	slope, err := geoPointSlope(b.p0, b.p1)
	if err != nil {
		return geoPoint{}, false, err
	}
	line, err := geoLineConstruct(b.p0, slope)
	if err != nil {
		return geoPoint{}, false, err
	}
	point, ok, err := geoSegmentLineIntersection(a, line)
	if err != nil || !ok {
		return geoPoint{}, false, err
	}
	ok, err = geoSegmentContainsPoint(b, point)
	return point, ok, err
}

func geoLineClosestPoint(line geoLine, point geoPoint) (geoPoint, float64, error) {
	slope, err := geoLineInverseSlope(line)
	if err != nil {
		return geoPoint{}, 0, err
	}
	perpendicular, err := geoLineConstruct(point, slope)
	if err != nil {
		return geoPoint{}, 0, err
	}
	closest, ok, err := geoLineIntersection(perpendicular, line)
	if err != nil {
		return geoPoint{}, 0, err
	}
	if !ok {
		return point, math.NaN(), nil
	}
	distance, err := geoPointDistance(closest, point)
	return closest, distance, err
}

func geoSegmentClosestLine(segment geoSegment, line geoLine) (geoPoint, float64, error) {
	point, ok, err := geoSegmentLineIntersection(segment, line)
	if err != nil {
		return geoPoint{}, 0, err
	}
	if ok {
		return point, 0, nil
	}
	_, a, err := geoLineClosestPoint(line, segment.p0)
	if err != nil {
		return geoPoint{}, 0, err
	}
	_, b, err := geoLineClosestPoint(line, segment.p1)
	if err != nil {
		return geoPoint{}, 0, err
	}
	if a < b {
		return segment.p0, a, nil
	}
	return segment.p1, b, nil
}

func geoSegmentClosestPoint(segment geoSegment, p geoPoint) (geoPoint, float64, error) {
	slope, err := geoPointInverseSlope(segment.p0, segment.p1)
	if err != nil {
		return geoPoint{}, 0, err
	}
	perpendicular, err := geoLineConstruct(p, slope)
	if err != nil {
		return geoPoint{}, 0, err
	}
	closest, _, err := geoSegmentClosestLine(segment, perpendicular)
	if err != nil {
		return geoPoint{}, 0, err
	}
	distance, err := geoPointDistance(closest, p)
	return closest, distance, err
}

func geoSegmentClosestSegment(on, to geoSegment) (geoPoint, float64, error) {
	point, ok, err := geoSegmentIntersection(on, to)
	if err != nil {
		return geoPoint{}, 0, err
	}
	if ok {
		return point, 0, nil
	}
	closest, distance, err := geoSegmentClosestPoint(on, to.p0)
	if err != nil {
		return geoPoint{}, 0, err
	}
	point, d, err := geoSegmentClosestPoint(on, to.p1)
	if err != nil {
		return geoPoint{}, 0, err
	}
	if compareFloat(d, distance) < 0 {
		closest, distance = point, d
	}
	_, d, err = geoSegmentClosestPoint(to, on.p0)
	if err != nil {
		return geoPoint{}, 0, err
	}
	if compareFloat(d, distance) < 0 {
		closest, distance = on.p0, d
	}
	_, d, err = geoSegmentClosestPoint(to, on.p1)
	if err != nil {
		return geoPoint{}, 0, err
	}
	if compareFloat(d, distance) < 0 {
		closest, distance = on.p1, d
	}
	return closest, distance, nil
}

func geoLinearBoxEdges(box geoBox) [4]geoSegment {
	upperLeft := geoPoint{box.low.x, box.high.y}
	lowerRight := geoPoint{box.high.x, box.low.y}
	return [4]geoSegment{{box.low, upperLeft}, {box.high, upperLeft}, {box.low, lowerRight}, {box.high, lowerRight}}
}

// wantPoint preserves the extra arithmetic performed only for non-NULL C results.
func geoBoxSegmentIntersection(box geoBox, segment geoSegment, wantPoint bool) (geoPoint, bool, error) {
	if !geoBoxesOverlap(geoBoxFromPoints(segment.p0, segment.p1), box) {
		return geoPoint{}, false, nil
	}
	var point geoPoint
	if wantPoint {
		center, err := geoBoxCenter(box)
		if err != nil {
			return geoPoint{}, false, err
		}
		point, _, err = geoSegmentClosestPoint(segment, center)
		if err != nil {
			return geoPoint{}, false, err
		}
	}
	if geoBoxContainsPoint(box, segment.p0) || geoBoxContainsPoint(box, segment.p1) {
		return point, true, nil
	}
	for _, edge := range geoLinearBoxEdges(box) {
		_, ok, err := geoSegmentIntersection(edge, segment)
		if err != nil {
			return geoPoint{}, false, err
		}
		if ok {
			return point, true, nil
		}
	}
	return point, false, nil
}

func geoBoxClosestSegment(box geoBox, segment geoSegment, wantPoint bool) (geoPoint, float64, error) {
	point, ok, err := geoBoxSegmentIntersection(box, segment, wantPoint)
	if err != nil {
		return geoPoint{}, 0, err
	}
	if ok {
		return point, 0, nil
	}
	edges := geoLinearBoxEdges(box)
	closest, distance, err := geoSegmentClosestSegment(edges[0], segment)
	if err != nil {
		return geoPoint{}, 0, err
	}
	for _, edge := range edges[1:] {
		point, d, err := geoSegmentClosestSegment(edge, segment)
		if err != nil {
			return geoPoint{}, 0, err
		}
		if compareFloat(d, distance) < 0 {
			closest, distance = point, d
		}
	}
	return closest, distance, nil
}
