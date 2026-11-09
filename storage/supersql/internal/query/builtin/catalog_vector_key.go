package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (d *keyDecoder) oidVectorCount(mask byte) (uint64, error) {
	count, err := d.number(4, mask)
	if err != nil {
		return 0, err
	}
	if count > math.MaxInt32 || count > uint64(len(d.data)-d.offset)/9 {
		return 0, storage.ErrInvalidEncoding
	}
	return count, nil
}

func (d *keyDecoder) oidVector(ctx context.Context, mask byte) (model.Value, error) {
	count, err := d.oidVectorCount(mask)
	if err != nil {
		return model.Value{}, err
	}
	size := count*uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(model.ArrayDimension{}))
	if err := d.charge(size); err != nil {
		return model.Value{}, err
	}
	result := model.Value{Type: model.TypeArray, ArrayVal: make([]model.Value, int(count)), Dimensions: []model.ArrayDimension{{LowerBound: 0, Length: int32(count)}}}
	for i := range result.ArrayVal {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		marker, err := d.byte(mask)
		if err != nil {
			return model.Value{}, err
		}
		if marker != 1 {
			return model.Value{}, storage.ErrInvalidEncoding
		}
		bits, err := d.number(8, mask)
		if err != nil {
			return model.Value{}, err
		}
		value := bits ^ (uint64(1) << 63)
		if value > math.MaxUint32 {
			return model.Value{}, storage.ErrInvalidEncoding
		}
		result.ArrayVal[i] = model.Int64Val(int64(value))
	}
	return result, nil
}
