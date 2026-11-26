package builtin

import (
	"cmp"
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimetz(signature Signature, options ResolveOptions) (scalarKernel, uint64, error) {
	switch signature.ID {
	case 1271:
		return timetzOverlaps, 0, nil
	case 1273, 6201:
		return compileTimetzFields(signature.ID == 6201), 0, nil
	case 1297, 1298, 1351, 1359, 1696, 1749, 1750, 1969, 2038, 2046, 2547, 3410, 4138:
		return compileTimetzValue(signature), 0, nil
	case 1350, 1386, 1388, 2037, 2047, 2059, 6336:
	default:
		return nil, 0, nil
	}
	zone, borrowed, err := resolveTimeZoneContext(options.TimeZone)
	if err != nil {
		return nil, 0, err
	}
	retained := uint64(unsafe.Sizeof(zone))
	if borrowed {
		zone.rule.StandardName = cloneTimezoneToken(zone.rule.StandardName)
		zone.rule.DaylightName = cloneTimezoneToken(zone.rule.DaylightName)
		retained += uint64(len(zone.rule.StandardName) + len(zone.rule.DaylightName))
	}
	return compileTimetzRuntime(signature, options, zone), retained, nil
}

func compileTimetzValue(signature Signature) scalarKernel {
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		index := 0
		switch signature.ID {
		case 1297, 1359, 2038, 2547:
			index = 1
		}
		micros, west, err := DecodeTimetz(args[index].BytesVal)
		if err != nil {
			return model.Value{}, err
		}
		switch signature.ID {
		case 1351:
			fields := intervalOutputFields{hour: micros / intervalMicrosPerHour, minute: micros / intervalMicrosPerMinute % 60, second: micros / intervalMicrosPerSecond % 60, microsecond: micros % intervalMicrosPerSecond}
			var scratch [64]byte
			output := appendIntervalOutputClock(scratch[:0], fields, true)
			output = appendTimetzOffset(output, west)
			if err := outputBudget(int64(len(output)), budget.OutputBytes); err != nil {
				return model.Value{}, err
			}
			if uint64(len(output)) > budget.MemoryBytes {
				return model.Value{}, sqlError("54000", "builtin invocation memory budget exceeded")
			}
			if budget.Allocator != nil {
				claim, err := budget.Allocator.Reserve(uint64(len(output)))
				if err != nil {
					return model.Value{}, err
				}
				defer claim.Release()
			}
			return model.StringVal(string(output)), nil
		case 2046:
			return timeValue(micros), nil
		case 1969:
			return timetzResult(ctx, roundTimePrecision(micros, args[1].IntVal), west, budget)
		case 1696, 3410:
			folded := uint32(micros) ^ uint32(uint64(micros)>>32)
			if signature.ID == 1696 {
				return model.Int64Val(int64(int32(pgHashUint32(folded) ^ pgHashUint32(uint32(west))))), nil
			}
			seed := uint64(args[1].IntVal)
			return model.Int64Val(int64(pgHashUint32Extended(folded, seed) ^ pgHashUint32Extended(uint32(west), seed))), nil
		case 1749, 1750, 2547:
			span := args[1-index].IntervalVal
			result, err := addTimeInterval(micros, span, signature.ID == 1750)
			if err != nil {
				return model.Value{}, err
			}
			return timetzResult(ctx, result.IntVal, west, budget)
		case 1297, 1298, 1359:
			base, err := timestampFromDate(args[1-index].IntVal)
			if err != nil {
				return model.Value{}, err
			}
			if !finiteTimestamp(base) {
				return timestampValue(base), nil
			}
			result := base + micros + int64(west)*intervalMicrosPerSecond
			if !validTimestamp(result) {
				return model.Value{}, sqlError("22008", "date out of range for timestamp")
			}
			return timestampValue(result), nil
		case 2038:
			span := args[0].IntervalVal
			if intervalInfinity(span) != 0 {
				return model.Value{}, sqlError("22023", "interval time zone must be finite")
			}
			if span.Months != 0 || span.Days != 0 {
				return model.Value{}, sqlError("22023", "interval time zone must not include months or days")
			}
			newWest := int32(-(span.Microseconds / intervalMicrosPerSecond))
			return timetzAtOffset(ctx, micros, west, newWest, budget)
		case 4138:
			base, baseWest, err := DecodeTimetz(args[1].BytesVal)
			if err != nil {
				return model.Value{}, err
			}
			offset := args[2].IntervalVal.Microseconds
			if offset < 0 {
				return model.Value{}, sqlError("22013", "invalid preceding or following size in window function")
			}
			less := args[4].IntVal != 0
			if args[3].IntVal != 0 {
				base -= offset
			} else {
				if offset > math.MaxInt64-base {
					return model.BoolVal(less), nil
				}
				base += offset
			}
			order := cmp.Or(cmp.Compare(micros+int64(west)*intervalMicrosPerSecond, base+int64(baseWest)*intervalMicrosPerSecond), cmp.Compare(west, baseWest))
			return model.BoolVal(less && order <= 0 || !less && order >= 0), nil
		}
		return model.Value{}, &MissingImplementationError{Signature: signature}
	}
}

