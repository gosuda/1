package builtin

import (
	"cmp"
	"context"
	"encoding/binary"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// SQL time stores microseconds since midnight, including the distinct 24:00 end.
func timeValue(micros int64) model.Value {
	return model.Value{Type: model.TypeTimestamp, IntVal: micros}
}

func compileTime(signature Signature) scalarKernel {
	if signature.ID == 3847 {
		return makeTime
	}
	switch signature.ID {
	case 1102, 1103, 1104, 1105, 1106, 1107, 1144, 1145, 1370, 1377, 1378, 1419, 1690, 1747, 1748, 1848, 1968, 2471:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		for index := range args {
			if signature.Arguments[index] == 1083 && (args[index].IntVal < 0 || args[index].IntVal > intervalMicrosPerDay) {
				return model.Value{}, sqlError("22008", "time out of range")
			}
		}
		left := args[0].IntVal
		switch signature.ID {
		case 1370:
			return model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: left}}, nil
		case 1419:
			span := args[0].IntervalVal
			if intervalInfinity(span) != 0 {
				return model.Value{}, sqlError("22008", "cannot convert infinite interval to time")
			}
			result := span.Microseconds % intervalMicrosPerDay
			if result < 0 {
				result += intervalMicrosPerDay
			}
			return timeValue(result), nil
		case 1968:
			return timeValue(roundTimePrecision(left, args[1].IntVal)), nil
		case 1144:
			fields := intervalOutputFields{hour: left / intervalMicrosPerHour, minute: left / intervalMicrosPerMinute % 60, second: left / intervalMicrosPerSecond % 60, microsecond: left % intervalMicrosPerSecond}
			var scratch [15]byte
			output := appendIntervalOutputClock(scratch[:0], fields, true)
			if err := outputBudget(int64(len(output)), budget.OutputBytes); err != nil {
				return model.Value{}, err
			}
			return model.StringVal(string(output)), ctx.Err()
		case 2471:
			if err := outputBudget(8, budget.OutputBytes); err != nil {
				return model.Value{}, err
			}
			output := make([]byte, 8)
			binary.BigEndian.PutUint64(output, uint64(left))
			return model.BytesVal(output), ctx.Err()
		case 1747, 1748, 1848:
			span := args[1].IntervalVal
			if signature.ID == 1848 {
				left = args[1].IntVal
				span = args[0].IntervalVal
			}
			return addTimeInterval(left, span, signature.ID == 1748)
		}
		right := args[1].IntVal
		switch signature.ID {
		case 1102:
			return model.BoolVal(left < right), nil
		case 1103:
			return model.BoolVal(left <= right), nil
		case 1104:
			return model.BoolVal(left > right), nil
		case 1105:
			return model.BoolVal(left >= right), nil
		case 1106:
			return model.BoolVal(left != right), nil
		case 1107:
			return model.Int64Val(int64(cmp.Compare(left, right))), nil
		case 1145:
			return model.BoolVal(left == right), nil
		case 1377:
			return timeValue(max(left, right)), nil
		case 1378:
			return timeValue(min(left, right)), nil
		default:
			return model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: left - right}}, nil
		}
	}
}

func makeTime(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
	hour, minute, seconds := args[0].IntVal, args[1].IntVal, args[2].FloatVal
	invalidHour := hour < 0 || hour > 24
	invalidMinute := minute < 0 || minute >= 60
	if invalidHour || invalidMinute || math.IsNaN(seconds) {
		return model.Value{}, sqlError("22008", "time field value out of range")
	}
	micros := math.RoundToEven(seconds * 1e6)
	if micros < 0 || micros > 60e6 {
		return model.Value{}, sqlError("22008", "time field value out of range")
	}
	result := (hour*60+minute)*intervalMicrosPerMinute + int64(micros)
	if result > intervalMicrosPerDay {
		return model.Value{}, sqlError("22008", "time field value out of range")
	}
	return timeValue(result), nil
}

func addTimeInterval(value int64, span model.Interval, subtract bool) (model.Value, error) {
	if intervalInfinity(span) != 0 {
		return model.Value{}, sqlError("22008", "cannot apply infinite interval to time")
	}
	// PostgreSQL wraps the int64 sum before reducing modulo a day.
	result := int64(uint64(value) + uint64(span.Microseconds))
	if subtract {
		result = int64(uint64(value) - uint64(span.Microseconds))
	}
	result %= intervalMicrosPerDay
	if result < 0 {
		result += intervalMicrosPerDay
	}
	return timeValue(result), nil
}
