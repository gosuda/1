package expr

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func evalArrayIndex(e *evaluation, in instruction) error {
	out := &e.state.registers[in.dst]
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		source := e.state.registers[in.args[0]].value(lane)
		value := model.NullVal(out.vector.Type.Storage)
		if !source.IsNull && len(source.ArrayVal) != 0 && len(source.Dimensions) == len(in.args)-1 {
			offset, valid := uint64(0), true
			for d, r := range in.args[1:] {
				index := e.state.registers[r].value(lane)
				dimension := source.Dimensions[d]
				relative := index.IntVal - int64(dimension.LowerBound)
				if index.IsNull || relative < 0 || relative >= int64(dimension.Length) {
					valid = false
					break
				}
				if offset > (uint64(len(source.ArrayVal))-1-uint64(relative))/uint64(dimension.Length) {
					valid = false
					break
				}
				offset = offset*uint64(dimension.Length) + uint64(relative)
			}
			if valid {
				value = source.ArrayVal[int(offset)]
			}
		}
		out.put(lane, value)
	}
	return e.ctx.Err()
}

func evalArraySlice(e *evaluation, in instruction) error {
	out := &e.state.registers[in.dst]
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		source := e.state.registers[in.args[0]].value(lane)
		if source.IsNull {
			out.put(lane, model.NullVal(model.TypeArray))
			continue
		}
		var lower, upper [6]int64
		for d, dimension := range source.Dimensions {
			lower[d] = int64(dimension.LowerBound)
			upper[d] = lower[d] + int64(dimension.Length) - 1
		}
		null := false
		arg := 1
		for d := uint8(0); d < in.subscripts.Dimensions; d++ {
			if in.subscripts.Upper&(1<<d) != 0 {
				index := e.state.registers[in.args[arg]].value(lane)
				upper[d], null = index.IntVal, null || index.IsNull
				arg++
			}
		}
		for d := uint8(0); d < in.subscripts.Dimensions; d++ {
			if in.subscripts.Lower&(1<<d) != 0 {
				index := e.state.registers[in.args[arg]].value(lane)
				lower[d], null = index.IntVal, null || index.IsNull
				arg++
			}
		}
		if null {
			out.put(lane, model.NullVal(model.TypeArray))
			continue
		}
		rank := len(source.Dimensions)
		if rank == 0 || int(in.subscripts.Dimensions) > rank {
			out.put(lane, model.Value{Type: model.TypeArray})
			continue
		}
		var dimensions [6]model.ArrayDimension
		total := uint64(1)
		for d, dimension := range source.Dimensions {
			lower[d] = max(lower[d], int64(dimension.LowerBound))
			upper[d] = min(upper[d], int64(dimension.LowerBound)+int64(dimension.Length)-1)
			if lower[d] > upper[d] {
				total = 0
				break
			}
			length := upper[d] - lower[d] + 1
			dimensions[d] = model.ArrayDimension{Length: int32(length), LowerBound: 1}
			total *= uint64(length)
		}
		if total == 0 {
			out.put(lane, model.Value{Type: model.TypeArray})
			continue
		}
		dimensionBytes := uint64(rank) * uint64(unsafe.Sizeof(model.ArrayDimension{}))
		width := uint64(unsafe.Sizeof(model.Value{}))
		if total > (math.MaxInt32-dimensionBytes)/width {
			return ErrLimit
		}
		if err := e.charge(total*width + dimensionBytes); err != nil {
			return err
		}
		value := model.Value{Type: model.TypeArray, Dimensions: make([]model.ArrayDimension, rank), ArrayVal: make([]model.Value, int(total))}
		copy(value.Dimensions, dimensions[:rank])
		// Source registers retain borrowed payloads until publication owns its copy.
		out.put(lane, value)
		var strides, cursor [6]int
		stride, offset := 1, 0
		for d := rank - 1; d >= 0; d-- {
			strides[d] = stride
			offset += int(lower[d]-int64(source.Dimensions[d].LowerBound)) * stride
			stride *= int(source.Dimensions[d].Length)
		}
		run := int(dimensions[rank-1].Length)
		for dst := 0; dst < int(total); dst += run {
			for start := 0; start < run; start += 256 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
				end := min(start+256, run)
				copy(value.ArrayVal[dst+start:dst+end], source.ArrayVal[offset+start:offset+end])
			}
			if dst+run == int(total) {
				break
			}
			for d := rank - 2; d >= 0; d-- {
				cursor[d]++
				offset += strides[d]
				if cursor[d] < int(dimensions[d].Length) {
					break
				}
				offset -= cursor[d] * strides[d]
				cursor[d] = 0
			}
		}
	}
	return e.ctx.Err()
}

