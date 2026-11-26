package builtin

import (
	"context"
	"encoding/binary"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// encodeMultirange borrows an already sorted, disjoint, non-adjacent sequence.
// Normalization belongs to the constructor/algebra, not this storage codec.
func encodeMultirange(ctx context.Context, family RangeFamilyInfo, values []rangeDatum, budget CallBudget) (model.Value, error) {
	if uint64(len(values)) > math.MaxInt32 {
		return model.Value{}, sqlError("54000", "multirange count exceeds encoding limit")
	}
	size := uint64(4)
	for i, value := range values {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		if value.empty {
			return model.Value{}, invalidRangeValue()
		}
		length, err := rangeEncodedSize(family, value)
		if err != nil {
			return model.Value{}, err
		}
		if length > math.MaxUint32 || size > math.MaxUint64-4-length {
			return model.Value{}, sqlError("54000", "multirange exceeds encoding limit")
		}
		size += 4 + length
	}
	buffer, err := newByteResult(ctx, size, budget)
	if err != nil {
		return model.Value{}, err
	}
	defer buffer.close()
	output := binary.BigEndian.AppendUint32(buffer.data[:0], uint32(len(values)))
	for _, value := range values {
		length, err := rangeEncodedSize(family, value)
		if err != nil {
			return model.Value{}, err
		}
		output = binary.BigEndian.AppendUint32(output, uint32(length))
		output, err = appendRangeBytes(ctx, output, family, value)
		if err != nil {
			return model.Value{}, err
		}
	}
	return buffer.value(), ctx.Err()
}
