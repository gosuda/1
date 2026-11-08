package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type nullCountCodec struct {
	array          bool
	elementStorage model.DataType
	nonnull        bool
}

func resolveNullCount(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64, array bool) (Callable, error) {
	// Child descriptors are validated but not retained by NULL-only operations.
	element, err := validateVariadicLayouts(ctx, signature, arguments, result, math.MaxUint64, 0, array, options)
	if err != nil {
		return Callable{}, err
	}
	if result.Type.ID != 23 || result.Type.Storage != model.TypeInt64 {
		return Callable{}, sqlError("42804", "null count requires int4 result")
	}
	metadata := uint64(unsafe.Sizeof(nullCountCodec{})) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	metadata += uint64(len(arguments)) * uint64(unsafe.Sizeof(model.Type{}))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "null count metadata budget exceeded")
	}
	codec := &nullCountCodec{array: array, elementStorage: element.Type.Storage, nonnull: signature.ID == 440}
	types := make([]model.Type, len(arguments))
	for i, argument := range arguments {
		types[i] = argument.Type
	}
	return Callable{
		signature: signature, kernel: codec.count, resultStorage: model.TypeInt64,
		argumentTypes: types, metadataBytes: metadata,
	}, nil
}

func (codec *nullCountCodec) count(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
	values, err := decodeVariadicValues(arguments, 0, codec.array)
	if err != nil {
		return model.Value{}, err
	}
	if values.nullArray {
		return model.NullVal(model.TypeInt64), nil
	}
	// PG ArrayGetNItems caps the count by MaxAllocSize / sizeof(Datum),
	// which also keeps both counters representable as int4.
	if len(values.values) > (1<<30-1)/8 {
		return model.Value{}, sqlError("54000", "array size exceeds the maximum allowed")
	}
	nulls := int32(0)
	for i := range values.values {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		value := &values.values[i]
		if codec.array && value.Type != codec.elementStorage {
			return model.Value{}, sqlError("42804", "variadic element storage differs from its SQL type")
		}
		if value.IsNull {
			nulls++
		}
	}
	if codec.nonnull {
		return model.Int64Val(int64(len(values.values)) - int64(nulls)), nil
	}
	return model.Int64Val(int64(nulls)), nil
}
