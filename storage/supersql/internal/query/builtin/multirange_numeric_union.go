package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Numeric scale survives into the result. PostgreSQL sorts the concatenated
// component sequence before choosing union bounds, so a stable streaming merge
// is insufficient for this subtype. Other families retain the streaming path.
func numericMultirangeUnion(ctx context.Context, ops *rangeTypeOps, a, b multirangeSequence, budget CallBudget) (model.Value, error) {
	count := uint64(a.cursor.remaining) + uint64(b.cursor.remaining)
	if count > math.MaxInt32 {
		return model.Value{}, sqlError("54000", "multirange component count exceeds limit")
	}
	nested, claim, err := reserveMultirangeWork(ctx, count*uint64(unsafe.Sizeof(rangeDatum{})), budget)
	if err != nil {
		return model.Value{}, err
	}
	if claim != nil {
		defer claim.Release()
	}
	values := make([]rangeDatum, int(count))
	offset := 0
	for _, sequence := range [2]multirangeSequence{a, b} {
		for {
			value, more, err := sequence.next(ctx)
			if err != nil {
				return model.Value{}, err
			}
			if !more {
				break
			}
			if offset >= len(values) {
				return model.Value{}, invalidRangeValue()
			}
			values[offset] = value
			offset++
		}
	}
	if offset != len(values) {
		return model.Value{}, invalidRangeValue()
	}
	return normalizeMultirange(ctx, ops, values, nested)
}
