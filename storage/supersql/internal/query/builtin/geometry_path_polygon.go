package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileGeoPathPolygon(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 137, 227, 339, 340, 341, 342, 343, 344, 345, 346, 370, 371, 421, 729,
		973, 979, 982, 983, 984, 985, 986, 987, 1426, 1428, 1429, 1430, 1431,
		1432, 1433, 1434, 1435, 1436, 1437, 1438, 1439, 1445, 1446, 1447,
		1448, 1449, 1531, 1540, 1545, 1556, 2566, 2567, 2568, 2569, 3275, 3292, 5011:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		id := signature.ID
		switch id {
		case 137, 371, 421, 1426:
			pathIndex := 0
			if id == 137 || id == 371 {
				pathIndex = 1
			}
			path, err := geoDecodePath(args[pathIndex])
			if err != nil {
				return model.Value{}, err
			}
			point, err := geoDecodePoint(args[1-pathIndex])
			if err != nil {
				return model.Value{}, err
			}
			if id == 137 || id == 1426 {
				result, err := geoPathContainsPoint(ctx, path, point)
				return model.BoolVal(result), err
			}
			result, err := geoPointPathDistance(ctx, point, path)
			return model.Float64Val(result), err
		case 1428, 1429, 3275, 3292:
			polyIndex := 0
			if id == 1429 || id == 3275 {
				polyIndex = 1
			}
			polygon, err := geoDecodePolygon(args[polyIndex])
			if err != nil {
				return model.Value{}, err
			}
			point, err := geoDecodePoint(args[1-polyIndex])
			if err != nil {
				return model.Value{}, err
			}
			if id == 1428 || id == 1429 {
				result, err := geoPointInside(ctx, point, polygon.points)
				return model.BoolVal(result), err
			}
			result, err := geoPointPolygonDistance(ctx, point, polygon)
			return model.Float64Val(result), err
		case 1448:
			box, err := geoDecodeBox(args[0])
			if err != nil {
				return model.Value{}, err
			}
			buffer, err := newGeoPolygon(ctx, 4, budget)
			if err != nil {
				return model.Value{}, err
			}
			defer buffer.close()
			geoWritePoint(buffer.data[4:], box.low)
			geoWritePoint(buffer.data[20:], geoPoint{box.low.x, box.high.y})
			geoWritePoint(buffer.data[36:], box.high)
			geoWritePoint(buffer.data[52:], geoPoint{box.high.x, box.low.y})
			return buffer.value(), nil
		}
		if signature.Arguments[0] == 602 {
			path, err := geoDecodePath(args[0])
			if err != nil {
				return model.Value{}, err
			}
			return geoPathOperation(ctx, id, path, args, budget)
		}
		polygon, err := geoDecodePolygon(args[0])
		if err != nil {
			return model.Value{}, err
		}
		return geoPolygonOperation(ctx, id, polygon, args, budget)
	}
}

