package builtin

import (
	"context"
	"iter"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// InRangeSupport identifies B-tree support procedure 3 for one ordering input
// and offset type. Entries are PostgreSQL 18.4 pg_amproc identities.
type InRangeSupport struct {
	Family   model.OID
	Input    model.TypeID
	Offset   model.TypeID
	Function model.FunctionID
}

var inRangeSupportCatalog = [...]InRangeSupport{
	{Family: 434, Input: 1082, Offset: 1186, Function: 4133},
	{Family: 434, Input: 1114, Offset: 1186, Function: 4134},
	{Family: 434, Input: 1184, Offset: 1186, Function: 4135},
	{Family: 1970, Input: 700, Offset: 701, Function: 4140},
	{Family: 1970, Input: 701, Offset: 701, Function: 4139},
	{Family: 1976, Input: 20, Offset: 20, Function: 4126},
	{Family: 1976, Input: 21, Offset: 20, Function: 4130},
	{Family: 1976, Input: 21, Offset: 21, Function: 4132},
	{Family: 1976, Input: 21, Offset: 23, Function: 4131},
	{Family: 1976, Input: 23, Offset: 20, Function: 4127},
	{Family: 1976, Input: 23, Offset: 21, Function: 4129},
	{Family: 1976, Input: 23, Offset: 23, Function: 4128},
	{Family: 1982, Input: 1186, Offset: 1186, Function: 4136},
	{Family: 1988, Input: 1700, Offset: 1700, Function: 4141},
	{Family: 1996, Input: 1083, Offset: 1186, Function: 4137},
	{Family: 2000, Input: 1266, Offset: 1186, Function: 4138},
}

func InRangeSupportFunctions(family model.OID, input model.TypeID) iter.Seq[InRangeSupport] {
	return func(yield func(InRangeSupport) bool) {
		for _, support := range inRangeSupportCatalog {
			if support.Family == family && support.Input == input && !yield(support) {
				return
			}
		}
	}
}

func LookupInRangeSupport(function model.FunctionID) (InRangeSupport, bool) {
	for _, support := range inRangeSupportCatalog {
		if support.Function == function {
			return support, true
		}
	}
	return InRangeSupport{}, false
}

func compileTemporalInRange(signature Signature, options ResolveOptions) (scalarKernel, uint64, error) {
	if signature.ID != 4135 {
		return temporalInRangeKernel(signature, namedTimeZone{}), 0, nil
	}
	zone, retained, err := retainTimeZoneContext(options.TimeZone)
	if err != nil {
		return nil, 0, err
	}
	return temporalInRangeKernel(signature, zone), retained, nil
}

func temporalInRangeKernel(signature Signature, zone namedTimeZone) scalarKernel {
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		offset := args[2].IntervalVal
		if err := ValidateTemporalInRangeOffset(signature.ID, offset); err != nil {
			return model.Value{}, err
		}
		subtract := args[3].IntVal != 0
		less := args[4].IntVal != 0
		switch signature.ID {
		case 4133:
			value, err := timestampFromDate(args[0].IntVal)
			if err != nil {
				return model.Value{}, err
			}
			base, err := timestampFromDate(args[1].IntVal)
			if err != nil {
				return model.Value{}, err
			}
			return timestampInRange(ctx, value, base, offset, subtract, less, namedTimeZone{}, false, budget)
		case 4134:
			return timestampInRange(ctx, args[0].IntVal, args[1].IntVal, offset, subtract, less, namedTimeZone{}, false, budget)
		case 4135:
			return timestampInRange(ctx, args[0].IntVal, args[1].IntVal, offset, subtract, less, zone, true, budget)
		case 4136:
			return intervalInRange(ctx, args[0].IntervalVal, args[1].IntervalVal, offset, subtract, less, budget)
		case 4137:
			base := args[1].IntVal
			if subtract {
				base -= offset.Microseconds
			} else if offset.Microseconds > 0 && base > math.MaxInt64-offset.Microseconds {
				return model.BoolVal(less), nil
			} else {
				base += offset.Microseconds
			}
			order := 0
			if args[0].IntVal < base {
				order = -1
			} else if args[0].IntVal > base {
				order = 1
			}
			return inRangeComparison(order, less), nil
		}
		return model.Value{}, &MissingImplementationError{Signature: signature}
	}
}

func ValidateTemporalInRangeOffset(function model.FunctionID, offset model.Interval) error {
	negative := false
	switch function {
	case 4137, 4138:
		negative = offset.Microseconds < 0
	case 4133, 4134, 4135, 4136:
		negative = compareInterval(offset, model.Interval{}) < 0
	default:
		return sqlError("42804", "function is not a temporal in_range support procedure")
	}
	if negative {
		return sqlError("22013", "invalid preceding or following size in window function")
	}
	return nil
}

func timestampInRange(ctx context.Context, value, base int64, offset model.Interval, subtract, less bool, zone namedTimeZone, zoned bool, budget CallBudget) (model.Value, error) {
	if intervalInfinity(offset) > 0 && (subtract && base == math.MaxInt64 || !subtract && base == math.MinInt64) {
		return model.BoolVal(true), nil
	}
	span := offset
	if subtract {
		arguments := [1]model.Value{intervalValue(offset)}
		negated, err := intervalNegate(ctx, arguments[:], budget)
		if err != nil {
			return model.Value{}, err
		}
		span = negated.IntervalVal
	}
	var sum model.Value
	var err error
	if zoned {
		sum, err = zone.addTimestampInterval(base, span)
	} else {
		sum, err = timestampAddInterval(base, span)
	}
	if err != nil {
		return model.Value{}, err
	}
	order := 0
	if value < sum.IntVal {
		order = -1
	} else if value > sum.IntVal {
		order = 1
	}
	return inRangeComparison(order, less), nil
}

func intervalInRange(ctx context.Context, value, base, offset model.Interval, subtract, less bool, budget CallBudget) (model.Value, error) {
	if intervalInfinity(offset) > 0 && (subtract && intervalInfinity(base) > 0 || !subtract && intervalInfinity(base) < 0) {
		return model.BoolVal(true), nil
	}
	arguments := [2]model.Value{intervalValue(base), intervalValue(offset)}
	var sum model.Value
	var err error
	if subtract {
		sum, err = intervalSubtract(ctx, arguments[:], budget)
	} else {
		sum, err = intervalAdd(ctx, arguments[:], budget)
	}
	if err != nil {
		return model.Value{}, err
	}
	return inRangeComparison(compareInterval(value, sum.IntervalVal), less), nil
}

func inRangeComparison(order int, less bool) model.Value {
	return model.BoolVal(less && order <= 0 || !less && order >= 0)
}
