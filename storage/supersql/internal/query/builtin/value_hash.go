package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileValueHash(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 1688, 1697, 2039, 2963, 3409, 3411, 3412, 3418,
		6413, 6414, 6415, 6416, 6417, 6418, 6425, 6426:
	default:
		return nil
	}
	extended := signature.Result == 20
	inputType := signature.Arguments[0]
	return func(ctx context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		seed := uint64(0)
		if extended {
			seed = uint64(args[1].IntVal)
		}
		input := args[0]
		var hash uint64
		var err error
		switch inputType {
		case 16:
			word := uint32(0)
			if input.IntVal != 0 {
				word = 1
			}
			hash = pgHashUint32Extended(word, seed)
		case 1082:
			hash = pgHashUint32Extended(uint32(input.IntVal), seed)
		case 1083, 1114, 1184:
			hash = pgHashUint32Extended(valueHashInt64Word(input.IntVal), seed)
		case 1186:
			interval := input.IntervalVal
			days := int64(interval.Months)*intervalDaysPerMonth + int64(interval.Days)
			// REL_18_4 interval_hash truncates the comparison span to its low
			// 64 bits, including for the all-minimum/all-maximum infinity fields.
			span := uint64(days)*uint64(intervalMicrosPerDay) + uint64(interval.Microseconds)
			hash = pgHashUint32Extended(valueHashInt64Word(int64(span)), seed)
		case 17:
			hash, err = pgHashBytesExtended(ctx, input.BytesVal, seed)
		case 2950:
			var data [16]byte
			data, err = parseUUIDText(ctx, input.StrVal)
			if err == nil {
				hash, err = pgHashBytesExtended(ctx, data[:], seed)
			}
		}
		if err != nil {
			return model.Value{}, err
		}
		if extended {
			return model.Int64Val(int64(hash)), nil
		}
		return model.Int64Val(int64(int32(hash))), nil
	}
}

func valueHashInt64Word(value int64) uint32 {
	// hashint8 folds the sign-adjusted high half to preserve int4 equality.
	high := uint32(value >> 32)
	if value < 0 {
		high = ^high
	}
	return uint32(value) ^ high
}