func geoPathOperation(ctx context.Context, id model.FunctionID, path geoPath, args []model.Value, budget CallBudget) (model.Value, error) {
	switch id {
	case 979, 987, 1531:
		return geoPathMeasure(ctx, path, id == 979)
	case 1430:
		return model.BoolVal(path.closed), nil
	case 1431:
		return model.BoolVal(!path.closed), nil
	case 1432, 1545:
		return model.Int64Val(int64(path.points.count())), nil
	case 1433, 1434:
		buffer, err := newGeoPath(ctx, path.points.count(), id == 1433, budget)
		if err != nil {
			return model.Value{}, err
		}
		defer buffer.close()
		if err := geoCopyPoints(ctx, buffer.data[5:], path.points); err != nil {
			return model.Value{}, err
		}
		return buffer.value(), nil
	case 1449:
		if !path.closed {
			return model.Value{}, sqlError("22023", "open path cannot be converted to polygon")
		}
		buffer, err := newGeoPolygon(ctx, path.points.count(), budget)
		if err != nil {
			return model.Value{}, err
		}
		defer buffer.close()
		if err := geoCopyPoints(ctx, buffer.data[4:], path.points); err != nil {
			return model.Value{}, err
		}
		return buffer.value(), nil
	case 1436, 1437, 1438, 1439:
		point, err := geoDecodePoint(args[1])
		if err != nil {
			return model.Value{}, err
		}
		return geoPathTransform(ctx, path, point, id, budget)
	}
	other, err := geoDecodePath(args[1])
	if err != nil {
		return model.Value{}, err
	}
	switch id {
	case 370:
		return geoPathsDistance(ctx, path, other)
	case 973:
		a, err := geoPolygonBounds(ctx, geoPolygon{path.points})
		if err != nil {
			return model.Value{}, err
		}
		b, err := geoPolygonBounds(ctx, geoPolygon{other.points})
		if err != nil {
			return model.Value{}, err
		}
		if !geoBoxesOverlap(a, b) {
			return model.BoolVal(false), nil
		}
		result, err := geoPathsIntersect(ctx, path, other)
		return model.BoolVal(result), err
	case 982:
		return model.BoolVal(path.points.count() < other.points.count()), nil
	case 983:
		return model.BoolVal(path.points.count() > other.points.count()), nil
	case 984:
		return model.BoolVal(path.points.count() == other.points.count()), nil
	case 985:
		return model.BoolVal(path.points.count() <= other.points.count()), nil
	case 986:
		return model.BoolVal(path.points.count() >= other.points.count()), nil
	case 1435:
		if path.closed || other.closed {
			return model.NullVal(model.TypeBytes), nil
		}
		buffer, err := newGeoPath(ctx, path.points.count()+other.points.count(), false, budget)
		if err != nil {
			return model.Value{}, err
		}
		defer buffer.close()
		if err := geoCopyPoints(ctx, buffer.data[5:], path.points); err != nil {
			return model.Value{}, err
		}
		if err := geoCopyPoints(ctx, buffer.data[5+len(path.points):], other.points); err != nil {
			return model.Value{}, err
		}
		return buffer.value(), nil
	}
	return model.Value{}, sqlError("XX000", "unhandled path operation")
}

func geoPolygonOperation(ctx context.Context, id model.FunctionID, polygon geoPolygon, args []model.Value, budget CallBudget) (model.Value, error) {
	switch id {
	case 227, 1540:
		circle, err := geoPolygonCircle(ctx, polygon)
		if err != nil {
			return model.Value{}, err
		}
		return geoPointValue(ctx, circle.center, budget)
	case 1445, 1556:
		return model.Int64Val(int64(polygon.points.count())), nil
	case 1446, 5011:
		box, err := geoPolygonBounds(ctx, polygon)
		if err != nil {
			return model.Value{}, err
		}
		return geoBoxValue(ctx, box, budget)
	case 1447:
		buffer, err := newGeoPath(ctx, polygon.points.count(), true, budget)
		if err != nil {
			return model.Value{}, err
		}
		defer buffer.close()
		if err := geoCopyPoints(ctx, buffer.data[5:], polygon.points); err != nil {
			return model.Value{}, err
		}
		return buffer.value(), nil
	}
	other, err := geoDecodePolygon(args[1])
	if err != nil {
		return model.Value{}, err
	}
	if id == 339 {
		result, err := geoPolygonSame(ctx, polygon, other)
		return model.BoolVal(result), err
	}
	a, err := geoPolygonBounds(ctx, polygon)
	if err != nil {
		return model.Value{}, err
	}
	b, err := geoPolygonBounds(ctx, other)
	if err != nil {
		return model.Value{}, err
	}
	switch id {
	case 341:
		return model.BoolVal(a.high.x < b.low.x), nil
	case 342:
		return model.BoolVal(a.high.x <= b.high.x), nil
	case 343:
		return model.BoolVal(a.low.x >= b.low.x), nil
	case 344:
		return model.BoolVal(a.low.x > b.high.x), nil
	case 2566:
		return model.BoolVal(a.high.y < b.low.y), nil
	case 2567:
		return model.BoolVal(a.high.y <= b.high.y), nil
	case 2568:
		return model.BoolVal(a.low.y >= b.low.y), nil
	case 2569:
		return model.BoolVal(a.low.y > b.high.y), nil
	case 340, 345:
		if id == 345 {
			polygon, other, a, b = other, polygon, b, a
		}
		result, err := geoPolygonContains(ctx, polygon, other, a, b)
		return model.BoolVal(result), err
	case 346, 729:
		overlaps, err := geoPolygonOverlap(ctx, polygon, other, a, b)
		if err != nil {
			return model.Value{}, err
		}
		if id == 346 {
			return model.BoolVal(overlaps), nil
		}
		if overlaps {
			return model.Float64Val(0), nil
		}
		return geoPathsDistance(ctx, geoPath{polygon.points, true}, geoPath{other.points, true})
	}
	return model.Value{}, sqlError("XX000", "unhandled polygon operation")
}