func timetzAtOffset(ctx context.Context, micros int64, west, newWest int32, budget CallBudget) (model.Value, error) {
	delta := int32(uint32(west) - uint32(newWest))
	micros = (micros + int64(delta)*intervalMicrosPerSecond) % intervalMicrosPerDay
	if micros < 0 {
		micros += intervalMicrosPerDay
	}
	return timetzResult(ctx, micros, newWest, budget)
}

func appendTimetzOffset(output []byte, west int32) []byte {
	// PostgreSQL retains int32 overflow in abs(zone), then prints unsigned fields.
	seconds := west
	sign := byte('+')
	if west > 0 {
		sign = '-'
	}
	if seconds < 0 {
		seconds = -seconds
	}
	output = append(output, sign)
	output = appendIntervalOutputMagnitude(output, int64(uint32(seconds/3600)), true)
	if seconds%3600 != 0 {
		output = append(output, ':')
		output = appendIntervalOutputMagnitude(output, int64(uint32(seconds/60%60)), true)
		if seconds%60 != 0 {
			output = append(output, ':')
			output = appendIntervalOutputMagnitude(output, int64(uint32(seconds%60)), true)
		}
	}
	return output
}

func timetzOverlaps(ctx context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	// Rank endpoints using the UTC-then-zone comparison, not wire byte order.
	var endpoints [4]temporalEndpoint
	var micros [4]int64
	var zones [4]int32
	for i, arg := range args {
		endpoints[i].null = arg.IsNull
		if !arg.IsNull {
			var err error
			micros[i], zones[i], err = DecodeTimetz(arg.BytesVal)
			if err != nil {
				return model.Value{}, err
			}
			micros[i] += int64(zones[i]) * intervalMicrosPerSecond
		}
	}
	for i := range endpoints {
		for j := range endpoints {
			if !endpoints[j].null && cmp.Or(cmp.Compare(micros[i], micros[j]), cmp.Compare(zones[i], zones[j])) > 0 {
				endpoints[i].value++
			}
		}
	}
	left := orderedTemporalPeriod(endpoints[0], endpoints[1])
	right := orderedTemporalPeriod(endpoints[2], endpoints[3])
	if left.unknown || right.unknown {
		return model.NullVal(model.TypeBoolean), nil
	}
	if left.start > right.start {
		left, right = right, left
	}
	if left.start < right.start {
		if left.endNull {
			return model.NullVal(model.TypeBoolean), nil
		}
		if right.start < left.end {
			return model.BoolVal(true), nil
		}
		if right.endNull {
			return model.NullVal(model.TypeBoolean), nil
		}
		return model.BoolVal(false), nil
	}
	if left.endNull || right.endNull {
		return model.NullVal(model.TypeBoolean), nil
	}
	return model.BoolVal(true), nil
}
