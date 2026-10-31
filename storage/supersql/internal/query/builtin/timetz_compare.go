package builtin

import (
	"cmp"
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compareTimetz(left, right []byte) (int, error) {
	leftTime, leftZone, err := DecodeTimetz(left)
	if err != nil {
		return 0, err
	}
	rightTime, rightZone, err := DecodeTimetz(right)
	if err != nil {
		return 0, err
	}
	leftUTC := leftTime + int64(leftZone)*1_000_000
	rightUTC := rightTime + int64(rightZone)*1_000_000
	return cmp.Or(cmp.Compare(leftUTC, rightUTC), cmp.Compare(leftZone, rightZone)), nil
}

func compileTimetzComparison(signature Signature) scalarKernel {
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		order, err := compareTimetz(args[0].BytesVal, args[1].BytesVal)
		if err != nil {
			return model.Value{}, err
		}
		switch signature.ID {
		case 1352:
			return model.BoolVal(order == 0), nil
		case 1353:
			return model.BoolVal(order != 0), nil
		case 1354:
			return model.BoolVal(order < 0), nil
		case 1355:
			return model.BoolVal(order <= 0), nil
		case 1356:
			return model.BoolVal(order >= 0), nil
		case 1357:
			return model.BoolVal(order > 0), nil
		case 1358:
			return model.Int64Val(int64(order)), nil
		case 1379:
			if order > 0 {
				micros, west, _ := DecodeTimetz(args[0].BytesVal)
				return timetzResult(ctx, micros, west, budget)
			}
		case 1380:
			if order < 0 {
				micros, west, _ := DecodeTimetz(args[0].BytesVal)
				return timetzResult(ctx, micros, west, budget)
			}
		default:
			return model.Value{}, &MissingImplementationError{Signature: signature}
		}
		micros, west, _ := DecodeTimetz(args[1].BytesVal)
		return timetzResult(ctx, micros, west, budget)
	}
}
