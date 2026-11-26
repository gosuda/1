package builtin

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/decimalint"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileNumericExtra(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 1376, 1481, 1732, 1733, 1734, 1735, 1736, 1737, 1738, 1739, 1741, 1764, 1766, 1767, 2169, 2461, 2917, 2918, 3281, 3449, 5042, 5043:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (result model.Value, err error) {
		if err = ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if signature.ID == 2917 {
			return numericExtraTypmodIn(ctx, args[0])
		}
		if signature.ID == 2918 {
			var buffer [32]byte
			text := buffer[:0]
			if args[0].IntVal >= 4 {
				encoded := int32(args[0].IntVal) - 4
				text = append(text, '(')
				text = strconv.AppendInt(text, int64(uint32(encoded)>>16), 10)
				text = append(text, ',')
				text = strconv.AppendInt(text, int64((encoded&2047)^1024)-1024, 10)
				text = append(text, ')')
			}
			return ownedText(ctx, budget.OutputBytes, string(text))
		}
		work, err := budget.NewWorkspace(ctx)
		if err != nil {
			return model.Value{}, numericAllocationError(err)
		}
		defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
		if signature.ID == 1376 {
			value, err := numericFactorial(ctx, work, args[0].IntVal)
			if err != nil {
				return model.Value{}, err
			}
			return numericValue(ctx, value, budget.OutputLimit(work))
		}
		text := args[0].StrVal
		if signature.ID == 3449 {
			var token string
			shape, err := walkJSONB(ctx, text, func(kind jsonTextKind, value string, key bool, depth int) error {
				if depth == 0 && kind == jsonTextNumber {
					token = value
				}
				return nil
			})
			if err != nil {
				return model.Value{}, err
			}
			if shape.kind == jsonTextNull {
				return model.NullVal(model.TypeNumeric), nil
			}
			if shape.kind != jsonTextNumber {
				return model.Value{}, sqlError("22023", "cannot cast jsonb value to type numeric")
			}
			text = token
		}
		left, err := parseNumeric(ctx, work, text)
		if err != nil {
			return model.Value{}, err
		}
		if left.special == numericFinite && left.scale > numericMaxScale {
			return model.Value{}, numericOverflow()
		}
		switch signature.ID {
		case 3449:
			return numericValue(ctx, left, budget.OutputLimit(work))
		case 3281, 5042:
			if left.special != numericFinite {
				return model.NullVal(model.TypeInt64), nil
			}
			scale := left.scale
			if signature.ID == 5042 {
				scale, err = numericMinimumScale(ctx, left)
			}
			return model.Int64Val(int64(scale)), err
		case 5043:
			if left.special == numericFinite {
				var scale int
				scale, err = numericMinimumScale(ctx, left)
				if err == nil {
					left, err = numericRescale(ctx, work, left, scale, false)
				}
			}
		case 2461:
			return numericExtraSend(ctx, left, budget.OutputLimit(work))
		case 1764:
			var one *decimalint.Int
			one, err = work.FromInt64(ctx, 1)
			if err == nil {
				left, err = numericArithmetic(ctx, work, left, decimal{coefficient: one}, opPl)
			}
		case 1732, 1733:
			left, err = numericExp(ctx, work, left)
		case 1734, 1735:
			left, err = numericLn(ctx, work, left)
		case 1481, 1741:
			var ten *decimalint.Int
			ten, err = work.FromInt64(ctx, 10)
			if err == nil {
				left, err = numericLog(ctx, work, decimal{coefficient: ten}, left)
			}
		default:
			var right decimal
			right, err = parseNumeric(ctx, work, args[1].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			if right.special == numericFinite && right.scale > numericMaxScale {
				return model.Value{}, numericOverflow()
			}
			switch signature.ID {
			case 1766, 1767:
				var order int
				order, err = numericCompare(ctx, left, right)
				chooseSmaller := signature.ID == 1766 && order >= 0
				chooseLarger := signature.ID == 1767 && order <= 0
				if chooseSmaller || chooseLarger {
					left = right
				}
			case 1736, 1737:
				left, err = numericLog(ctx, work, left, right)
			case 1738, 1739, 2169:
				left, err = numericExponentiate(ctx, work, left, right)
			}
		}
		if err != nil {
			return model.Value{}, err
		}
		return numericValue(ctx, left, budget.OutputLimit(work))
	}
}

