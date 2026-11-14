package neosepia

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Permuting a complete block retains variable-width payloads and array children:
// their range slots move, not their data. Composite fields share the parent's
// lane order. The source remains borrowed until the permuted block is encoded.
func permuteBlock(ctx context.Context, source *columnar.Batch, order []uint32, maxBytes uint64) (columnar.Batch, uint64, error) {
	if source == nil || source.Selection != nil || source.Length != len(order) || source.Length == 0 {
		return columnar.Batch{}, 0, columnar.ErrInvalidBatch
	}
	if err := source.Validate(source.Length); err != nil {
		return columnar.Batch{}, 0, err
	}
	budget := encBudget{limit: maxBytes}
	if err := budget.charge(uint64((len(order) + 7) / 8)); err != nil {
		return columnar.Batch{}, 0, err
	}
	seen := make([]byte, (len(order)+7)/8)
	for i, lane := range order {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return columnar.Batch{}, 0, err
			}
		}
		if uint64(lane) >= uint64(len(order)) || seen[lane/8]&(1<<(lane%8)) != 0 {
			return columnar.Batch{}, 0, columnar.ErrInvalidBatch
		}
		seen[lane/8] |= 1 << (lane % 8)
	}
	if err := budget.chargeMul(uint64(len(source.Columns)), uint64(unsafe.Sizeof(columnar.Vector{}))); err != nil {
		return columnar.Batch{}, 0, err
	}
	result := columnar.Batch{Length: source.Length, Columns: make([]columnar.Vector, len(source.Columns))}
	for i := range source.Columns {
		vector, err := permuteVector(ctx, &source.Columns[i], order, &budget)
		if err != nil {
			return columnar.Batch{}, 0, err
		}
		result.Columns[i] = vector
	}
	return result, budget.used, nil
}

func permuteVector(ctx context.Context, source *columnar.Vector, order []uint32, budget *encBudget) (columnar.Vector, error) {
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	if source.Encoding != columnar.Flat || source.Offset != 0 || source.Length != len(order) {
		return columnar.Vector{}, columnar.ErrInvalidVector
	}
	result := columnar.Vector{Type: source.Type, Length: source.Length}
	if len(source.Nulls) != 0 {
		if err := budget.charge(uint64((len(order) + 7) / 8)); err != nil {
			return columnar.Vector{}, err
		}
		result.Nulls = make([]byte, (len(order)+7)/8)
		for i, lane := range order {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return columnar.Vector{}, err
				}
			}
			if source.IsNull(int(lane)) {
				result.Nulls[i/8] |= 1 << (i % 8)
			}
		}
	}
	var err error
	switch source.Type.Storage {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		result.Int64, err = permuteLanes(ctx, source.Int64, order, budget)
	case model.TypeFloat64:
		result.Float64, err = permuteLanes(ctx, source.Float64, order, budget)
	case model.TypeInterval:
		result.Intervals, err = permuteLanes(ctx, source.Intervals, order, budget)
	case model.TypeBytes, model.TypeString, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		result.Data = source.Data
		result.Ranges, err = permuteLanes(ctx, source.Ranges, order, budget)
	case model.TypeVectorF32:
		result.Float32 = source.Float32
		result.Ranges, err = permuteLanes(ctx, source.Ranges, order, budget)
	case model.TypeArray:
		result.Children = source.Children
		result.Dimensions = source.Dimensions
		result.Ranges, err = permuteLanes(ctx, source.Ranges, order, budget)
		if err == nil {
			result.ShapeRanges, err = permuteLanes(ctx, source.ShapeRanges, order, budget)
		}
	case model.TypeComposite:
		if err := budget.chargeMul(uint64(len(source.Children)), uint64(unsafe.Sizeof(columnar.Vector{}))); err != nil {
			return columnar.Vector{}, err
		}
		result.Children = make([]columnar.Vector, len(source.Children))
		for i := range source.Children {
			result.Children[i], err = permuteVector(ctx, &source.Children[i], order, budget)
			if err != nil {
				break
			}
		}
	default:
		err = columnar.ErrInvalidVector
	}
	if err != nil {
		return columnar.Vector{}, err
	}
	return result, nil
}

func permuteLanes[T any](ctx context.Context, source []T, order []uint32, budget *encBudget) ([]T, error) {
	var value T
	if err := budget.chargeMul(uint64(len(order)), uint64(unsafe.Sizeof(value))); err != nil {
		return nil, err
	}
	result := make([]T, len(order))
	for i, lane := range order {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		result[i] = source[lane]
	}
	return result, nil
}
