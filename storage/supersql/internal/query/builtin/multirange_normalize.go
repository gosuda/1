package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func reserveMultirangeWork(ctx context.Context, size uint64, budget CallBudget) (CallBudget, columnar.Allocation, error) {
	nested, err := budget.After(size)
	if err != nil {
		return CallBudget{}, nil, err
	}
	admission := budget
	admission.OutputBytes = math.MaxInt64
	claim, err := reserveOutputBytes(ctx, size, admission)
	return nested, claim, err
}

// The caller owns canonical range values and excludes their live storage from
// budget. Sorting and compaction mutate that temporary.
func normalizeMultirange(ctx context.Context, ops *rangeTypeOps, values []rangeDatum, budget CallBudget) (model.Value, error) {
	nested, claim, err := reserveMultirangeWork(ctx, 3*uint64(unsafe.Sizeof(rangeDatum{})), budget)
	if err != nil {
		return model.Value{}, err
	}
	if claim != nil {
		defer claim.Release()
	}
	if err := sortMultirange(ctx, ops, values, nested); err != nil {
		return model.Value{}, err
	}
	count := 0
	for _, value := range values {
		if value.empty {
			continue
		}
		if count == 0 {
			values[0] = value
			count++
			continue
		}
		join, err := ops.overlaps(ctx, values[count-1], value, nested)
		if err != nil {
			return model.Value{}, err
		}
		if !join {
			join, err = ops.adjacent(ctx, values[count-1], value, nested)
		}
		if err != nil {
			return model.Value{}, err
		}
		if join {
			values[count-1], err = ops.union(ctx, values[count-1], value, false, nested)
			if err != nil {
				return model.Value{}, err
			}
		} else {
			values[count] = value
			count++
		}
	}
	clear(values[count:])
	return encodeMultirange(ctx, ops.family, values[:count], nested)
}