var (
	errArrayAssignmentSubscript = &model.Error{Code: "2202E", Message: "array subscript error"}
	errArrayAssignmentNull      = &model.Error{Code: "22004", Message: "array subscript in assignment must not be null"}
)

func evalArrayStore(e *evaluation, in instruction) error {
	out := &e.state.registers[in.dst]
	last := len(in.args) - 1
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		source := e.state.registers[in.args[0]].value(lane)
		replacement := e.state.registers[in.args[last]].value(lane)
		var value model.Value
		var err error
		if in.sliceAssignment {
			value, err = arraySliceStore(e, in, lane, source, replacement, e.state.program.registers[in.dst].layout.Children[0].Type.Storage)
		} else {
			var indexes [6]int64
			for i, register := range in.args[1:last] {
				index := e.state.registers[register].value(lane)
				if index.IsNull {
					return errArrayAssignmentNull
				}
				indexes[i] = index.IntVal
			}
			value, err = arrayElementStore(e, source, indexes[:last-1], replacement, e.state.program.registers[in.dst].layout.Children[0].Type.Storage)
		}
		if err != nil {
			return err
		}
		out.put(lane, value)
	}
	return e.ctx.Err()
}

func arrayElementStore(e *evaluation, source model.Value, indexes []int64, replacement model.Value, element model.DataType) (model.Value, error) {
	if len(indexes) == 0 || len(indexes) > 6 {
		return model.Value{}, errArrayAssignmentSubscript
	}
	var storage [6]model.ArrayDimension
	dimensions := storage[:len(indexes)]
	if source.IsNull || len(source.Dimensions) == 0 {
		for i, index := range indexes {
			if index < math.MinInt32 || index > math.MaxInt32 {
				return model.Value{}, errArrayAssignmentSubscript
			}
			dimensions[i] = model.ArrayDimension{Length: 1, LowerBound: int32(index)}
		}
	} else {
		if len(source.Dimensions) != len(indexes) {
			return model.Value{}, errArrayAssignmentSubscript
		}
		copy(dimensions, source.Dimensions)
		for i, index := range indexes {
			lower := int64(dimensions[i].LowerBound)
			upper := lower + int64(dimensions[i].Length) - 1
			lower, upper = min(lower, index), max(upper, index)
			if lower < math.MinInt32 || upper > math.MaxInt32 || upper-lower+1 > math.MaxInt32 {
				return model.Value{}, errArrayAssignmentSubscript
			}
			dimensions[i] = model.ArrayDimension{Length: int32(upper - lower + 1), LowerBound: int32(lower)}
		}
	}
	result, err := allocateArrayStore(e, dimensions, element)
	if err != nil {
		return model.Value{}, err
	}
	if !source.IsNull && len(source.Dimensions) != 0 {
		if err := copyArrayRegion(e.ctx, result.ArrayVal, result.Dimensions, source.ArrayVal, source.Dimensions); err != nil {
			return model.Value{}, err
		}
	}
	offset, ok := arrayCoordinateOffset(dimensions, indexes)
	if !ok {
		return model.Value{}, errArrayAssignmentSubscript
	}
	result.ArrayVal[offset] = replacement
	return result, nil
}

