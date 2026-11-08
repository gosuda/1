package expr

import (
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errArrayStackShape = &model.Error{Code: "2202E", Message: "multidimensional arrays must have array expressions with matching dimensions"}
	errArrayStackRank  = &model.Error{Code: "54000", Message: "number of array dimensions (7) exceeds the maximum allowed (6)"}
	errArrayStackSize  = &model.Error{Code: "54000", Message: "array size exceeds the maximum allowed"}
)

func evalArrayStack(e *evaluation, in instruction) error {
	out := &e.state.registers[in.dst]
	for _, lane := range e.lanes() {
		var inner []model.ArrayDimension
		elements := 0
		haveEmpty := false
		for i, r := range in.args {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			child := e.state.registers[r].value(lane)
			if child.IsNull || len(child.Dimensions) == 0 {
				haveEmpty = true
				continue
			}
			if inner == nil {
				if len(child.Dimensions) >= 6 {
					return errArrayStackRank
				}
				inner, elements = child.Dimensions, len(child.ArrayVal)
				continue
			}
			if !slices.Equal(inner, child.Dimensions) {
				return errArrayStackShape
			}
		}
		if inner == nil {
			out.put(lane, model.Value{Type: model.TypeArray})
			continue
		}
		if haveEmpty {
			return errArrayStackShape
		}
		if uint64(len(in.args)) >= math.MaxInt32 || uint64(elements) > math.MaxInt32/uint64(len(in.args)) {
			return errArrayStackSize
		}
		total := uint64(elements) * uint64(len(in.args))
		dimensionBytes := uint64(len(inner)+1) * uint64(unsafe.Sizeof(model.ArrayDimension{}))
		width := uint64(unsafe.Sizeof(model.Value{}))
		if total > (math.MaxInt32-dimensionBytes)/width {
			return errArrayStackSize
		}
		// PostgreSQL checks one-past-last only when constructing the result.
		for _, dimension := range inner {
			if int64(dimension.LowerBound)+int64(dimension.Length) > math.MaxInt32 {
				return &model.Error{Code: "54000", Message: fmt.Sprintf("array lower bound is too large: %d", dimension.LowerBound)}
			}
		}
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if err := e.charge(total*width + dimensionBytes); err != nil {
			return err
		}
		value := model.Value{Type: model.TypeArray, Dimensions: make([]model.ArrayDimension, len(inner)+1), ArrayVal: make([]model.Value, int(total))}
		value.Dimensions[0] = model.ArrayDimension{Length: int32(len(in.args)), LowerBound: 1}
		copy(value.Dimensions[1:], inner)
		// Child registers retain borrowed payloads until the output builder owns its copy.
		out.put(lane, value)
		offset := 0
		for _, r := range in.args {
			child := e.state.registers[r].value(lane)
			for start := 0; start < len(child.ArrayVal); start += 256 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
				end := min(start+256, len(child.ArrayVal))
				copy(value.ArrayVal[offset+start:offset+end], child.ArrayVal[start:end])
			}
			offset += len(child.ArrayVal)
		}
	}
	return e.ctx.Err()
}
