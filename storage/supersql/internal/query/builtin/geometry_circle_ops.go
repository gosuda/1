package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func geoCircleArea(circle geoCircle) (float64, error) {
	squared, err := geoMul(circle.radius, circle.radius)
	if err != nil {
		return 0, err
	}
	return geoMul(squared, math.Pi)
}

func geoCircleDistanceFromRadius(distance, radius float64) (float64, error) {
	result, err := geoSub(distance, radius)
	if err != nil {
		return 0, err
	}
	if result < 0 {
		result = 0
	}
	return result, nil
}

func geoCircleArithmetic(id model.FunctionID, circle geoCircle, point geoPoint) (geoCircle, error) {
	result := geoCircle{radius: circle.radius}
	var err error
	switch id {
	case 1146:
		result.center, err = geoPointAdd(circle.center, point)
	case 1147:
		result.center, err = geoPointSub(circle.center, point)
	case 1148:
		result.center, err = geoPointMul(circle.center, point)
	case 1149:
		result.center, err = geoPointDiv(circle.center, point)
	}
	if err != nil {
		return geoCircle{}, err
	}
	if id == 1148 || id == 1149 {
		magnitude, err := geoHypot(point.x, point.y)
		if err != nil {
			return geoCircle{}, err
		}
		if id == 1148 {
			result.radius, err = geoMul(circle.radius, magnitude)
		} else {
			result.radius, err = geoDiv(circle.radius, magnitude)
		}
		if err != nil {
			return geoCircle{}, err
		}
	}
	return result, nil
}

func geoCircleCompare(id model.FunctionID, left, right geoCircle) (bool, error) {
	if id >= 1462 && id <= 1467 {
		a, err := geoCircleArea(left)
		if err != nil {
			return false, err
		}
		b, err := geoCircleArea(right)
		if err != nil {
			return false, err
		}
		switch id {
		case 1462:
			return geoFPEq(a, b), nil
		case 1463:
			return geoFPNe(a, b), nil
		case 1464:
			return geoFPLt(a, b), nil
		case 1465:
			return geoFPGt(a, b), nil
		case 1466:
			return geoFPLe(a, b), nil
		case 1467:
			return geoFPGe(a, b), nil
		}
	}
	if id == 1453 || id == 1458 || id == 1459 {
		distance, err := geoPointDistance(left.center, right.center)
		if err != nil {
			return false, err
		}
		var radius float64
		switch id {
		case 1453:
			radius, err = geoSub(left.radius, right.radius)
		case 1458:
			radius, err = geoSub(right.radius, left.radius)
		case 1459:
			radius, err = geoAdd(left.radius, right.radius)
		}
		return geoFPLe(distance, radius), err
	}

	x, y := left.center.x, right.center.x
	if id == 1460 || id == 1461 || id == 2587 || id == 2588 {
		x, y = left.center.y, right.center.y
	}
	var a, b float64
	var err error
	switch id {
	case 1454, 1455, 1460, 2587:
		a, err = geoAdd(x, left.radius)
	case 1456, 1457, 1461, 2588:
		a, err = geoSub(x, left.radius)
	}
	if err != nil {
		return false, err
	}
	switch id {
	case 1454, 1456, 1460, 2588:
		b, err = geoSub(y, right.radius)
	case 1455, 1457, 1461, 2587:
		b, err = geoAdd(y, right.radius)
	}
	if err != nil {
		return false, err
	}
	switch id {
	case 1454, 1460:
		return geoFPLt(a, b), nil
	case 1455, 2587:
		return geoFPLe(a, b), nil
	case 1456, 2588:
		return geoFPGe(a, b), nil
	case 1457, 1461:
		return geoFPGt(a, b), nil
	default:
		panic("invalid circle comparison")
	}
}

func geoCircleBox(circle geoCircle) (geoBox, error) {
	delta, err := geoDiv(circle.radius, math.Sqrt(2))
	if err != nil {
		return geoBox{}, err
	}
	var box geoBox
	box.high.x, err = geoAdd(circle.center.x, delta)
	if err != nil {
		return geoBox{}, err
	}
	box.low.x, err = geoSub(circle.center.x, delta)
	if err != nil {
		return geoBox{}, err
	}
	box.high.y, err = geoAdd(circle.center.y, delta)
	if err != nil {
		return geoBox{}, err
	}
	box.low.y, err = geoSub(circle.center.y, delta)
	return box, err
}

func geoCirclePolygon(ctx context.Context, circle geoCircle, count int, budget CallBudget) (model.Value, error) {
	if geoFPZero(circle.radius) {
		return model.Value{}, sqlError("0A000", "cannot convert circle with radius zero to polygon")
	}
	if count < 2 {
		return model.Value{}, sqlError("22023", "must request at least 2 points")
	}
	// PostgreSQL's native polygon header is 40 bytes and its size uses int32.
	if uint64(count) > (math.MaxInt32-40)/16 {
		return model.Value{}, sqlError("54000", "too many points requested")
	}
	buffer, err := newGeoPolygon(ctx, count, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	step, err := geoDiv(2*math.Pi, float64(count))
	if err != nil {
		return model.Value{}, err
	}
	for i := range count {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		angle, err := geoMul(step, float64(i))
		if err != nil {
			return model.Value{}, err
		}
		sine, cosine := geoCircleSinCos(angle)
		x, err := geoMul(circle.radius, cosine)
		if err != nil {
			return model.Value{}, err
		}
		x, err = geoSub(circle.center.x, x)
		if err != nil {
			return model.Value{}, err
		}
		y, err := geoMul(circle.radius, sine)
		if err != nil {
			return model.Value{}, err
		}
		y, err = geoAdd(circle.center.y, y)
		if err != nil {
			return model.Value{}, err
		}
		geoWritePoint(buffer.data[4+i*16:], geoPoint{x: x, y: y})
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	return buffer.value(), nil
}

func geoPolygonCircle(ctx context.Context, polygon geoPolygon) (geoCircle, error) {
	var circle geoCircle
	var err error
	count := polygon.points.count()
	for i := range count {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return geoCircle{}, err
			}
		}
		circle.center, err = geoPointAdd(circle.center, polygon.points.at(i))
		if err != nil {
			return geoCircle{}, err
		}
	}
	circle.center.x, err = geoDiv(circle.center.x, float64(count))
	if err != nil {
		return geoCircle{}, err
	}
	circle.center.y, err = geoDiv(circle.center.y, float64(count))
	if err != nil {
		return geoCircle{}, err
	}
	for i := range count {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return geoCircle{}, err
			}
		}
		distance, err := geoPointDistance(polygon.points.at(i), circle.center)
		if err != nil {
			return geoCircle{}, err
		}
		circle.radius, err = geoAdd(circle.radius, distance)
		if err != nil {
			return geoCircle{}, err
		}
	}
	circle.radius, err = geoDiv(circle.radius, float64(count))
	if err != nil {
		return geoCircle{}, err
	}
	return circle, ctx.Err()
}
