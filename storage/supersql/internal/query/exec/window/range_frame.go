package window

import (
	"cmp"
	"context"
	"math"
	"reflect"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (c *cursor) validateRangeKey(index int, offset *model.Value) error {
	if len(c.options.OrderBy) != 1 {
		return runtime.ErrInvalidConfig
	}
	key := c.options.OrderBy[0]
	if key.Column < 0 || key.Column >= len(c.options.InputLayouts) {
		return runtime.ErrInvalidConfig
	}
	input := c.options.InputLayouts[key.Column]
	support := c.options.RangeSupport[index]
	catalog, exists := builtin.LookupInRangeSupport(support.Function)
	if !exists || catalog.Input != input.Type.ID || catalog.Offset != support.Offset.Type.ID {
		return &UnsupportedError{Function: support.Function, Reason: "offset RANGE requires an exact B-tree in_range support procedure"}
	}
	if !reflect.DeepEqual(support.Input, input) || support.InputCollation != key.InputCollation || offset == nil || offset.Type != support.Offset.Type.Storage {
		return columnar.ErrValueLayout
	}
	signature, exists := builtin.LookupSignature(support.Function)
	if !exists || signature.ArgumentCount != 5 || signature.Arguments[0] != catalog.Input || signature.Arguments[1] != catalog.Input || signature.Arguments[2] != catalog.Offset || signature.Arguments[3] != 16 || signature.Arguments[4] != 16 || signature.Result != 16 || !signature.Strict {
		return runtime.ErrInvalidConfig
	}
	switch support.Function {
	case 4141:
		if input.Type.Storage != model.TypeNumeric {
			return columnar.ErrValueLayout
		}
		c.rangeNumeric = true
	case 4139, 4140:
		if input.Type.Storage != model.TypeFloat64 {
			return columnar.ErrValueLayout
		}
		c.rangeFloat = true
	case 4126, 4127, 4128, 4129, 4130, 4131, 4132:
		if input.Type.Storage != model.TypeInt64 {
			return columnar.ErrValueLayout
		}
	case 4133, 4134, 4135, 4136, 4137, 4138:
		if offset.IsNull {
			return frameOffsetNull(index)
		}
		if err := builtin.ValidateTemporalInRangeOffset(support.Function, offset.IntervalVal); err != nil {
			return err
		}
		options := c.options.Builtin
		options.Collation = support.InputCollation
		if index == 1 && c.options.RangeSupport[0].Function == support.Function && c.options.RangeSupport[0].InputCollation == support.InputCollation {
			c.rangeCallables[index] = c.rangeCallables[0]
		} else {
			callable, err := builtin.Resolve(support.Function, options)
			if err != nil {
				return err
			}
			c.rangeCallables[index] = callable
			c.rangeMetadata += callable.OwnedBytes()
		}
		c.rangeValues[index] = *offset
		c.rangeTemporal = true
	default:
		return &UnsupportedError{Function: support.Function, Reason: "offset RANGE support procedure is not executable"}
	}
	return nil
}

func (c *cursor) floatRangeOffset(value *model.Value, index int) error {
	if value == nil || value.IsNull || value.Type != model.TypeFloat64 {
		return &model.Error{Code: "22013", Message: "window frame offset must be a nonnegative floating-point value"}
	}
	if math.IsNaN(value.FloatVal) || value.FloatVal < 0 {
		return &model.Error{Code: "22013", Message: "invalid preceding or following size in window function"}
	}
	c.rangeOffsets[index] = value.FloatVal
	return nil
}

type rangeBoundary struct {
	integer            int64
	overflow           int
	floating           float64
	isFloat, allNonNaN bool
	numeric            *builtin.NumericRange
}

func (c *cursor) makeRangeBoundary(subtract, end bool, offset int64) rangeBoundary {
	base := c.current[c.options.OrderBy[0].Column]
	if !c.rangeFloat {
		value, overflow := integerRangeBoundary(base.IntVal, offset, subtract)
		return rangeBoundary{integer: value, overflow: overflow}
	}
	amount := c.rangeOffsets[0]
	if end {
		amount = c.rangeOffsets[1]
	}
	boundary := rangeBoundary{isFloat: true, floating: base.FloatVal}
	if math.IsNaN(base.FloatVal) {
		return boundary
	}
	indeterminate := math.IsInf(amount, 1) && math.IsInf(base.FloatVal, 0)
	opposing := subtract && base.FloatVal > 0 || !subtract && base.FloatVal < 0
	if indeterminate && opposing {
		boundary.allNonNaN = true
		return boundary
	}
	if subtract {
		boundary.floating -= amount
	} else {
		boundary.floating += amount
	}
	return boundary
}

// Overflow remains an ordered virtual endpoint, not a wrapped integer or SQL
// arithmetic error. Every finite integer lies between the virtual endpoints.
func integerRangeBoundary(base, offset int64, subtract bool) (value int64, overflow int) {
	if subtract {
		if base < math.MinInt64+offset {
			return 0, -1
		}
		return base - offset, 0
	}
	if base > math.MaxInt64-offset {
		return 0, 1
	}
	return base + offset, 0
}

func compareRangeValue(ctx context.Context, value model.Value, boundary rangeBoundary, key OrderKey) (int, error) {
	if value.IsNull {
		if key.NullsFirst {
			return -1, nil
		}
		return 1, nil
	}
	order := -boundary.overflow
	if boundary.numeric != nil {
		var err error
		order, err = boundary.numeric.Compare(ctx, value)
		if err != nil {
			return 0, err
		}
	} else if boundary.isFloat {
		order = compareFloatRange(value.FloatVal, boundary)
	} else if boundary.overflow == 0 {
		order = cmp.Compare(value.IntVal, boundary.integer)
	}
	if key.Descending {
		return -order, nil
	}
	return order, nil
}

func compareFloatRange(value float64, boundary rangeBoundary) int {
	if math.IsNaN(value) {
		if math.IsNaN(boundary.floating) {
			return 0
		}
		return 1
	}
	if math.IsNaN(boundary.floating) {
		return -1
	}
	if boundary.allNonNaN {
		return 0
	}
	return cmp.Compare(value, boundary.floating)
}

func (c *cursor) rangeBound(ctx context.Context, kind ir.BoundKind, offset int64, end bool) (int64, error) {
	key := c.options.OrderBy[0]
	subtract := kind == ir.OffsetPreceding
	if key.Descending {
		subtract = !subtract
	}
	var boundary rangeBoundary
	if c.rangeNumeric {
		index := 0
		if end {
			index = 1
		}
		boundary.numeric = c.numericRanges[index]
		if err := boundary.numeric.SetBase(ctx, c.current[key.Column], subtract); err != nil {
			return 0, err
		}
	} else if !c.rangeTemporal {
		boundary = c.makeRangeBoundary(subtract, end, offset)
	}
	lo, hi := int64(0), int64(c.data.Rows())
	for lo < hi {
		if err := c.check(ctx); err != nil {
			return 0, err
		}
		middle := lo + (hi-lo)/2
		position, _, err := c.rows.read(ctx, middle)
		if err != nil {
			return 0, err
		}
		row, err := c.reader.Read(ctx, rowstore.Position(position))
		if err != nil {
			return 0, err
		}
		var before bool
		if c.rangeTemporal {
			before, err = c.temporalRangeBefore(ctx, row[key.Column], kind, end)
		} else {
			var order int
			order, err = compareRangeValue(ctx, row[key.Column], boundary, key)
			before = order < 0 || end && order == 0
		}
		if err != nil {
			return 0, err
		}
		if before {
			lo = middle + 1
		} else {
			hi = middle
		}
	}
	return lo, nil
}

func (c *cursor) temporalRangeBefore(ctx context.Context, candidate model.Value, kind ir.BoundKind, end bool) (bool, error) {
	key := c.options.OrderBy[0]
	if candidate.IsNull {
		return key.NullsFirst, nil
	}
	index := 0
	if end {
		index = 1
	}
	subtract := kind == ir.OffsetPreceding
	less := end
	if key.Descending {
		subtract = !subtract
		less = !less
	}
	arguments := [5]model.Value{
		candidate,
		c.current[key.Column],
		c.rangeValues[index],
		model.BoolVal(subtract),
		model.BoolVal(less),
	}
	result, err := c.rangeCallables[index].Call(ctx, arguments[:], builtin.CallBudget{})
	if err != nil {
		return false, err
	}
	if result.IsNull || result.Type != model.TypeBoolean {
		return false, runtime.ErrInvalidConfig
	}
	inRange := result.IntVal != 0
	if end {
		return inRange, nil
	}
	return !inRange, nil
}
