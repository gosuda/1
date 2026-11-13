package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileArrayFill(signature Signature) scalarKernel {
	if signature.ID != 1193 && signature.ID != 1286 {
		return nil
	}
	explicitBounds := signature.ID == 1286
	return func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		if arguments[1].IsNull || explicitBounds && arguments[2].IsNull {
			return model.Value{}, sqlError("22004", "dimension array or low bound array cannot be null")
		}
		if err := checkArrayFillVector(ctx, arguments[1]); err != nil {
			return model.Value{}, err
		}
		rank := len(arguments[1].ArrayVal)
		if rank > 6 {
			return model.Value{}, sqlError("54000", "number of array dimensions exceeds the maximum allowed")
		}
		if explicitBounds {
			if err := checkArrayFillVector(ctx, arguments[2]); err != nil {
				return model.Value{}, err
			}
			if len(arguments[2].ArrayVal) != rank {
				return model.Value{}, sqlError("2202E", "low bound array has different size than dimensions array")
			}
		}
		var dimensions [6]model.ArrayDimension
		for i := range rank {
			length, err := arrayFillInt32(arguments[1].ArrayVal[i])
			if err != nil {
				return model.Value{}, err
			}
			lowerBound := int32(1)
			if explicitBounds {
				lowerBound, err = arrayFillInt32(arguments[2].ArrayVal[i])
				if err != nil {
					return model.Value{}, err
				}
			}
			dimensions[i] = model.ArrayDimension{Length: length, LowerBound: lowerBound}
		}
		count := int64(1)
		if rank == 0 {
			count = 0
		}
		for _, dimension := range dimensions[:rank] {
			if dimension.Length < 0 {
				return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
			}
			count *= int64(dimension.Length)
			if count > math.MaxInt32 {
				return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
			}
		}
		// PostgreSQL checks its Datum-count limit after all int32 products.
		if count > (1<<30-1)/8 {
			return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
		}
		for _, dimension := range dimensions[:rank] {
			if int64(dimension.LowerBound)+int64(dimension.Length) > math.MaxInt32 {
				return model.Value{}, sqlError("54000", "array lower bound is too large")
			}
		}
		if count == 0 {
			return model.Value{Type: model.TypeArray}, nil
		}
		fill := arguments[0]
		if fill.IsNull {
			fill = model.NullVal(fill.Type)
		}
		return copyArrayFill(ctx, dimensions[:rank], fill, int(count), budget)
	}
}

func checkArrayFillVector(ctx context.Context, vector model.Value) error {
	if len(vector.Dimensions) > 1 {
		return sqlError("2202E", "dimension array must be one dimensional")
	}
	for start := 0; start < len(vector.ArrayVal); start += 256 {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, value := range vector.ArrayVal[start:min(start+256, len(vector.ArrayVal))] {
			if value.IsNull {
				return sqlError("22004", "dimension values cannot be null")
			}
		}
	}
	if err := model.ValidateArrayShape(vector.Dimensions, uint64(len(vector.ArrayVal))); err != nil {
		return sqlError("2202E", "invalid dimension array shape")
	}
	return nil
}

func arrayFillInt32(value model.Value) (int32, error) {
	if value.Type != model.TypeInt64 {
		return 0, sqlError("42804", "dimension value is not an integer")
	}
	if err := integerRange(value.IntVal, 23); err != nil {
		return 0, err
	}
	return int32(value.IntVal), nil
}

func copyArrayFill(ctx context.Context, dimensions []model.ArrayDimension, fill model.Value, count int, budget int64) (model.Value, error) {
	limit := uint64(min(budget, arrayInputMaxBytes))
	dimensionBytes := uint64(len(dimensions)) * uint64(arrayDimensionBytes)
	if dimensionBytes > limit {
		return model.Value{}, arrayPartsCopyError(model.ErrValueBudget)
	}
	// The wrapper accounts for both the output element header and its nesting depth.
	// Division admits the entire repeated payload without overflowing a product.
	single := [1]model.Value{fill}
	itemBytes, err := model.ValueBytes(ctx, model.Value{Type: model.TypeArray, ArrayVal: single[:]}, (limit-dimensionBytes)/uint64(count))
	if err != nil {
		return model.Value{}, arrayPartsCopyError(err)
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	owned := model.Value{Type: model.TypeArray, Dimensions: make([]model.ArrayDimension, len(dimensions)), ArrayVal: make([]model.Value, count)}
	copy(owned.Dimensions, dimensions)
	hasPayload := itemBytes > uint64(arrayValueBytes)
	if !hasPayload {
		fill.StrVal, fill.BytesVal, fill.VectorVal, fill.ArrayVal, fill.Dimensions = "", nil, nil, nil, nil
	}
	for start := 0; start < count; start += 256 {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		for i := range min(256, count-start) {
			value := fill
			if hasPayload {
				value, err = copyArrayFillValue(ctx, fill)
				if err != nil {
					return model.Value{}, err
				}
			}
			owned.ArrayVal[start+i] = value
		}
	}
	return owned, nil
}

// copyArrayFillValue copies a tree already admitted by the single-item sizing pass.
func copyArrayFillValue(ctx context.Context, value model.Value) (model.Value, error) {
	owned := value
	owned.StrVal, owned.BytesVal, owned.VectorVal, owned.ArrayVal, owned.Dimensions = "", nil, nil, nil, nil
	if value.StrVal != "" {
		text, err := ownedText(ctx, int64(len(value.StrVal)), value.StrVal)
		if err != nil {
			return model.Value{}, err
		}
		owned.StrVal = text.StrVal
	}
	if len(value.BytesVal) != 0 {
		bytes, err := ownedBytes(ctx, int64(len(value.BytesVal)), value.BytesVal)
		if err != nil {
			return model.Value{}, err
		}
		owned.BytesVal = bytes.BytesVal
	}
	if len(value.VectorVal) != 0 {
		owned.VectorVal = make([]float32, len(value.VectorVal))
		for start := 0; start < len(value.VectorVal); start += cancellationChunk / 4 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			copy(owned.VectorVal[start:], value.VectorVal[start:min(start+cancellationChunk/4, len(value.VectorVal))])
		}
	}
	if len(value.Dimensions) != 0 {
		owned.Dimensions = make([]model.ArrayDimension, len(value.Dimensions))
		for start := 0; start < len(value.Dimensions); start += 256 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			copy(owned.Dimensions[start:], value.Dimensions[start:min(start+256, len(value.Dimensions))])
		}
	}
	if len(value.ArrayVal) != 0 {
		owned.ArrayVal = make([]model.Value, len(value.ArrayVal))
		for start := 0; start < len(value.ArrayVal); start += 256 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			for i := range min(256, len(value.ArrayVal)-start) {
				child, err := copyArrayFillValue(ctx, value.ArrayVal[start+i])
				if err != nil {
					return model.Value{}, err
				}
				owned.ArrayVal[start+i] = child
			}
		}
	}
	return owned, nil
}
