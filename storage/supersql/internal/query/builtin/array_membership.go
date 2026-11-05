package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileArrayMembership(signature Signature, element Callable, lookupErr error) scalarKernel {
	switch signature.ID {
	case 2747, 2748, 2749:
	default:
		return nil
	}
	matchAll := signature.ID != 2747
	reverse := signature.ID == 2748
	return func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
		left, right := &arguments[0], &arguments[1]
		if err := model.ValidateArrayShape(left.Dimensions, uint64(len(left.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		if err := model.ValidateArrayShape(right.Dimensions, uint64(len(right.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
		if lookupErr != nil {
			return model.Value{}, lookupErr
		}
		searched, candidates := left.ArrayVal, right.ArrayVal
		if reverse {
			searched, candidates = candidates, searched
		}
		matched, err := matchArrayMembers(ctx, searched, candidates, element, matchAll, callBudget)
		if err != nil {
			return model.Value{}, err
		}
		return model.BoolVal(matched), nil
	}
}

func matchArrayMembers(ctx context.Context, searched, candidates []model.Value, element Callable, matchAll bool, callBudget CallBudget) (bool, error) {
	var arguments [2]model.Value
	for i := range searched {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if searched[i].IsNull {
			if matchAll {
				return false, nil
			}
			continue
		}
		matched := false
		for j := range candidates {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if candidates[j].IsNull {
				continue
			}
			arguments[0], arguments[1] = searched[i], candidates[j]
			var err error
			matched, err = arrayElementEqual(ctx, element, &arguments, callBudget)
			if err != nil {
				return false, err
			}
			if matched {
				break
			}
		}
		if !matchAll && matched {
			return true, nil
		}
		if matchAll && !matched {
			return false, nil
		}
	}
	return matchAll, nil
}
