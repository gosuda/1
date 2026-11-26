package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func geoPathEdge(path geoPath, i int) geoSegment {
	previous := i - 1
	if previous < 0 {
		previous = path.points.count() - 1
	}
	return geoSegment{path.points.at(previous), path.points.at(i)}
}

func geoPathsIntersect(ctx context.Context, a, b geoPath) (bool, error) {
	for i := range a.points.count() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if i == 0 && !a.closed {
			continue
		}
		for j := range b.points.count() {
			if j%256 == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			if j == 0 && !b.closed {
				continue
			}
			_, intersects, err := geoSegmentIntersection(geoPathEdge(a, i), geoPathEdge(b, j))
			if err != nil || intersects {
				return intersects, err
			}
		}
	}
	return false, nil
}

func geoPathsDistance(ctx context.Context, a, b geoPath) (model.Value, error) {
	result, haveMin := 0.0, false
	for i := range a.points.count() {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if i == 0 && !a.closed {
			continue
		}
		for j := range b.points.count() {
			if j%256 == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			if j == 0 && !b.closed {
				continue
			}
			_, distance, err := geoSegmentClosestSegment(geoPathEdge(a, i), geoPathEdge(b, j))
			if err != nil {
				return model.Value{}, err
			}
			if !haveMin || compareFloat(distance, result) < 0 {
				result, haveMin = distance, true
			}
		}
	}
	if !haveMin {
		return model.NullVal(model.TypeFloat64), nil
	}
	return model.Float64Val(result), nil
}

func geoPointPathDistance(ctx context.Context, p geoPoint, path geoPath) (float64, error) {
	result, haveMin := 0.0, false
	for i := range path.points.count() {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if i == 0 && !path.closed {
			continue
		}
		_, distance, err := geoSegmentClosestPoint(geoPathEdge(path, i), p)
		if err != nil {
			return 0, err
		}
		if !haveMin || compareFloat(distance, result) < 0 {
			result, haveMin = distance, true
		}
	}
	return result, nil
}

func geoPathContainsPoint(ctx context.Context, path geoPath, p geoPoint) (bool, error) {
	if path.closed {
		return geoPointInside(ctx, p, path.points)
	}
	a, err := geoPointDistance(p, path.points.at(0))
	if err != nil {
		return false, err
	}
	for i := range path.points.count() - 1 {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		b, err := geoPointDistance(p, path.points.at(i+1))
		if err != nil {
			return false, err
		}
		sum, err := geoAdd(a, b)
		if err != nil {
			return false, err
		}
		length, err := geoPointDistance(path.points.at(i), path.points.at(i+1))
		if err != nil {
			return false, err
		}
		if geoFPEq(sum, length) {
			return true, nil
		}
		a = b
	}
	return false, nil
}

func geoPathMeasure(ctx context.Context, path geoPath, area bool) (model.Value, error) {
	if area && !path.closed {
		return model.NullVal(model.TypeFloat64), nil
	}
	result := 0.0
	for i := range path.points.count() {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		if area {
			a, b := path.points.at(i), path.points.at((i+1)%path.points.count())
			term, err := geoMul(a.x, b.y)
			if err != nil {
				return model.Value{}, err
			}
			result, err = geoAdd(result, term)
			if err != nil {
				return model.Value{}, err
			}
			term, err = geoMul(a.y, b.x)
			if err != nil {
				return model.Value{}, err
			}
			result, err = geoSub(result, term)
			if err != nil {
				return model.Value{}, err
			}
		} else {
			if i == 0 && !path.closed {
				continue
			}
			edge := geoPathEdge(path, i)
			length, err := geoPointDistance(edge.p0, edge.p1)
			if err != nil {
				return model.Value{}, err
			}
			result, err = geoAdd(result, length)
			if err != nil {
				return model.Value{}, err
			}
		}
	}
	if area {
		var err error
		result, err = geoDiv(math.Abs(result), 2)
		if err != nil {
			return model.Value{}, err
		}
	}
	return model.Float64Val(result), nil
}

func geoCopyPoints(ctx context.Context, destination []byte, points geoPoints) error {
	for offset := 0; offset < len(points); offset += 4096 {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+4096, len(points))
		copy(destination[offset:end], points[offset:end])
	}
	return nil
}

func geoPathTransform(ctx context.Context, path geoPath, point geoPoint, id model.FunctionID, budget CallBudget) (model.Value, error) {
	buffer, err := newGeoPath(ctx, path.points.count(), path.closed, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	for i := range path.points.count() {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		p := path.points.at(i)
		switch id {
		case 1436:
			p, err = geoPointAdd(p, point)
		case 1437:
			p, err = geoPointSub(p, point)
		case 1438:
			p, err = geoPointMul(p, point)
		case 1439:
			p, err = geoPointDiv(p, point)
		}
		if err != nil {
			return model.Value{}, err
		}
		geoWritePoint(buffer.data[5+i*16:], p)
	}
	return buffer.value(), nil
}
