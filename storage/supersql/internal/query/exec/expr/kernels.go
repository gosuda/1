package expr

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func decode(op opcode) batchStep {
	switch op {
	case opInit:
		return evalInit
	case opConst, opParam:
		return evalConstant
	case opInput:
		return evalInput
	case opCapture:
		return evalCapture
	case opCall, opSQLJSON:
		return evalCall
	case opSubquery:
		return evalSubquery
	case opArrayQuantified:
		return evalArrayQuantified
	case opSequence:
		return evalSequence
	case opRelation:
		return evalRelation
	case opMinMax:
		return evalMinMax
	case opRowCompare:
		return evalRowCompare
	case opCopy, opCollate:
		return evalCopy
	case opRelabel:
		return evalRelabel
	case opNull, opNot, opMergeAnd, opMergeOr:
		return evalBoolean
	case opEnter, opFilter, opLeave:
		return evalMask
	case opRow, opArray:
		return evalNested
	case opArrayStack:
		return evalArrayStack
	case opArrayCoerce:
		return evalArrayCoerce
	case opArrayIndex:
		return evalArrayIndex
	case opArraySlice:
		return evalArraySlice
	case opField:
		return evalField
	case opFieldStore:
		return evalFieldStore
	case opArrayStore:
		return evalArrayStore
	case opJSONBStore:
		return evalJSONBStore
	case opOnceEnter, opOnceStore, opOnceRead:
		return evalOnce
	default:
		return nil
	}
}

func evalField(e *evaluation, in instruction) error {
	out, parent := &e.state.registers[in.dst], &e.state.registers[in.args[0]]
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		if parent.null(lane) {
			out.put(lane, model.NullVal(out.vector.Type.Storage))
			continue
		}
		out.put(lane, parent.values[lane].ArrayVal[in.index])
	}
	return nil
}

func evalFieldStore(e *evaluation, in instruction) error {
	out := &e.state.registers[in.dst]
	layout := &e.state.program.registers[in.dst].layout
	parent := &e.state.registers[in.args[0]]
	replacement := &e.state.registers[in.args[1]]
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if err := e.charge(uint64(len(layout.Children)) * uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return err
		}
		value := model.Value{Type: model.TypeComposite, ArrayVal: make([]model.Value, len(layout.Children))}
		if parent.null(lane) {
			for i, child := range layout.Children {
				value.ArrayVal[i] = model.NullVal(child.Type.Storage)
			}
		} else {
			copy(value.ArrayVal, parent.values[lane].ArrayVal)
		}
		value.ArrayVal[in.index] = replacement.value(lane)
		out.put(lane, value)
	}
	return nil
}

func evalInit(e *evaluation, in instruction) error {
	r := &e.state.registers[in.dst]
	value := model.NullVal(r.vector.Type.Storage)
	if in.index != 0 {
		value = model.BoolVal(in.index == 1)
	}
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		r.put(lane, value)
	}
	return nil
}

func evalConstant(e *evaluation, in instruction) error {
	value := model.Value{}
	if in.op == opConst {
		value = e.state.program.constants[in.index]
	} else {
		value = e.parameters[in.index].Value
	}
	if err := validateValue(e.ctx, value, e.state.program.registers[in.dst].layout, 0); err != nil {
		return err
	}
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		e.state.registers[in.dst].put(lane, value)
	}
	return nil
}

func evalCapture(e *evaluation, in instruction) error {
	if len(e.lanes()) == 0 {
		return nil
	}
	capture := e.state.program.captures[in.index]
	batch := e.state.options.Captures
	input := &batch.Columns[capture.Column]
	row := batch.Lane(0)
	bytes, err := columnar.ValueBytes(e.ctx, input, row, e.remaining())
	if err != nil {
		return err
	}
	if err := e.charge(bytes); err != nil {
		return err
	}
	value, _, err := columnar.CopyValue(e.ctx, input, row, bytes)
	if err != nil {
		return err
	}
	if err := validateValue(e.ctx, value, capture.layout, 0); err != nil {
		return err
	}
	if value.IsNull && !capture.nullable {
		return invalid("non-nullable capture contains NULL")
	}
	// Registers and scalar capabilities borrow this one immutable payload copy.
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		e.state.registers[in.dst].put(lane, value)
	}
	return nil
}

