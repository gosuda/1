package builtin

import (
	"cmp"
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileBtreeComparison(signature Signature, options ResolveOptions) scalarKernel {
	switch signature.ID {
	case 350, 1693:
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) { // PostgreSQL subtracts int2 and bool operands after promoting to int32.
			return model.Int64Val(args[0].IntVal - args[1].IntVal), nil
		}
	case 351, 356, 842, 2188, 2189, 2190, 2191, 2192, 2193:
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			return model.Int64Val(int64(cmp.Compare(args[0].IntVal, args[1].IntVal))), nil
		}
	case 354, 355, 2194, 2195:
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			return model.Int64Val(int64(compareFloat(args[0].FloatVal, args[1].FloatVal))), nil
		}
	case 184, 185:
		equal := signature.ID == 184
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			return model.BoolVal((args[0].IntVal == args[1].IntVal) == equal), nil
		}
	case 62, 240, 241, 242, 243, 244, 245, 246, 247, 248, 249, 250, 251, 252, 253, 359:
		collation := options.Collation
		if collation == 100 {
			collation = options.DefaultCollation
		}
		indeterminate := collation != 950 && collation != 951
		op := opCmp
		switch signature.ID {
		case 62, 240, 247:
			op = opEq
		case 245, 252:
			op = opNe
		case 241, 248:
			op = opLt
		case 242, 249:
			op = opLe
		case 243, 250:
			op = opGe
		case 244, 251:
			op = opGt
		}
		return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) { // nameeq uses namecmp too; unlike text equality it requires collation.
			if indeterminate {
				return model.Value{}, sqlError("42P22", "name comparison requires a resolved C or POSIX collation")
			}
			if _, err := textCharacters(ctx, args[0].StrVal); err != nil {
				return model.Value{}, err
			}
			if _, err := textCharacters(ctx, args[1].StrVal); err != nil {
				return model.Value{}, err
			}
			order, err := compareText(ctx, args[0].StrVal, args[1].StrVal)
			if err != nil {
				return model.Value{}, err
			}
			return comparisonResult(order, op), nil
		}
	default:
		return nil
	}
}