func numericExtraTypmodIn(ctx context.Context, input model.Value) (model.Value, error) {
	if len(input.Dimensions) != 1 {
		return model.Value{}, sqlError("22023", "type modifier array must be one-dimensional")
	}
	if err := model.ValidateArrayShape(input.Dimensions, uint64(len(input.ArrayVal))); err != nil {
		return model.Value{}, sqlError("2202E", "invalid array dimensions")
	}
	var values [2]int64
	for i, item := range input.ArrayVal {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if item.IsNull {
			return model.Value{}, sqlError("22004", "type modifier array must not contain nulls")
		}
		value, err := parseInteger(ctx, item.StrVal, 23)
		if err != nil {
			return model.Value{}, err
		}
		if i < len(values) {
			values[i] = value.IntVal
		}
	}
	if len(input.ArrayVal) < 1 || len(input.ArrayVal) > 2 {
		return model.Value{}, sqlError("22023", "invalid NUMERIC type modifier")
	}
	if values[0] < 1 || values[0] > 1000 {
		return model.Value{}, sqlError("22023", "NUMERIC precision must be between 1 and 1000")
	}
	if values[1] < -1000 || values[1] > 1000 {
		return model.Value{}, sqlError("22023", "NUMERIC scale must be between -1000 and 1000")
	}
	return model.Int64Val((values[0]<<16 | values[1]&2047) + 4), nil
}

func numericMinimumScale(ctx context.Context, value decimal) (int, error) {
	if value.coefficient.Sign() == 0 {
		return 0, ctx.Err()
	}
	scale, digits := value.scale, numericDigits(value.coefficient)
	for scale > 0 && value.coefficient.DecimalDigit(digits-1) == 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		scale--
		digits--
	}
	return scale, ctx.Err()
}

func numericExtraSend(ctx context.Context, value decimal, budget int64) (model.Value, error) {
	weight, count, offset := 0, 0, 0
	sign := uint16(0)
	switch value.special {
	case numericNaN:
		sign = 0xc000
	case numericPositiveInfinity:
		sign = 0xd000
	case numericNegativeInfinity:
		sign = 0xf000
	default:
		if value.coefficient.Sign() < 0 {
			sign = 0x4000
		}
		if value.coefficient.Sign() != 0 {
			weight, _ = numericLeading(value)
			digits := numericDigits(value.coefficient)
			offset = 4*(weight+1) - (digits - value.scale)
			last := digits
			for last > 0 && value.coefficient.DecimalDigit(last-1) == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
				last--
			}
			count = (offset + last + 3) / 4
		}
	}
	if err := outputBudget(int64(8+2*count), budget); err != nil {
		return model.Value{}, err
	}
	scale := uint16(value.scale)
	if value.special != numericFinite {
		// PostgreSQL extracts display-scale bits even from special headers.
		scale = (sign & 0x1f80) >> 7
	}
	output := make([]byte, 8+2*count)
	binary.BigEndian.PutUint16(output, uint16(count))
	binary.BigEndian.PutUint16(output[2:], uint16(int16(weight)))
	binary.BigEndian.PutUint16(output[4:], sign)
	binary.BigEndian.PutUint16(output[6:], scale)
	for i := range count {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		digit := uint16(0)
		for j := range 4 {
			digit *= 10
			if index := i*4 + j - offset; index >= 0 {
				digit += uint16(value.coefficient.DecimalDigit(index))
			}
		}
		binary.BigEndian.PutUint16(output[8+2*i:], digit)
	}
	return model.BytesVal(output), ctx.Err()
}

func numericFactorial(ctx context.Context, work *decimalint.Workspace, n int64) (result decimal, err error) {
	if n < 0 {
		return decimal{}, sqlError("22003", "factorial of a negative number is undefined")
	}
	if n > 32177 {
		return decimal{}, numericOverflow()
	}
	coefficient, err := work.FromInt64(ctx, 1)
	if err != nil {
		return decimal{}, err
	}
	for factor := int64(2); factor <= n; factor++ {
		if err = numericWork(ctx, coefficient.Digits()+5); err != nil {
			return decimal{}, err
		}
		multiplier, allocErr := work.FromInt64(ctx, factor)
		if allocErr != nil {
			return decimal{}, allocErr
		}
		next, mulErr := work.Mul(ctx, coefficient, multiplier)
		err = errors.Join(mulErr, coefficient.Close(), multiplier.Close())
		if err != nil {
			return decimal{}, err
		}
		coefficient = next
	}
	return decimal{coefficient: coefficient}, nil
}
