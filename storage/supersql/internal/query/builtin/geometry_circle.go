package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileGeoCircle(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 728, 785, 1146, 1147, 1148, 1149, 1416,
		1452, 1453, 1454, 1455, 1456, 1457, 1458, 1459, 1460, 1461,
		1462, 1463, 1464, 1465, 1466, 1467, 1468, 1469, 1470, 1471,
		1472, 1473, 1474, 1475, 1476, 1477, 1478, 1479, 1480, 1543, 1544,
		2587, 2588, 3290:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		switch signature.ID {
		case 1473:
			center, err := geoDecodePoint(args[0])
			if err != nil {
				return model.Value{}, err
			}
			return geoCircleValue(ctx, geoCircle{center: center, radius: args[1].FloatVal}, budget)
		case 1474:
			polygon, err := geoDecodePolygon(args[0])
			if err != nil {
				return model.Value{}, err
			}
			circle, err := geoPolygonCircle(ctx, polygon)
			if err != nil {
				return model.Value{}, err
			}
			return geoCircleValue(ctx, circle, budget)
		case 1479:
			box, err := geoDecodeBox(args[0])
			if err != nil {
				return model.Value{}, err
			}
			center, err := geoBoxCenter(box)
			if err != nil {
				return model.Value{}, err
			}
			radius, err := geoPointDistance(center, box.high)
			if err != nil {
				return model.Value{}, err
			}
			return geoCircleValue(ctx, geoCircle{center: center, radius: radius}, budget)
		case 1475:
			circle, err := geoDecodeCircle(args[1])
			if err != nil {
				return model.Value{}, err
			}
			return geoCirclePolygon(ctx, circle, int(args[0].IntVal), budget)
		case 728, 785:
			circleIndex, polygonIndex := 0, 1
			if signature.ID == 785 {
				circleIndex, polygonIndex = 1, 0
			}
			circle, err := geoDecodeCircle(args[circleIndex])
			if err != nil {
				return model.Value{}, err
			}
			polygon, err := geoDecodePolygon(args[polygonIndex])
			if err != nil {
				return model.Value{}, err
			}
			distance, err := geoPointPolygonDistance(ctx, circle.center, polygon)
			if err != nil {
				return model.Value{}, err
			}
			distance, err = geoCircleDistanceFromRadius(distance, circle.radius)
			return model.Float64Val(distance), err
		case 1476, 1478:
			point, err := geoDecodePoint(args[0])
			if err != nil {
				return model.Value{}, err
			}
			circle, err := geoDecodeCircle(args[1])
			if err != nil {
				return model.Value{}, err
			}
			if signature.ID == 1478 {
				distance, err := geoPointDistance(circle.center, point)
				return model.BoolVal(distance <= circle.radius), err
			}
			distance, err := geoPointDistance(point, circle.center)
			if err != nil {
				return model.Value{}, err
			}
			distance, err = geoCircleDistanceFromRadius(distance, circle.radius)
			return model.Float64Val(distance), err
		}

		circle, err := geoDecodeCircle(args[0])
		if err != nil {
			return model.Value{}, err
		}
		switch signature.ID {
		case 1416, 1472, 1543:
			return geoPointValue(ctx, circle.center, budget)
		case 1468:
			area, err := geoCircleArea(circle)
			return model.Float64Val(area), err
		case 1469:
			diameter, err := geoMul(circle.radius, 2)
			return model.Float64Val(diameter), err
		case 1470:
			return model.Float64Val(circle.radius), nil
		case 1480:
			box, err := geoCircleBox(circle)
			if err != nil {
				return model.Value{}, err
			}
			return geoBoxValue(ctx, box, budget)
		case 1544:
			return geoCirclePolygon(ctx, circle, 12, budget)
		case 1146, 1147, 1148, 1149:
			point, err := geoDecodePoint(args[1])
			if err != nil {
				return model.Value{}, err
			}
			result, err := geoCircleArithmetic(signature.ID, circle, point)
			if err != nil {
				return model.Value{}, err
			}
			return geoCircleValue(ctx, result, budget)
		case 1477, 3290:
			point, err := geoDecodePoint(args[1])
			if err != nil {
				return model.Value{}, err
			}
			if signature.ID == 1477 {
				distance, err := geoPointDistance(circle.center, point)
				return model.BoolVal(distance <= circle.radius), err
			}
			distance, err := geoPointDistance(point, circle.center)
			if err != nil {
				return model.Value{}, err
			}
			distance, err = geoCircleDistanceFromRadius(distance, circle.radius)
			return model.Float64Val(distance), err
		}

		right, err := geoDecodeCircle(args[1])
		if err != nil {
			return model.Value{}, err
		}
		if signature.ID == 1471 {
			distance, err := geoPointDistance(circle.center, right.center)
			if err != nil {
				return model.Value{}, err
			}
			radii, err := geoAdd(circle.radius, right.radius)
			if err != nil {
				return model.Value{}, err
			}
			distance, err = geoCircleDistanceFromRadius(distance, radii)
			return model.Float64Val(distance), err
		}
		if signature.ID == 1452 {
			return model.BoolVal(((math.IsNaN(circle.radius) && math.IsNaN(right.radius)) || geoFPEq(circle.radius, right.radius)) && geoPointSame(circle.center, right.center)), nil
		}
		result, err := geoCircleCompare(signature.ID, circle, right)
		return model.BoolVal(result), err
	}
}
