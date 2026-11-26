package neosepia

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// gatherDecodedBlock selects decoded PAX lanes without row values. Byte payloads remain
// borrowed until encoding; array child arenas contain only the selected elements.
// Columns must already be validated by decoding. Revalidating the full source
// for each retained group makes interleaved compaction quadratic in block rows.
func gatherDecodedBlock(ctx context.Context, source *columnar.Batch, order []uint32, maxBytes uint64) (columnar.Batch, uint64, error) {
	if err := ctx.Err(); err != nil {
		return columnar.Batch{}, 0, err
	}
	if source == nil || source.Selection != nil || source.Length < 0 {
		return columnar.Batch{}, 0, columnar.ErrInvalidBatch
	}
	for _, lane := range order {
		if err := ctx.Err(); err != nil {
			return columnar.Batch{}, 0, err
		}
		if uint64(lane) >= uint64(source.Length) {
			return columnar.Batch{}, 0, columnar.ErrInvalidBatch
		}
	}
	budget := encBudget{limit: maxBytes}
	if err := budget.chargeMul(uint64(len(source.Columns)), uint64(unsafe.Sizeof(columnar.Vector{}))); err != nil {
		return columnar.Batch{}, 0, err
	}
	result := columnar.Batch{Length: len(order), Columns: make([]columnar.Vector, len(source.Columns))}
	for i := range source.Columns {
		vector, err := gatherVector(ctx, &source.Columns[i], order, &budget, 0)
		if err != nil {
			return columnar.Batch{}, 0, err
		}
		result.Columns[i] = vector
	}
	return result, budget.used, nil
}

func gatherVector(ctx context.Context, source *columnar.Vector, order []uint32, budget *encBudget, depth int) (columnar.Vector, error) {
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	if depth > encMaxDepth || source.Offset != 0 {
		return columnar.Vector{}, columnar.ErrInvalidVector
	}
	if source.Encoding != columnar.Flat && source.Encoding != columnar.Dictionary {
		return columnar.Vector{}, columnar.ErrInvalidVector
	}
	if source.Encoding == columnar.Dictionary {
		switch source.Type.Storage {
		case model.TypeBytes, model.TypeString, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		default:
			return columnar.Vector{}, columnar.ErrInvalidVector
		}
	}
	result := columnar.Vector{Type: source.Type, Length: len(order)}
	if len(source.Nulls) != 0 {
		if err := budget.charge(uint64((len(order) + 7) / 8)); err != nil {
			return columnar.Vector{}, err
		}
		result.Nulls = make([]byte, (len(order)+7)/8)
		for i, lane := range order {
			if err := ctx.Err(); err != nil {
				return columnar.Vector{}, err
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
		if source.Encoding == columnar.Dictionary {
			if err := budget.chargeMul(uint64(len(order)), uint64(unsafe.Sizeof(columnar.Range{}))); err != nil {
				return columnar.Vector{}, err
			}
			result.Ranges = make([]columnar.Range, len(order))
			for i, lane := range order {
				if err := ctx.Err(); err != nil {
					return columnar.Vector{}, err
				}
				if !source.IsNull(int(lane)) {
					result.Ranges[i] = source.Ranges[source.Indices[lane]]
				}
			}
		} else {
			result.Ranges, err = permuteLanes(ctx, source.Ranges, order, budget)
		}
	case model.TypeVectorF32:
		result.Float32 = source.Float32
		result.Ranges, err = permuteLanes(ctx, source.Ranges, order, budget)
	case model.TypeComposite:
		if err := budget.chargeMul(uint64(len(source.Children)), uint64(unsafe.Sizeof(columnar.Vector{}))); err != nil {
			return columnar.Vector{}, err
		}
		result.Children = make([]columnar.Vector, len(source.Children))
		for i := range source.Children {
			result.Children[i], err = gatherVector(ctx, &source.Children[i], order, budget, depth+1)
			if err != nil {
				break
			}
		}
	case model.TypeArray:
		err = gatherArray(ctx, source, order, &result, budget, depth)
	default:
		err = columnar.ErrInvalidVector
	}
	if err != nil {
		return columnar.Vector{}, err
	}
	return result, nil
}

func gatherArray(ctx context.Context, source *columnar.Vector, order []uint32, result *columnar.Vector, budget *encBudget, depth int) error {
	var elements, dimensions uint64
	for _, lane := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if source.IsNull(int(lane)) {
			continue
		}
		elements += uint64(source.Ranges[lane].Length)
		dimensions += uint64(source.ShapeRanges[lane].Length)
		if elements > math.MaxUint32 || dimensions > math.MaxUint32 {
			return columnar.ErrBatchBudget
		}
	}
	if elements > uint64(math.MaxInt) || dimensions > uint64(math.MaxInt) {
		return columnar.ErrBatchBudget
	}
	if err := budget.chargeMul(uint64(len(order)), 2*uint64(unsafe.Sizeof(columnar.Range{}))); err != nil {
		return err
	}
	if err := budget.chargeMul(elements, 4); err != nil {
		return err
	}
	if err := budget.chargeMul(dimensions, uint64(unsafe.Sizeof(model.ArrayDimension{}))); err != nil {
		return err
	}
	if err := budget.charge(uint64(unsafe.Sizeof(columnar.Vector{}))); err != nil {
		return err
	}
	result.Ranges = make([]columnar.Range, len(order))
	result.ShapeRanges = make([]columnar.Range, len(order))
	result.Dimensions = make([]model.ArrayDimension, 0, int(dimensions))
	childOrder := make([]uint32, 0, int(elements))
	for i, lane := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if source.IsNull(int(lane)) {
			continue
		}
		values, shape := source.Ranges[lane], source.ShapeRanges[lane]
		result.Ranges[i] = columnar.Range{Offset: uint32(len(childOrder)), Length: values.Length}
		result.ShapeRanges[i] = columnar.Range{Offset: uint32(len(result.Dimensions)), Length: shape.Length}
		for offset := uint32(0); offset < values.Length; offset++ {
			if offset&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			childOrder = append(childOrder, values.Offset+offset)
		}
		result.Dimensions = append(result.Dimensions, source.Dimensions[shape.Offset:uint64(shape.Offset)+uint64(shape.Length)]...)
	}
	child, err := gatherVector(ctx, &source.Children[0], childOrder, budget, depth+1)
	if err != nil {
		return err
	}
	result.Children = []columnar.Vector{child}
	return nil
}
