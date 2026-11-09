package model

import (
	"errors"
	"math"
)

var ErrInvalidArrayShape = errors.New("invalid SQL array dimensions")

// ValidateArrayShape checks representability: at most six dimensions, int32
// subscripts, and lengths covering exactly the element count. Empty catalog
// vectors retain one zero-length dimension; ordinary empty arrays have none.
// Constructing operators may impose stricter bound limits.
func ValidateArrayShape(dimensions []ArrayDimension, elements uint64) error {
	if len(dimensions) > 6 {
		return ErrInvalidArrayShape
	}
	if elements == 0 {
		if len(dimensions) == 0 {
			return nil
		}
		if len(dimensions) != 1 || dimensions[0].Length != 0 || dimensions[0].LowerBound == math.MinInt32 {
			return ErrInvalidArrayShape
		}
		return nil
	}
	if len(dimensions) == 0 {
		return ErrInvalidArrayShape
	}
	count := uint64(1)
	for _, dim := range dimensions {
		if dim.Length <= 0 {
			return ErrInvalidArrayShape
		}
		if int64(dim.LowerBound)+int64(dim.Length)-1 > math.MaxInt32 {
			return ErrInvalidArrayShape
		}
		if uint64(dim.Length) > elements/count {
			return ErrInvalidArrayShape
		}
		count *= uint64(dim.Length)
	}
	if count != elements {
		return ErrInvalidArrayShape
	}
	return nil
}
