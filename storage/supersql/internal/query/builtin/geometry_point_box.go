package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileGeoPointBox(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 115, 116, 125, 126, 127, 128, 129, 130, 131, 132, 133, 134, 135, 136,
		138, 186, 187, 188, 189, 190, 191, 192, 193, 357, 364, 367, 975, 976, 977,
		978, 980, 988, 989, 990, 991, 992, 1406, 1407, 1421, 1422, 1423, 1424,
		1425, 1440, 1441, 1442, 1443, 1444, 1534, 1542, 2562, 2563, 2564, 2565,
		4067, 4091:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		id := signature.ID
		if id == 1440 {
			return geoPointValue(ctx, geoPoint{args[0].FloatVal, args[1].FloatVal}, budget)
		}
		if id == 136 || id == 193 || id == 357 || id == 364 || id == 367 {
			pointIndex, boxIndex := 0, 1
			if id == 193 || id == 357 {
				pointIndex, boxIndex = 1, 0
			}
			p, err := geoDecodePoint(args[pointIndex])
			if err != nil {
				return model.Value{}, err
			}
			box, err := geoDecodeBox(args[boxIndex])
			if err != nil {
				return model.Value{}, err
			}
			if id == 136 || id == 193 {
				return model.BoolVal(geoBoxContainsPoint(box, p)), nil
			}
			closest, distance, err := geoClosestPointBox(box, p)
			if err != nil {
				return model.Value{}, err
			}
			if id != 367 {
				return model.Float64Val(distance), nil
			}
			if math.IsNaN(distance) {
				return model.NullVal(model.TypeBytes), nil
			}
			return geoPointValue(ctx, closest, budget)
		}
		if signature.Arguments[0] == 600 {
			a, err := geoDecodePoint(args[0])
			if err != nil {
				return model.Value{}, err
			}
			if id == 4091 {
				return geoBoxValue(ctx, geoBox{high: a, low: a}, budget)
			}
			b, err := geoDecodePoint(args[1])
			if err != nil {
				return model.Value{}, err
			}
			switch id {
			case 131:
				return model.BoolVal(geoFPGt(a.y, b.y)), nil
			case 132:
				return model.BoolVal(geoFPLt(a.x, b.x)), nil
			case 133:
				return model.BoolVal(geoFPGt(a.x, b.x)), nil
			case 134:
				return model.BoolVal(geoFPLt(a.y, b.y)), nil
			case 135:
				return model.BoolVal(geoPointSame(a, b)), nil
			case 988:
				return model.BoolVal(!geoPointSame(a, b)), nil
			case 989, 1406:
				return model.BoolVal(geoFPEq(a.x, b.x)), nil
			case 990, 1407:
				return model.BoolVal(geoFPEq(a.y, b.y)), nil
			case 991:
				distance, err := geoPointDistance(a, b)
				return model.Float64Val(distance), err
			case 992:
				slope, err := geoPointSlope(a, b)
				return model.Float64Val(slope), err
			case 1421:
				return geoBoxValue(ctx, geoBoxFromPoints(a, b), budget)
			case 1441, 1442, 1443, 1444:
				var result geoPoint
				switch id {
				case 1441:
					result, err = geoPointAdd(a, b)
				case 1442:
					result, err = geoPointSub(a, b)
				case 1443:
					result, err = geoPointMul(a, b)
				case 1444:
					result, err = geoPointDiv(a, b)
				}
				if err != nil {
					return model.Value{}, err
				}
				return geoPointValue(ctx, result, budget)
			}
		}
		a, err := geoDecodeBox(args[0])
		if err != nil {
			return model.Value{}, err
		}
		switch id {
		case 138, 1534, 1542:
			center, err := geoBoxCenter(a)
			if err != nil {
				return model.Value{}, err
			}
			return geoPointValue(ctx, center, budget)
		case 975:
			area, err := geoBoxArea(a)
			return model.Float64Val(area), err
		case 976:
			width, err := geoSub(a.high.x, a.low.x)
			return model.Float64Val(width), err
		case 977:
			height, err := geoSub(a.high.y, a.low.y)
			return model.Float64Val(height), err
		case 1422, 1423, 1424, 1425:
			p, err := geoDecodePoint(args[1])
			if err != nil {
				return model.Value{}, err
			}
			operation := geoPointAdd
			switch id {
			case 1423:
				operation = geoPointSub
			case 1424:
				operation = geoPointMul
			case 1425:
				operation = geoPointDiv
			}
			high, err := operation(a.high, p)
			if err != nil {
				return model.Value{}, err
			}
			low, err := operation(a.low, p)
			if err != nil {
				return model.Value{}, err
			}
			result := geoBox{high: high, low: low}
			if id == 1424 || id == 1425 {
				result = geoBoxFromPoints(high, low)
			}
			return geoBoxValue(ctx, result, budget)
		}
		b, err := geoDecodeBox(args[1])
		if err != nil {
			return model.Value{}, err
		}
		switch id {
		case 115:
			return model.BoolVal(geoFPGe(a.low.y, b.high.y)), nil
		case 116:
			return model.BoolVal(geoFPLe(a.high.y, b.low.y)), nil
		case 125:
			return model.BoolVal(geoBoxesOverlap(a, b)), nil
		case 126, 127, 128, 129, 130:
			areaA, err := geoBoxArea(a)
			if err != nil {
				return model.Value{}, err
			}
			areaB, err := geoBoxArea(b)
			if err != nil {
				return model.Value{}, err
			}
			var result bool
			switch id {
			case 126:
				result = geoFPGe(areaA, areaB)
			case 127:
				result = geoFPGt(areaA, areaB)
			case 128:
				result = geoFPEq(areaA, areaB)
			case 129:
				result = geoFPLt(areaA, areaB)
			case 130:
				result = geoFPLe(areaA, areaB)
			}
			return model.BoolVal(result), nil
		case 186:
			return model.BoolVal(geoPointSame(a.high, b.high) && geoPointSame(a.low, b.low)), nil
		case 187:
			return model.BoolVal(geoBoxContainsBox(a, b)), nil
		case 188:
			return model.BoolVal(geoFPLt(a.high.x, b.low.x)), nil
		case 189:
			return model.BoolVal(geoFPLe(a.high.x, b.high.x)), nil
		case 190:
			return model.BoolVal(geoFPGe(a.low.x, b.low.x)), nil
		case 191:
			return model.BoolVal(geoFPGt(a.low.x, b.high.x)), nil
		case 192:
			return model.BoolVal(geoBoxContainsBox(b, a)), nil
		case 978:
			centerA, err := geoBoxCenter(a)
			if err != nil {
				return model.Value{}, err
			}
			centerB, err := geoBoxCenter(b)
			if err != nil {
				return model.Value{}, err
			}
			distance, err := geoPointDistance(centerA, centerB)
			return model.Float64Val(distance), err
		case 980:
			if !geoBoxesOverlap(a, b) {
				return model.NullVal(model.TypeBytes), nil
			}
			return geoBoxValue(ctx, geoBox{
				high: geoPoint{geoBoxFloatMin(a.high.x, b.high.x), geoBoxFloatMin(a.high.y, b.high.y)},
				low:  geoPoint{geoBoxFloatMax(a.low.x, b.low.x), geoBoxFloatMax(a.low.y, b.low.y)},
			}, budget)
		case 2562:
			return model.BoolVal(geoFPLt(a.high.y, b.low.y)), nil
		case 2563:
			return model.BoolVal(geoFPLe(a.high.y, b.high.y)), nil
		case 2564:
			return model.BoolVal(geoFPGe(a.low.y, b.low.y)), nil
		case 2565:
			return model.BoolVal(geoFPGt(a.low.y, b.high.y)), nil
		case 4067:
			return geoBoxValue(ctx, geoBox{
				high: geoPoint{geoBoxFloatMax(a.high.x, b.high.x), geoBoxFloatMax(a.high.y, b.high.y)},
				low:  geoPoint{geoBoxFloatMin(a.low.x, b.low.x), geoBoxFloatMin(a.low.y, b.low.y)},
			}, budget)
		}
		return model.Value{}, sqlError("XX000", "unhandled point or box operation")
	}
}
