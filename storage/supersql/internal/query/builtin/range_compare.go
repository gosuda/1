package builtin

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type rangeTypeOps struct {
	family     RangeFamilyInfo
	comparison Callable
}

func newRangeTypeOps(ctx context.Context, family RangeFamilyInfo, options ResolveOptions, maximum uint64) (rangeTypeOps, uint64, error) {
	ops := rangeTypeOps{family: family}
	header := uint64(unsafe.Sizeof(ops))
	if header > maximum {
		return ops, 0, sqlError("54000", "range subtype metadata budget exceeded")
	}
	class, known := LookupOperatorClass(family.SubtypeClass)
	subtypeType := model.Type{ID: family.representation(), Storage: family.SubtypeStorage}
	if !known || class.CompareFunction == 0 || !comparisonBinaryCoercible(subtypeType, class.InputType, options) {
		return ops, 0, sqlError("42804", "range subtype comparison metadata is invalid")
	}
	subtype, known := resolvedTypeLayout(options, family.representation())
	if !known {
		return ops, 0, sqlError("42704", "range subtype does not exist")
	}
	if class.InputType != 2277 && class.InputType != 2249 && class.InputType != 3500 && class.InputType != 3831 && class.InputType != 4537 {
		subtype.Type.ID = class.InputType
		subtype.Type.Modifier = -1
	}
	subtype.Type.Collation = family.Collation
	output := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	options.Collation = family.Collation
	comparison, err := ResolveTyped(ctx, class.CompareFunction, []model.TypeLayout{subtype, subtype}, output, options, maximum-header)
	if err != nil {
		return ops, 0, err
	}
	ops.comparison = comparison
	return ops, header + comparison.OwnedBytes(), nil
}

func (ops *rangeTypeOps) compareValues(ctx context.Context, a, b rangeBound, budget CallBudget) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if a.infinite && b.infinite {
		if a.side == b.side {
			return 0, nil
		}
		if a.side == rangeLower {
			return -1, nil
		}
		return 1, nil
	}
	if a.infinite {
		if a.side == rangeLower {
			return -1, nil
		}
		return 1, nil
	}
	if b.infinite {
		if b.side == rangeLower {
			return 1, nil
		}
		return -1, nil
	}
	args := [2]model.Value{borrowRangeBoundValue(ops.family, a), borrowRangeBoundValue(ops.family, b)}
	result, err := ops.comparison.Call(ctx, args[:], budget.WithOutput(0))
	if err != nil {
		return 0, err
	}
	if result.IsNull || result.Type != model.TypeInt64 {
		return 0, sqlError("XX000", "range subtype comparison returned invalid result")
	}
	return int(result.IntVal), nil
}

func (ops *rangeTypeOps) compareBounds(ctx context.Context, a, b rangeBound, budget CallBudget) (int, error) {
	order, err := ops.compareValues(ctx, a, b, budget)
	if err != nil || order != 0 || a.infinite || b.infinite {
		return order, err
	}
	if !a.inclusive && !b.inclusive {
		if a.side == b.side {
			return 0, nil
		}
		if a.side == rangeLower {
			return 1, nil
		}
		return -1, nil
	}
	if !a.inclusive {
		if a.side == rangeLower {
			return 1, nil
		}
		return -1, nil
	}
	if !b.inclusive {
		if b.side == rangeLower {
			return -1, nil
		}
		return 1, nil
	}
	return 0, nil
}

func (ops *rangeTypeOps) compareRanges(ctx context.Context, a, b rangeDatum, budget CallBudget) (int, error) {
	if a.empty {
		if b.empty {
			return 0, ctx.Err()
		}
		return -1, ctx.Err()
	}
	if b.empty {
		return 1, ctx.Err()
	}
	order, err := ops.compareBounds(ctx, a.lower, b.lower, budget)
	if err != nil || order != 0 {
		return order, err
	}
	return ops.compareBounds(ctx, a.upper, b.upper, budget)
}
