package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileGeoLinear(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 225, 239, 277, 278, 361, 362, 363, 365, 366, 368, 369, 372, 373, 380, 381, 702, 704, 725, 727, 959, 960, 961, 981, 993, 994, 995, 996, 997, 998, 999,
		1408, 1409, 1410, 1411, 1412, 1413, 1414, 1415, 1482, 1483, 1484, 1485, 1486, 1487, 1488, 1489, 1492, 1493, 1494, 1495, 1496, 1497, 1498, 1499, 1530, 1532, 1541:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		var points [2]geoPoint
		var segments [2]geoSegment
		var lines [2]geoLine
		var boxes [2]geoBox
		for i, id := range signature.Arguments {
			var err error
			switch id {
			case 600:
				points[i], err = geoDecodePoint(args[i])
			case 601:
				segments[i], err = geoDecodeSegment(args[i])
			case 603:
				boxes[i], err = geoDecodeBox(args[i])
			case 628:
				lines[i], err = geoDecodeLine(args[i])
			}
			if err != nil {
				return model.Value{}, err
			}
		}
		id := signature.ID
		var point geoPoint
		var distance float64
		var ok bool
		var err error
		switch id {
		case 993:
			return geoSegmentValue(ctx, geoSegment{points[0], points[1]}, budget)
		case 1493:
			line, err := geoLineFromPoints(points[0], points[1])
			if err != nil {
				return model.Value{}, err
			}
			return geoLineValue(ctx, line, budget)
		case 981, 1541:
			return geoSegmentValue(ctx, geoSegment{boxes[0].high, boxes[0].low}, budget)
		case 225, 1532:
			point.x, err = geoAdd(segments[0].p0.x, segments[0].p1.x)
			if err != nil {
				return model.Value{}, err
			}
			point.x, err = geoDiv(point.x, 2)
			if err != nil {
				return model.Value{}, err
			}
			point.y, err = geoAdd(segments[0].p0.y, segments[0].p1.y)
			if err != nil {
				return model.Value{}, err
			}
			point.y, err = geoDiv(point.y, 2)
			if err != nil {
				return model.Value{}, err
			}
			return geoPointValue(ctx, point, budget)
		case 997, 1410:
			return model.BoolVal(geoFPEq(segments[0].p0.x, segments[0].p1.x)), nil
		case 998, 1411:
			return model.BoolVal(geoFPEq(segments[0].p0.y, segments[0].p1.y)), nil
		case 999, 1482:
			equal := geoPointSame(segments[0].p0, segments[1].p0) && geoPointSame(segments[0].p1, segments[1].p1)
			if id == 1482 {
				equal = !equal
			}
			return model.BoolVal(equal), nil
		case 995, 996, 1408, 1409:
			a, err := geoPointSlope(segments[0].p0, segments[0].p1)
			if err != nil {
				return model.Value{}, err
			}
			var b float64
			if id == 996 || id == 1409 {
				b, err = geoPointInverseSlope(segments[1].p0, segments[1].p1)
			} else {
				b, err = geoPointSlope(segments[1].p0, segments[1].p1)
			}
			return model.BoolVal(geoFPEq(a, b)), err
		case 1483, 1484, 1485, 1486:
			a, err := geoPointDistance(segments[0].p0, segments[0].p1)
			if err != nil {
				return model.Value{}, err
			}
			b, err := geoPointDistance(segments[1].p0, segments[1].p1)
			if err != nil {
				return model.Value{}, err
			}
			switch id {
			case 1483:
				ok = geoFPLt(a, b)
			case 1484:
				ok = geoFPLe(a, b)
			case 1485:
				ok = geoFPGt(a, b)
			case 1486:
				ok = geoFPGe(a, b)
			}
			return model.BoolVal(ok), nil
		case 1487, 1530:
			distance, err = geoPointDistance(segments[0].p0, segments[0].p1)
		case 1498, 1414:
			return model.BoolVal(geoFPZero(lines[0].b)), nil
		case 1499, 1415:
			return model.BoolVal(geoFPZero(lines[0].a)), nil
		case 1492:
			ok, err = geoLineEqual(lines[0], lines[1])
			return model.BoolVal(ok), err
		case 1497, 1413:
			ok, err = geoLinePerpendicular(lines[0], lines[1])
			return model.BoolVal(ok), err
		case 1495, 1496, 1412:
			_, ok, err = geoLineIntersection(lines[0], lines[1])
			if id != 1495 {
				ok = !ok
			}
			return model.BoolVal(ok), err
		case 239:
			distance, err = geoLineDistance(lines[0], lines[1])
		case 1494:
			point, ok, err = geoLineIntersection(lines[0], lines[1])
		case 362:
			point, ok, err = geoSegmentIntersection(segments[0], segments[1])
		case 994:
			_, ok, err = geoSegmentIntersection(segments[0], segments[1])
			return model.BoolVal(ok), err
		case 361:
			_, distance, err = geoSegmentClosestSegment(segments[0], segments[1])
		case 1489:
			a, err := geoPointSlope(segments[0].p0, segments[0].p1)
			if err != nil {
				return model.Value{}, err
			}
			b, err := geoPointSlope(segments[1].p0, segments[1].p1)
			if err != nil {
				return model.Value{}, err
			}
			if a == b {
				return model.NullVal(model.TypeBytes), nil
			}
			point, distance, err = geoSegmentClosestSegment(segments[1], segments[0])
			ok = !math.IsNaN(distance)
			if err != nil {
				return model.Value{}, err
			}
		case 363, 366:
			point, distance, err = geoSegmentClosestPoint(segments[1], points[0])
			ok = !math.IsNaN(distance)
		case 380:
			_, distance, err = geoSegmentClosestPoint(segments[0], points[1])
		case 369:
			ok, err = geoSegmentContainsPoint(segments[1], points[0])
			return model.BoolVal(ok), err
		case 725, 961:
			point, distance, err = geoLineClosestPoint(lines[1], points[0])
			ok = !math.IsNaN(distance)
		case 702:
			_, distance, err = geoLineClosestPoint(lines[0], points[1])
		case 959:
			ok, err = geoLineContainsPoint(lines[1], points[0])
			return model.BoolVal(ok), err
		case 727:
			_, distance, err = geoSegmentClosestLine(segments[0], lines[1])
		case 704:
			_, distance, err = geoSegmentClosestLine(segments[1], lines[0])
		case 1488:
			a, err := geoPointSlope(segments[1].p0, segments[1].p1)
			if err != nil {
				return model.Value{}, err
			}
			b, err := geoLineSlope(lines[0])
			if err != nil {
				return model.Value{}, err
			}
			if a == b {
				return model.NullVal(model.TypeBytes), nil
			}
			point, distance, err = geoSegmentClosestLine(segments[1], lines[0])
			ok = !math.IsNaN(distance)
			if err != nil {
				return model.Value{}, err
			}
		case 960:
			ok, err = geoLineContainsPoint(lines[1], segments[0].p0)
			if err != nil || !ok {
				return model.BoolVal(false), err
			}
			ok, err = geoLineContainsPoint(lines[1], segments[0].p1)
			return model.BoolVal(ok), err
		case 277:
			_, ok, err = geoSegmentLineIntersection(segments[0], lines[1])
			return model.BoolVal(ok), err
		case 372:
			return model.BoolVal(geoBoxContainsPoint(boxes[1], segments[0].p0) && geoBoxContainsPoint(boxes[1], segments[0].p1)), nil
		case 373:
			_, ok, err = geoBoxSegmentIntersection(boxes[1], segments[0], false)
			return model.BoolVal(ok), err
		case 365, 368:
			point, distance, err = geoBoxClosestSegment(boxes[1], segments[0], id == 368)
			ok = !math.IsNaN(distance)
		case 381:
			_, distance, err = geoBoxClosestSegment(boxes[0], segments[1], false)
		case 278:
			edges := geoLinearBoxEdges(boxes[1])
			// inter_lb visits the right edge before the bottom edge.
			edges[2], edges[3] = edges[3], edges[2]
			for _, edge := range edges {
				_, ok, err = geoSegmentLineIntersection(edge, lines[0])
				if err != nil || ok {
					return model.BoolVal(ok), err
				}
			}
			return model.BoolVal(false), nil
		}
		if err != nil {
			return model.Value{}, err
		}
		if signature.Result == 600 {
			if !ok {
				return model.NullVal(model.TypeBytes), nil
			}
			return geoPointValue(ctx, point, budget)
		}
		return model.Float64Val(distance), nil
	}
}
