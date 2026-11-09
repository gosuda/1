package builtin

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileNumericHash(signature Signature) scalarKernel {
	if signature.ID != 432 && signature.ID != 780 {
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (result model.Value, err error) {
		work, err := budget.NewWorkspace(ctx)
		if err != nil {
			return model.Value{}, numericAllocationError(err)
		}
		defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
		value, err := parseNumeric(ctx, work, args[0].StrVal)
		if err != nil {
			return model.Value{}, err
		}
		seed := uint64(0)
		extended := signature.ID == 780
		if extended {
			seed = uint64(args[1].IntVal)
		}
		hash := seed
		if value.special == numericFinite {
			if value.coefficient.Sign() == 0 {
				hash = seed - 1
			} else {
				digits := numericDigits(value.coefficient)
				last := digits - 1
				for last >= 0 && value.coefficient.DecimalDigit(last) == 0 {
					if err := ctx.Err(); err != nil {
						return model.Value{}, err
					}
					last--
				}
				remaining, err := budget.After(work.UsedBytes())
				if err != nil {
					return model.Value{}, err
				}
				hash, err = hashNumericDigits(ctx, digits-value.scale, 0, last, func(i int) byte {
					if i < 0 || i >= digits {
						return 0
					}
					return byte(value.coefficient.DecimalDigit(i))
				}, seed, remaining)
				if err != nil {
					return model.Value{}, err
				}
			}
		}
		if extended {
			return model.Int64Val(int64(hash)), nil
		}
		return model.Int64Val(int64(int32(hash))), nil
	}
}

// PostgreSQL hashes unsigned base-10000 digits in native little-endian order,
// omitting sign and display scale and XORing the sign-extended weight.
func hashNumericDigits(ctx context.Context, point, first, last int, digit func(int) byte, seed uint64, budget CallBudget) (uint64, error) {
	weight := (point - first - 1) / 4
	if point-first-1 < 0 && (point-first-1)%4 != 0 {
		weight--
	}
	start := point - 4*(weight+1)
	count := (last - start + 4) / 4
	size := uint64(count) * 2
	claim, err := numericTemporary(budget, 0, size)
	if err != nil {
		return 0, err
	}
	if claim != nil {
		defer claim.Release()
	}
	data := make([]byte, int(size))
	for i := range count {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		value := uint16(0)
		for j := range 4 {
			value = value*10 + uint16(digit(start+i*4+j))
		}
		binary.LittleEndian.PutUint16(data[i*2:], value)
	}
	hash, err := pgHashBytesExtended(ctx, data, seed)
	return hash ^ uint64(int64(weight)), err
}

func hashJSONBNumber(ctx context.Context, text string, seed uint64, budget CallBudget) (uint64, error) {
	number, err := inspectJSONBNumber(ctx, text)
	if err != nil {
		return 0, err
	}
	if number.zero {
		return seed - 1, nil
	}
	return hashNumericDigits(ctx, number.point, number.first, number.last, func(i int) byte { return jsonbNumberDigit(number, i) }, seed, budget)
}