func arraySliceStore(e *evaluation, in instruction, lane int, source, replacement model.Value, element model.DataType) (model.Value, error) {
	if replacement.IsNull || len(replacement.Dimensions) == 0 {
		return model.Value{}, errArrayAssignmentSubscript
	}
	rank := len(source.Dimensions)
	if source.IsNull || rank == 0 {
		rank = len(replacement.Dimensions)
	}
	if rank == 0 || rank > 6 || len(replacement.Dimensions) != rank || int(in.subscripts.Dimensions) > rank {
		return model.Value{}, errArrayAssignmentSubscript
	}
	var lower, upper [6]int64
	for d := 0; d < rank; d++ {
		if !source.IsNull && len(source.Dimensions) != 0 {
			lower[d] = int64(source.Dimensions[d].LowerBound)
			upper[d] = lower[d] + int64(source.Dimensions[d].Length) - 1
		} else {
			lower[d] = int64(replacement.Dimensions[d].LowerBound)
			upper[d] = lower[d] + int64(replacement.Dimensions[d].Length) - 1
		}
	}
	arg := 1
	for d := uint8(0); d < in.subscripts.Dimensions; d++ {
		if in.subscripts.Upper&(1<<d) != 0 {
			index := e.state.registers[in.args[arg]].value(lane)
			if index.IsNull {
				return model.Value{}, errArrayAssignmentNull
			}
			upper[d] = index.IntVal
			arg++
		}
	}
	for d := uint8(0); d < in.subscripts.Dimensions; d++ {
		if in.subscripts.Lower&(1<<d) != 0 {
			index := e.state.registers[in.args[arg]].value(lane)
			if index.IsNull {
				return model.Value{}, errArrayAssignmentNull
			}
			lower[d] = index.IntVal
			arg++
		}
	}
	var targetStorage, dimensionStorage [6]model.ArrayDimension
	target := targetStorage[:rank]
	for d := range rank {
		length := upper[d] - lower[d] + 1
		if length < 0 || length > math.MaxInt32 || lower[d] < math.MinInt32 || lower[d] > math.MaxInt32 || int64(replacement.Dimensions[d].Length) != length {
			return model.Value{}, errArrayAssignmentSubscript
		}
		target[d] = model.ArrayDimension{Length: int32(length), LowerBound: int32(lower[d])}
	}
	dimensions := dimensionStorage[:rank]
	if source.IsNull || len(source.Dimensions) == 0 {
		copy(dimensions, target)
	} else {
		copy(dimensions, source.Dimensions)
		for d := range rank {
			oldLower := int64(dimensions[d].LowerBound)
			oldUpper := oldLower + int64(dimensions[d].Length) - 1
			if rank != 1 && (lower[d] < oldLower || upper[d] > oldUpper) {
				return model.Value{}, errArrayAssignmentSubscript
			}
			nextLower, nextUpper := min(oldLower, lower[d]), max(oldUpper, upper[d])
			if nextLower < math.MinInt32 || nextUpper > math.MaxInt32 || nextUpper-nextLower+1 > math.MaxInt32 {
				return model.Value{}, errArrayAssignmentSubscript
			}
			dimensions[d] = model.ArrayDimension{Length: int32(nextUpper - nextLower + 1), LowerBound: int32(nextLower)}
		}
	}
	result, err := allocateArrayStore(e, dimensions, element)
	if err != nil {
		return model.Value{}, err
	}
	if !source.IsNull && len(source.Dimensions) != 0 {
		if err := copyArrayRegion(e.ctx, result.ArrayVal, result.Dimensions, source.ArrayVal, source.Dimensions); err != nil {
			return model.Value{}, err
		}
	}
	if err := copyArrayRegion(e.ctx, result.ArrayVal, result.Dimensions, replacement.ArrayVal, target); err != nil {
		return model.Value{}, err
	}
	return result, nil
}

func allocateArrayStore(e *evaluation, dimensions []model.ArrayDimension, element model.DataType) (model.Value, error) {
	total := uint64(1)
	for _, dimension := range dimensions {
		if dimension.Length < 0 || dimension.Length != 0 && total > math.MaxUint64/uint64(dimension.Length) {
			return model.Value{}, errArrayAssignmentSubscript
		}
		total *= uint64(dimension.Length)
	}
	if total > math.MaxInt32 {
		return model.Value{}, ErrLimit
	}
	bytes := uint64(len(dimensions))*uint64(unsafe.Sizeof(model.ArrayDimension{})) + total*uint64(unsafe.Sizeof(model.Value{}))
	if err := e.charge(bytes); err != nil {
		return model.Value{}, err
	}
	result := model.Value{Type: model.TypeArray, Dimensions: make([]model.ArrayDimension, len(dimensions)), ArrayVal: make([]model.Value, int(total))}
	copy(result.Dimensions, dimensions)
	for i := range result.ArrayVal {
		result.ArrayVal[i] = model.NullVal(element)
	}
	return result, nil
}

func copyArrayRegion(ctx context.Context, destination []model.Value, destinationShape []model.ArrayDimension, source []model.Value, sourceShape []model.ArrayDimension) error {
	if len(destinationShape) != len(sourceShape) {
		return errArrayAssignmentSubscript
	}
	var storage [6]int64
	coordinate := storage[:len(sourceShape)]
	for offset, value := range source {
		if offset&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		remainder := offset
		for d := len(sourceShape) - 1; d >= 0; d-- {
			length := int(sourceShape[d].Length)
			if length <= 0 {
				return errArrayAssignmentSubscript
			}
			coordinate[d] = int64(sourceShape[d].LowerBound) + int64(remainder%length)
			remainder /= length
		}
		target, ok := arrayCoordinateOffset(destinationShape, coordinate)
		if !ok {
			return errArrayAssignmentSubscript
		}
		destination[target] = value
	}
	return nil
}

func arrayCoordinateOffset(dimensions []model.ArrayDimension, coordinate []int64) (int, bool) {
	if len(dimensions) != len(coordinate) {
		return 0, false
	}
	offset := uint64(0)
	for d, dimension := range dimensions {
		relative := coordinate[d] - int64(dimension.LowerBound)
		if relative < 0 || relative >= int64(dimension.Length) {
			return 0, false
		}
		offset = offset*uint64(dimension.Length) + uint64(relative)
	}
	if offset > math.MaxInt32 {
		return 0, false
	}
	return int(offset), true
}
