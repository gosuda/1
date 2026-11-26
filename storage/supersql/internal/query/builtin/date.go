package builtin

import (
	"cmp"
	"context"
	"encoding/binary"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// SQL date uses signed days from 2000-01-01, not clock.Instant nanoseconds.
// The TypeTimestamp storage tag is shared; the resolved OID fixes its units.
const dateEpochJulian int64 = 2451545
const dateEndJulian int64 = 2147483494

func finiteDate(value int64) bool     { return value != math.MinInt32 && value != math.MaxInt32 }
func validDateDatum(value int64) bool { return value >= math.MinInt32 && value <= math.MaxInt32 }
func validDate(value int64) bool {
	return value >= -dateEpochJulian && value < dateEndJulian-dateEpochJulian
}
func dateValue(days int64) model.Value { return model.Value{Type: model.TypeTimestamp, IntVal: days} }

func compileDate(signature Signature) scalarKernel {
	if signature.ID == 3846 {
		return makeDate
	}
	switch signature.ID {
	case 1086, 1087, 1088, 1089, 1090, 1091, 1092, 1138, 1139, 1140, 1141, 1142, 1373, 2469, 2550:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		for index := range args {
			if signature.Arguments[index] != 1082 {
				continue
			}
			value := args[index].IntVal
			if !validDateDatum(value) {
				return model.Value{}, sqlError("22008", "date out of range")
			}
		}
		left := args[0].IntVal
		if signature.ID == 1373 {
			return model.BoolVal(finiteDate(left)), nil
		}
		if signature.ID == 2469 {
			if err := outputBudget(4, budget.OutputBytes); err != nil {
				return model.Value{}, err
			}
			result := make([]byte, 4)
			binary.BigEndian.PutUint32(result, uint32(int32(left)))
			return model.BytesVal(result), ctx.Err()
		}
		right := args[1].IntVal
		switch signature.ID {
		case 1086:
			return model.BoolVal(left == right), nil
		case 1087:
			return model.BoolVal(left < right), nil
		case 1088:
			return model.BoolVal(left <= right), nil
		case 1089:
			return model.BoolVal(left > right), nil
		case 1090:
			return model.BoolVal(left >= right), nil
		case 1091:
			return model.BoolVal(left != right), nil
		case 1092:
			return model.Int64Val(int64(cmp.Compare(left, right))), nil
		case 1138:
			return dateValue(max(left, right)), nil
		case 1139:
			return dateValue(min(left, right)), nil
		case 1140:
			if !finiteDate(left) || !finiteDate(right) {
				return model.Value{}, sqlError("22008", "cannot subtract infinite dates")
			}
			return model.Int64Val(left - right), nil
		case 1142:
			right = -right
		case 2550:
			left, right = right, left
		}
		if !finiteDate(left) {
			return dateValue(left), nil
		}
		result := left + right
		if !validDate(result) {
			return model.Value{}, sqlError("22008", "date out of range")
		}
		return dateValue(result), nil
	}
}

func makeDate(_ context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
	year, month, day := args[0].IntVal, args[1].IntVal, args[2].IntVal
	if year == 0 || year == math.MinInt32 {
		return model.Value{}, sqlError("22008", "date field value out of range")
	}
	if year < 0 {
		year++
	}
	if month < 1 || month > 12 {
		return model.Value{}, sqlError("22008", "date field value out of range")
	}
	days := timestampMonthDays(year, month)
	if day < 1 || day > days {
		return model.Value{}, sqlError("22008", "date field value out of range")
	}
	if year < -4713 || year > 5874898 {
		return model.Value{}, sqlError("22008", "date out of range")
	}
	if month > 2 {
		month++
		year += 4800
	} else {
		month += 13
		year += 4799
	}
	century := year / 100
	julian := year*365 - 32167 + year/4 - century + century/4 + 7834*month/256 + day
	result := julian - dateEpochJulian
	if !validDate(result) {
		return model.Value{}, sqlError("22008", "date out of range")
	}
	return dateValue(result), nil
}