func evalInput(e *evaluation, in instruction) error {
	input := &e.input.Columns[e.state.program.inputs[in.index].Column]
	out := &e.state.registers[in.dst]
	if !fixed(input.Type.Storage) {
		for _, lane := range e.lanes() {
			bytes, err := columnar.ValueBytes(e.ctx, input, e.input.Lane(lane), e.remaining())
			if err != nil {
				return err
			}
			if err := e.charge(bytes); err != nil {
				return err
			}
			value, _, err := columnar.CopyValue(e.ctx, input, e.input.Lane(lane), bytes)
			if err != nil {
				return err
			}
			if err := validateValue(e.ctx, value, e.state.program.registers[in.dst].layout, 0); err != nil {
				return err
			}
			out.put(lane, value)
		}
		return nil
	}
	// Flatten selected encodings once, before any arithmetic kernel is dispatched.
	switch input.Encoding {
	case columnar.Constant:
		for i, lane := range e.lanes() {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			if err := copyFixed(e, out, input, lane, 0, 0); err != nil {
				return err
			}
		}
	case columnar.Flat:
		for i, lane := range e.lanes() {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			index := e.input.Lane(lane) + input.Offset
			if err := copyFixed(e, out, input, lane, index, index); err != nil {
				return err
			}
		}
	case columnar.Dictionary:
		for i, lane := range e.lanes() {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			index := e.input.Lane(lane) + input.Offset
			payload := 0
			if len(input.Nulls) == 0 || input.Nulls[index/8]&(1<<uint(index%8)) == 0 {
				payload = int(input.Indices[index])
			}
			if err := copyFixed(e, out, input, lane, index, payload); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFixed(e *evaluation, out *laneRegister, input *columnar.Vector, lane, nullIndex, index int) error {
	value := model.Value{Type: input.Type.Storage}
	if len(input.Nulls) != 0 && input.Nulls[nullIndex/8]&(1<<uint(nullIndex%8)) != 0 {
		value.IsNull = true
		out.put(lane, value)
		return nil
	}
	if input.Type.Storage == model.TypeFloat64 {
		value.FloatVal = input.Float64[index]
	} else {
		value.IntVal = input.Int64[index]
	}
	if err := validateValue(e.ctx, value, model.TypeLayout{Type: input.Type}, 0); err != nil {
		return err
	}
	out.put(lane, value)
	return nil
}

func evalRelabel(e *evaluation, in instruction) error {
	source := e.state.program.registers[in.args[0]].layout.Type.ID
	destination := e.state.program.registers[in.dst].layout.Type.ID
	toOID := source == 23 && builtin.IsOIDType(destination)
	toInt4 := builtin.IsOIDType(source) && destination == 23
	if !toOID && !toInt4 {
		return evalCopy(e, in)
	}
	out, input := &e.state.registers[in.dst], &e.state.registers[in.args[0]]
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		value := input.value(lane)
		if !value.IsNull {
			value.IntVal = int64(uint32(value.IntVal))
			if toInt4 {
				value.IntVal = int64(int32(value.IntVal))
			}
		}
		out.put(lane, value)
	}
	return nil
}

func evalCopy(e *evaluation, in instruction) error {
	out, input := &e.state.registers[in.dst], &e.state.registers[in.args[0]]
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		out.put(lane, input.value(lane))
	}
	return nil
}

func evalCall(e *evaluation, in instruction) error {
	if len(e.lanes()) == 0 {
		return nil
	}
	out := &e.state.registers[in.dst]
	if in.vector != nil && e.mode != ScalarCalls {
		args := e.vectors[:len(in.args)]
		for i, r := range in.args {
			args[i] = &e.state.registers[r].vector
		}
		return in.vector(e.ctx, e.lanes(), args, &out.vector)
	}
	args := e.args[:len(in.args)]
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		hasNull := false
		for i, r := range in.args {
			args[i] = e.state.registers[r].value(lane)
			hasNull = hasNull || args[i].IsNull
		}
		if in.negated && hasNull {
			out.put(lane, model.NullVal(out.vector.Type.Storage))
			continue
		}
		value, err := e.invokeScalar(in.call, args, e.state.program.registers[in.dst], in.ownsResult)
		if err != nil {
			return err
		}
		out.put(lane, value)
	}
	return e.ctx.Err()
}

func (e *evaluation) retainCallResult(value model.Value, owned bool, budget uint64) (model.Value, error) {
	if fixed(value.Type) || value.IsNull {
		return value, nil
	}
	bytes, err := valueBytes(e.ctx, value, budget)
	if err != nil {
		return model.Value{}, err
	}
	if err := e.charge(bytes); err != nil {
		return model.Value{}, err
	}
	if owned {
		return value, nil
	}
	if err := e.charge(bytes); err != nil {
		return model.Value{}, err
	}
	value, _, err = model.CopyValue(e.ctx, value, bytes)
	if err != nil {
		return model.Value{}, err
	}
	// The copy no longer references the capability's temporary result.
	if e.state.work != nil && !e.callAdmission {
		if err := e.state.work.Resize(e.used - bytes); err != nil {
			return model.Value{}, err
		}
	}
	e.used -= bytes
	return value, nil
}

