package builtin

import (
	"context"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileNativeTemporal(signature Signature, zone namedTimeZone) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		switch signature.ID {
		case 2071, 2072, 2546:
			dateIndex := 0
			if signature.ID == 2546 { dateIndex = 1 }
			value, err := timestampFromDate(arguments[dateIndex].IntVal)
			if err != nil { return model.Value{}, err }
			span := arguments[1-dateIndex].IntervalVal
			if signature.ID == 2072 {
				negated, err := intervalNegate(ctx, []model.Value{intervalValue(span)}, budget)
				if err != nil { return model.Value{}, err }
				span = negated.IntervalVal
			}
			return timestampAddInterval(value, span)
		}
		left, err := nativeComparableTimestamp(signature.Arguments[0], arguments[0].IntVal, zone, signature.Arguments[1] == 1184)
		if err != nil { return model.Value{}, err }
		right, err := nativeComparableTimestamp(signature.Arguments[1], arguments[1].IntVal, zone, signature.Arguments[0] == 1184)
		if err != nil { return model.Value{}, err }
		order := intCompare(left, right)
		position := nativeTemporalComparisonPosition(signature.ID)
		switch position {
		case 0: return model.BoolVal(order < 0), nil
		case 1: return model.BoolVal(order <= 0), nil
		case 2: return model.BoolVal(order == 0), nil
		case 3: return model.BoolVal(order > 0), nil
		case 4: return model.BoolVal(order >= 0), nil
		case 5: return model.BoolVal(order != 0), nil
		default: return model.Int64Val(int64(order)), nil
		}
	}
}

func nativeComparableTimestamp(typ model.TypeID, value int64, zone namedTimeZone, zonedPair bool) (int64, error) {
	if typ == 1082 {
		converted, err := timestampFromDate(value)
		if err != nil { return 0, err }
		value = converted
	}
	if zonedPair && typ != 1184 {
		converted, err := zone.convertTimestamp(value, false)
		if err != nil { return 0, err }
		value = converted.IntVal
	}
	return value, nil
}

func nativeTemporalComparisonPosition(id model.FunctionID) int {
	switch {
	case id >= 2338 && id <= 2344: return int(id - 2338)
	case id >= 2351 && id <= 2357: return int(id - 2351)
	case id >= 2364 && id <= 2370: return int(id - 2364)
	case id >= 2377 && id <= 2383: return int(id - 2377)
	case id >= 2520 && id <= 2526: return int(id - 2520)
	case id >= 2527 && id <= 2533: return int(id - 2527)
	default: return -1
	}
}

func compileNativeInRange(signature Signature) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (result model.Value, err error) {
		subtract, less := arguments[3].IntVal != 0, arguments[4].IntVal != 0
		if signature.ID == 4139 || signature.ID == 4140 {
			offset := arguments[2].FloatVal
			if math.IsNaN(offset) || offset < 0 { return model.Value{}, sqlError("22013", "invalid preceding or following size in window function") }
			base := arguments[1].FloatVal
			if subtract { base -= offset } else { base += offset }
			return inRangeComparison(compareFloat(arguments[0].FloatVal, base), less), nil
		}
		if signature.ID == 4141 {
			work, err := budget.NewWorkspace(ctx)
			if err != nil { return model.Value{}, numericAllocationError(err) }
			defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
			value, err := parseNumeric(ctx, work, arguments[0].StrVal); if err != nil { return model.Value{}, err }
			base, err := parseNumeric(ctx, work, arguments[1].StrVal); if err != nil { return model.Value{}, err }
			offset, err := parseNumeric(ctx, work, arguments[2].StrVal); if err != nil { return model.Value{}, err }
			zero := decimal{}
			order, err := numericCompare(ctx, offset, zero); if err != nil { return model.Value{}, err }
			if order < 0 { return model.Value{}, sqlError("22013", "invalid preceding or following size in window function") }
			op := opPl; if subtract { op = opMi }
			base, err = numericArithmetic(ctx, work, base, offset, op); if err != nil { return model.Value{}, err }
			order, err = numericCompare(ctx, value, base); if err != nil { return model.Value{}, err }
			return inRangeComparison(order, less), nil
		}
		offset := arguments[2].IntVal
		if offset < 0 { return model.Value{}, sqlError("22013", "invalid preceding or following size in window function") }
		base := arguments[1].IntVal
		if subtract {
			if base < math.MinInt64+offset { return model.BoolVal(!less), nil }
			base -= offset
		} else {
			if base > math.MaxInt64-offset { return model.BoolVal(less), nil }
			base += offset
		}
		return inRangeComparison(intCompare(arguments[0].IntVal, base), less), nil
	}
}
