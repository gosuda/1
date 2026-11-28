package aggregate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func validateArrayAggregateLayouts(op operation, input, output model.TypeLayout) error {
	if err := input.Validate(); err != nil {
		return err
	}
	if err := output.Validate(); err != nil {
		return err
	}
	if output.Type.Storage != model.TypeArray || output.Type.Modifier != -1 {
		return columnar.ErrValueLayout
	}
	if op == arrayAggregateArrays {
		input.Type.Modifier = -1
		if input.Type.Storage != model.TypeArray || !model.TypeLayoutsEqual(input, output) {
			return columnar.ErrValueLayout
		}
		return nil
	}
	typ, ok := builtin.LookupType(input.Type.ID)
	if !ok || typ.Array == 0 || input.Type.Storage == model.TypeArray {
		return columnar.ErrValueLayout
	}
	wrongIdentity := output.Type.ID != typ.Array || output.Type.Collation != input.Type.Collation
	if wrongIdentity || !model.TypeLayoutsEqual(input, output.Children[0]) {
		return columnar.ErrValueLayout
	}
	return nil
}

func arrayAggregateShape(state *transition, value model.Value) error {
	if value.IsNull {
		return &model.Error{Code: "22004", Message: "cannot accumulate null arrays"}
	}
	if state.count == 0 && len(value.Dimensions) >= 6 {
		return &model.Error{Code: "54000", Message: "number of array dimensions exceeds the maximum allowed (6)"}
	}
	if len(value.ArrayVal) == 0 {
		return &model.Error{Code: "2202E", Message: "cannot accumulate empty arrays"}
	}
	if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
		return &model.Error{Code: "2202E", Message: "cannot accumulate arrays of different dimensionality", Cause: err}
	}
	if state.count != 0 && !slices.Equal(value.Dimensions, state.value.Dimensions[1:]) {
		return &model.Error{Code: "2202E", Message: "cannot accumulate arrays of different dimensionality"}
	}
	return nil
}

func arrayAggregateResultBounds(dimensions []model.ArrayDimension) error {
	// PostgreSQL defers this check until finalization, after all input-shape errors.
	for _, dimension := range dimensions {
		if int64(dimension.LowerBound)+int64(dimension.Length) > math.MaxInt32 {
			return &model.Error{Code: "54000", Message: fmt.Sprintf("array lower bound is too large: %d", dimension.LowerBound)}
		}
	}
	return nil
}

func arrayAggregateElement(value model.Value, storage model.DataType) model.Value {
	if value.IsNull {
		return model.NullVal(storage)
	}
	return value
}

func arrayAggregateCopyError(err error) error {
	if errors.Is(err, model.ErrValueBudget) {
		return resource("array_agg result exceeds byte limit", err)
	}
	return err
}

func (c *cursor) advanceArrayAggregate(ctx context.Context, call compiledCall, state *transition, value model.Value) error {
	layout := c.layouts[call.arguments[0]]
	dimensions := 1
	var scalar [1]model.Value
	scalar[0] = value
	elements := scalar[:]
	elementStorage := layout.Type.Storage
	if call.op == arrayAggregateArrays {
		if err := arrayAggregateShape(state, value); err != nil {
			return err
		}
		dimensions += len(value.Dimensions)
		elements = value.ArrayVal
		elementStorage = layout.Children[0].Type.Storage
	}
	if err := validateArgumentValue(ctx, value, layout); err != nil {
		return err
	}
	if state.count == math.MaxInt32 {
		return &model.Error{Code: "54000", Message: "array size exceeds the maximum allowed"}
	}

	const headerBytes = uint64(unsafe.Sizeof(model.Value{}))
	fixed := uint64(unsafe.Sizeof(transition{})) + uint64(dimensions)*uint64(unsafe.Sizeof(model.ArrayDimension{}))
	limit := c.options.MaxRecordBytes * 8
	remaining := limit - fixed - state.arrayBytes
	payload := uint64(0)
	for _, element := range elements {
		if err := c.check(ctx); err != nil {
			return err
		}
		used, err := model.ValueBytes(ctx, arrayAggregateElement(element, elementStorage), remaining-payload)
		if err != nil {
			return arrayAggregateCopyError(err)
		}
		payload += used
	}
	maxCapacity := int((remaining - payload) / headerBytes)
	old := state.value.ArrayVal
	if len(old) > maxCapacity || len(elements) > maxCapacity-len(old) {
		return resource("array_agg result exceeds byte limit", model.ErrValueBudget)
	}
	length := len(old) + len(elements)
	buffer := old
	if length > cap(old) || cap(old) > maxCapacity {
		// Clip growth after measuring payload. Shrinking can recover spare
		// headers when a later element needs more deep-payload space.
		capacity := min(maxCapacity, max(length, max(8, cap(old)*2)))
		buffer = make([]model.Value, length, capacity)
		for offset := 0; offset < len(old); offset += 256 {
			if err := c.check(ctx); err != nil {
				clear(buffer)
				return err
			}
			end := min(offset+256, len(old))
			copy(buffer[offset:end], old[offset:end])
		}
	} else {
		buffer = buffer[:length]
	}
	// Publish only after the last copy and cancellation check. A reused
	// buffer must not retain pointers written into its unpublished tail.
	tail := buffer[len(old):]
	copiedBytes := uint64(0)
	for i, element := range elements {
		if err := c.check(ctx); err != nil {
			clear(tail)
			return err
		}
		owned, used, err := model.CopyValue(ctx, arrayAggregateElement(element, elementStorage), payload-copiedBytes)
		if err != nil {
			clear(tail)
			return arrayAggregateCopyError(err)
		}
		tail[i] = owned
		copiedBytes += used
	}
	if err := c.check(ctx); err != nil {
		clear(tail)
		return err
	}
	shape := state.value.Dimensions
	if state.count == 0 {
		shape = make([]model.ArrayDimension, dimensions)
		shape[0].LowerBound = 1
		if call.op == arrayAggregateArrays {
			copy(shape[1:], value.Dimensions)
		}
	}
	shape[0].Length = int32(state.count + 1)
	if len(old) != 0 && &buffer[0] != &old[0] {
		clear(old[:cap(old)])
	}
	state.value = model.Value{Type: model.TypeArray, ArrayVal: buffer, Dimensions: shape}
	state.arrayBytes += copiedBytes
	state.count++
	return nil
}

func (g *group) clearArrays() {
	for i := range g.states {
		value := &g.states[i].value
		if value.Type == model.TypeArray {
			clear(value.ArrayVal[:cap(value.ArrayVal)])
			value.ArrayVal, value.Dimensions = nil, nil
		}
	}
}