func booleanIs(value model.Value, truth bool) bool {
	return !value.IsNull && (value.IntVal == 1) == truth
}

func nullTest(value model.Value, negated bool) bool {
	if value.Type != model.TypeComposite || value.IsNull {
		return value.IsNull != negated
	}
	for _, field := range value.ArrayVal {
		if field.IsNull == negated {
			return false
		}
	}
	return true
}

func evalBoolean(e *evaluation, in instruction) error {
	out, input := &e.state.registers[in.dst], &e.state.registers[in.args[0]]
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		value := input.value(lane)
		switch in.op {
		case opNull:
			out.put(lane, model.BoolVal(nullTest(value, in.negated)))
		case opNot:
			if !value.IsNull {
				value.IntVal = 1 - value.IntVal
			}
			out.put(lane, value)
		case opMergeAnd:
			previous := out.value(lane)
			if booleanIs(value, false) || booleanIs(previous, false) {
				out.put(lane, model.BoolVal(false))
			} else if value.IsNull || previous.IsNull {
				out.put(lane, model.NullVal(model.TypeBoolean))
			} else {
				out.put(lane, model.BoolVal(true))
			}
		case opMergeOr:
			previous := out.value(lane)
			if booleanIs(value, true) || booleanIs(previous, true) {
				out.put(lane, model.BoolVal(true))
			} else if value.IsNull || previous.IsNull {
				out.put(lane, model.NullVal(model.TypeBoolean))
			} else {
				out.put(lane, model.BoolVal(false))
			}
		}
	}
	return nil
}

func evalMask(e *evaluation, in instruction) error {
	if in.op == opLeave {
		e.depth--
		return nil
	}
	input := e.lanes()
	if in.op == opEnter {
		e.depth++
	}
	output := e.masks[e.depth][:0]
	if in.mask == maskAll {
		output = append(output, input...)
	} else {
		r := &e.state.registers[in.args[0]]
		for i, lane := range input {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			null := r.null(lane)
			keep := false
			switch in.mask {
			case maskTrue:
				keep = !null && r.vector.Int64[lane] != 0
			case maskNotTrue:
				keep = null || r.vector.Int64[lane] == 0
			case maskNull:
				keep = null
			case maskNotNull:
				keep = !null
			case maskAnd:
				keep = null || r.vector.Int64[lane] != 0
			case maskOr:
				keep = null || r.vector.Int64[lane] == 0
			case maskZero:
				keep = !null && r.vector.Int64[lane] == 0
			}
			if keep {
				output = append(output, lane)
			}
		}
	}
	e.masks[e.depth] = output
	return nil
}

func evalOnce(e *evaluation, in instruction) error {
	cache := &e.state.registers[in.index]
	if in.op == opOnceEnter {
		input := e.lanes()
		e.depth++
		output := e.masks[e.depth][:0]
		for i, lane := range input {
			if i&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			if cache.computed[lane/8]&(1<<uint(lane%8)) == 0 {
				output = append(output, lane)
			}
		}
		e.masks[e.depth] = output
		return nil
	}
	if in.op == opOnceRead {
		return evalCopy(e, instruction{dst: in.dst, args: []int{in.index}})
	}
	input := &e.state.registers[in.args[0]]
	for i, lane := range e.lanes() {
		if i&255 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		cache.put(lane, input.value(lane))
		cache.computed[lane/8] |= 1 << uint(lane%8)
	}
	e.depth--
	return nil
}

func evalNested(e *evaluation, in instruction) error {
	out := &e.state.registers[in.dst]
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		cost := uint64(len(in.args)) * uint64(unsafe.Sizeof(model.Value{}))
		if in.op == opArray && len(in.args) != 0 {
			cost += uint64(unsafe.Sizeof(model.ArrayDimension{}))
		}
		if err := e.charge(cost); err != nil {
			return err
		}
		value := model.Value{Type: out.vector.Type.Storage, ArrayVal: make([]model.Value, len(in.args))}
		for i, r := range in.args {
			value.ArrayVal[i] = e.state.registers[r].value(lane)
		}
		if in.op == opArray && len(in.args) != 0 {
			value.Dimensions = []model.ArrayDimension{{Length: int32(len(in.args)), LowerBound: 1}}
		}
		out.put(lane, value)
	}
	return nil
}
