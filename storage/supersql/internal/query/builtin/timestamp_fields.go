package builtin

import (
	"context"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimestampFields(signature Signature) scalarKernel {
	if signature.ID != 2021 && signature.ID != 6202 {
		return nil
	}
	numeric := signature.ID == 6202
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		unit := dateFieldUnit(args[0].StrVal)
		value := args[1].IntVal
		switch unit {
		case intervalInputUnknown, intervalInputIgnore, intervalInputAgo:
			return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp without time zone", false)
		case intervalFieldReserved:
			return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp without time zone", true)
		}
		if !finiteTimestamp(value) {
			switch unit {
			case intervalInputMicrosecond, intervalInputMillisecond, intervalInputSecond, intervalInputMinute, intervalInputHour, intervalInputDay, intervalInputMonth, intervalFieldQuarter, intervalInputWeek, dateFieldDOW, dateFieldISODOW, dateFieldDOY, intervalInputUnsupported:
				storage := model.TypeFloat64
				if numeric {
					storage = model.TypeNumeric
				}
				return model.NullVal(storage), nil
			default:
				sign := 1
				if value < 0 {
					sign = -1
				}
				return intervalInfiniteField(intervalFieldEpoch, sign, numeric, budget.OutputBytes)
			}
		}
		if unit == intervalFieldEpoch {
			return timestampEpochField(ctx, value, numeric, budget)
		}
		days, micros := splitTimestamp(value)
		// timestamp2tm accepts internal values beyond the input upper bound,
		// but refuses negative Julian days before checking supported units.
		if days+dateEpochJulian < 0 {
			return model.Value{}, sqlError("22008", "timestamp out of range")
		}
		if unit == intervalInputUnsupported {
			return model.Value{}, temporalFieldError(args[0].StrVal, "timestamp without time zone", true)
		}
		return timestampCalendarField(ctx, unit, days, micros, numeric, budget)
	}
}

func timestampEpochField(ctx context.Context, value int64, numeric bool, budget CallBudget) (model.Value, error) {
	const epoch int64 = -946684800000000
	if numeric {
		if value >= math.MaxInt64+epoch {
			return timestampNumericEpochOverflow(ctx, value, budget)
		}
		return intervalNumericField(ctx, (value-epoch)/1e6, (value-epoch)%1e6, 6, budget.OutputBytes)
	}
	if value < math.MaxInt64+epoch {
		return model.Float64Val(float64(value-epoch) / 1e6), nil
	}
	return model.Float64Val((float64(value) - float64(epoch)) / 1e6), nil
}

func timestampCalendarField(ctx context.Context, unit intervalInputUnit, days, micros int64, numeric bool, budget CallBudget) (model.Value, error) {
	var result int64
	switch unit {
	case intervalInputMicrosecond, intervalInputMillisecond, intervalInputSecond:
		result = micros % intervalMicrosPerMinute
		if unit == intervalInputSecond {
			if numeric {
				return intervalNumericField(ctx, result/1e6, result%1e6, 6, budget.OutputBytes)
			}
			return model.Float64Val(float64(result/1e6) + float64(result%1e6)/1e6), nil
		}
		if unit == intervalInputMillisecond {
			if numeric {
				return intervalNumericField(ctx, result/1000, result%1000, 3, budget.OutputBytes)
			}
			return model.Float64Val(float64(result/1e6)*1000 + float64(result%1e6)/1000), nil
		}
	case intervalInputMinute:
		result = micros % intervalMicrosPerHour / intervalMicrosPerMinute
	case intervalInputHour:
		result = micros / intervalMicrosPerHour
	case dateFieldJulian:
		if numeric {
			return timestampNumericJulian(ctx, days+dateEpochJulian, micros, budget)
		}
		seconds := float64(micros/1e6) + float64(micros%1e6)/1e6
		return model.Float64Val(float64(days+dateEpochJulian) + seconds/86400), nil
	default:
		result = dateIntegralField(unit, days)
	}
	if numeric {
		return intervalNumericField(ctx, result, 0, 0, budget.OutputBytes)
	}
	return model.Float64Val(float64(result)), nil
}

func timestampNumericJulian(ctx context.Context, julian, micros int64, budget CallBudget) (result model.Value, err error) {
	work, err := budget.NewWorkspace(ctx)
	if err != nil {
		return model.Value{}, numericAllocationError(err)
	}
	defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
	numerator, err := work.FromInt64(ctx, micros)
	if err != nil {
		return model.Value{}, err
	}
	denominator, err := work.FromInt64(ctx, intervalMicrosPerDay)
	if err != nil {
		return model.Value{}, err
	}
	fraction, err := numericDivide(ctx, work, decimal{coefficient: numerator}, decimal{coefficient: denominator}, false)
	if err != nil {
		return model.Value{}, err
	}
	day, err := work.FromInt64(ctx, julian)
	if err != nil {
		return model.Value{}, err
	}
	sum, err := numericArithmetic(ctx, work, decimal{coefficient: day}, fraction, opPl)
	if err != nil {
		return model.Value{}, err
	}
	return numericValue(ctx, sum, budget.OutputLimit(work))
}

func timestampNumericEpochOverflow(ctx context.Context, value int64, budget CallBudget) (result model.Value, err error) {
	work, err := budget.NewWorkspace(ctx)
	if err != nil {
		return model.Value{}, numericAllocationError(err)
	}
	defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
	stamp, err := work.FromInt64(ctx, value)
	if err != nil {
		return model.Value{}, err
	}
	epoch, err := work.FromInt64(ctx, -946684800000000)
	if err != nil {
		return model.Value{}, err
	}
	difference, err := work.Sub(ctx, stamp, epoch)
	if err != nil {
		return model.Value{}, err
	}
	divisor, err := work.FromInt64(ctx, 1000000)
	if err != nil {
		return model.Value{}, err
	}
	// PostgreSQL's overflow path divides at numeric's selected scale before
	// rounding to six places; it can lose microseconds before the final padding.
	quotient, err := numericDivide(ctx, work, decimal{coefficient: difference}, decimal{coefficient: divisor}, false)
	if err != nil {
		return model.Value{}, err
	}
	rounded, err := numericRescale(ctx, work, quotient, 6, true)
	if err != nil {
		return model.Value{}, err
	}
	return numericValue(ctx, rounded, budget.OutputLimit(work))
}
