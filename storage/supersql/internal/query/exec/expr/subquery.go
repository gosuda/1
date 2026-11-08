package expr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var errArraySubqueryNull = &model.Error{Code: "22004", Message: "cannot accumulate null arrays"}

// SubqueryID is an opaque identity interpreted only by SubqueryRuntime.
type SubqueryID uint64

// Subquery is borrowed during Compile; only used, owned metadata is retained.
type Subquery struct {
	Scalar   ir.ScalarID
	ID       SubqueryID
	Captures []model.SlotID
}

// SubqueryRow is borrowed through the synchronous runtime callback. Present is
// false only for the typed-null row emitted by an empty exact-cardinality body.
type SubqueryRow struct {
	Values  []model.Value
	Present bool
}

// SubqueryRuntime borrows inputs and callbacks for synchronous calls. Scalar
// cardinality is proved before consume; row values remain alive through visit.
// Returning false from visit stops the stream. Every path closes the child.
type SubqueryRuntime interface {
	EvaluateScalar(context.Context, SubqueryID, []model.Value, func(SubqueryRow) error) error
	EvaluateRows(context.Context, SubqueryID, []model.Value, func(SubqueryRow) (bool, error)) error
}

type subqueryCapture struct {
	slot    model.SlotID
	value   register
	operand int
}

type subqueryComparison struct {
	left       *Program
	right      register
	result     register
	call       ScalarCall
	strict     bool
	ownsResult bool
}

type compiledSubquery struct {
	scalar        ir.ScalarID
	id            SubqueryID
	result        register
	kind          ir.SubqueryKind
	rowComparison ir.RowComparisonKind
	column        register
	columns       []register
	nestedArray   bool
	comparisons   []subqueryComparison
	captures      []subqueryCapture
}

func (c *compiler) validateSubqueries(ctx context.Context) error {
	if len(c.options.Subqueries) > c.p.limits.MaxInstructions {
		return ErrLimit
	}
	for i, descriptor := range c.options.Subqueries {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, ok := c.graph.Scalar(descriptor.Scalar)
		if !ok || n.Kind != ir.Subquery || n.SubqueryKind < ir.ScalarSubquery || n.SubqueryKind > ir.TupleSubquery || descriptor.ID == 0 {
			return invalid("invalid scalar subquery descriptor")
		}
		for _, prior := range c.options.Subqueries[:i] {
			if prior.Scalar == descriptor.Scalar || prior.ID == descriptor.ID {
				return invalid("duplicate scalar subquery descriptor")
			}
		}
		body, ok := c.graph.Rel(n.Relation)
		if !ok || len(descriptor.Captures) != int(c.graph.CaptureCount(body.Scope)) {
			return invalid("scalar subquery capture count mismatch")
		}
		for j, slot := range descriptor.Captures {
			want, ok := c.graph.Capture(body.Scope, uint32(j))
			if !ok || slot != want {
				return invalid("scalar subquery capture identity mismatch")
			}
		}
	}
	return nil
}

func (c *compiler) bindSlot(slot model.SlotID, in *instruction) error {
	in.op, in.index = opInput, -1
	for i, input := range c.p.inputs {
		if input.Slot == slot {
			in.index = i
			return nil
		}
	}
	for i := range c.p.captures {
		if c.p.captures[i].Slot == slot {
			in.op, in.index = opCapture, i
			c.p.captures[i].used = true
			return nil
		}
	}
	return invalid("scalar slot has no input or capture mapping")
}

func (c *compiler) subquery(ctx context.Context, id ir.ScalarID, dst, depth int) error {
	var descriptor *Subquery
	for i := range c.options.Subqueries {
		if c.options.Subqueries[i].Scalar == id {
			descriptor = &c.options.Subqueries[i]
			break
		}
	}
	if descriptor == nil {
		return ErrSubquery
	}
	if len(descriptor.Captures) > c.p.limits.MaxRegisters-len(c.p.registers) {
		return ErrLimit
	}
	n, _ := c.graph.Scalar(id)
	comparisonCount := int(n.ComparisonCount())
	columnCount := 0
	if n.SubqueryKind == ir.TupleSubquery {
		body, _ := c.graph.Rel(n.Relation)
		columnCount = int(body.OutputCount())
	}
	bytes := 3*uint64(unsafe.Sizeof(compiledSubquery{})) +
		uint64(len(descriptor.Captures))*(uint64(unsafe.Sizeof(subqueryCapture{}))+3*uint64(unsafe.Sizeof(register{}))) +
		uint64(comparisonCount)*uint64(unsafe.Sizeof(subqueryComparison{})) +
		uint64(columnCount)*uint64(unsafe.Sizeof(register{}))
	if err := c.p.charge(bytes); err != nil {
		return err
	}
	metadata := compiledSubquery{
		scalar: id, id: descriptor.ID, result: c.p.registers[dst], kind: n.SubqueryKind,
		rowComparison: n.RowComparison,
		comparisons:   make([]subqueryComparison, comparisonCount),
		captures:      make([]subqueryCapture, len(descriptor.Captures)),
		columns:       make([]register, columnCount),
	}
	in := instruction{op: opSubquery, dst: dst, index: len(c.p.subqueries), args: make([]int, len(descriptor.Captures))}
	if n.SubqueryKind == ir.AnySubquery || n.SubqueryKind == ir.AllSubquery || n.SubqueryKind == ir.RowSubquery {
		for i := range metadata.comparisons {
			comparison, err := c.compileSubqueryComparison(ctx, id, n, uint32(i), depth)
			if err != nil {
				return err
			}
			metadata.comparisons[i] = comparison
		}
	} else if n.SubqueryKind == ir.ArraySubquery {
		output, _ := c.graph.Output(n.Relation, 0)
		slot, _ := c.graph.Slot(output)
		layout, err := c.layout(ctx, slot.Layout, 0)
		if err != nil {
			return err
		}
		metadata.column = register{layout: layout, nullable: slot.Nullable}
		metadata.nestedArray = layout.Type.Storage == model.TypeArray && model.TypeLayoutsEqual(layout, metadata.result.layout)
	} else if n.SubqueryKind == ir.TupleSubquery {
		for i := range metadata.columns {
			output, _ := c.graph.Output(n.Relation, uint32(i))
			slot, _ := c.graph.Slot(output)
			layout, err := c.layout(ctx, slot.Layout, 0)
			if err != nil {
				return err
			}
			metadata.columns[i] = register{layout: layout, nullable: slot.Nullable}
		}
	}
	for i, slotID := range descriptor.Captures {
		slot, ok := c.graph.Slot(slotID)
		if !ok {
			return ErrInvalid
		}
		layout, err := c.layout(ctx, slot.Layout, 0)
		if err != nil {
			return err
		}
		r := register{layout: layout, nullable: slot.Nullable}
		operand := instruction{dst: len(c.p.registers)}
		if err := c.bindSlot(slotID, &operand); err != nil {
			return err
		}
		c.p.registers = append(c.p.registers, r)
		if err := c.p.emit(instruction{op: opInit, dst: operand.dst}); err != nil {
			return err
		}
		if err := c.p.emit(operand); err != nil {
			return err
		}
		in.args[i] = operand.dst
		metadata.captures[i] = subqueryCapture{slot: slotID, value: r, operand: operand.dst}
	}
	in.index = len(c.p.subqueries)
	c.p.subqueries = append(c.p.subqueries, metadata)
	return c.p.emit(in)
}

func (c *compiler) compileSubqueryComparison(ctx context.Context, id ir.ScalarID, n ir.Scalar, index uint32, depth int) (subqueryComparison, error) {
	left, _ := c.graph.Operand(id, index)
	signatureID, _ := c.graph.Comparison(id, index)
	signature, _ := c.graph.Signature(signatureID)
	options := c.options
	options.Limits.MaxDepth -= depth + 1
	options.Limits.MaxBytes = c.p.limits.MaxBytes - c.p.bytes
	options.Limits.MaxInstructions -= len(c.p.code) + c.p.templateInstructions
	if options.Limits.MaxDepth <= 0 || options.Limits.MaxBytes == 0 || options.Limits.MaxInstructions <= 0 {
		return subqueryComparison{}, ErrLimit
	}
	program, err := Compile(ctx, c.graph, []ir.ScalarID{left}, options)
	if err != nil {
		return subqueryComparison{}, err
	}
	if err := c.p.charge(uint64(unsafe.Sizeof(Program{})) + program.bytes); err != nil {
		return subqueryComparison{}, err
	}
	c.p.templateInstructions += len(program.code) + program.templateInstructions
	rightID, _ := c.graph.SignatureParameter(signatureID, 1)
	rightLayout, err := c.layout(ctx, rightID, 0)
	if err != nil {
		return subqueryComparison{}, err
	}
	resultLayout, err := c.layout(ctx, signature.Result, 0)
	if err != nil {
		return subqueryComparison{}, err
	}
	output, _ := c.graph.Output(n.Relation, index)
	slot, _ := c.graph.Slot(output)
	callNode := ir.Scalar{Kind: ir.Call, Layout: signature.Result, Signature: signatureID, Function: signature.Function, Operator: signature.Operator, Strict: signature.Strict}
	call := instruction{}
	if err := c.resolve(ctx, callNode, &call); err != nil {
		return subqueryComparison{}, err
	}
	return subqueryComparison{
		left: program, right: register{layout: rightLayout, nullable: slot.Nullable},
		result: register{layout: resultLayout, nullable: true},
		call: call.call, strict: signature.Strict, ownsResult: call.ownsResult,
	}, nil
}

func (p *Program) verifySubqueries() error {
	for _, descriptor := range p.subqueries {
		if descriptor.id == 0 || descriptor.scalar == 0 || descriptor.result.once || descriptor.kind < ir.ScalarSubquery || descriptor.kind > ir.TupleSubquery {
			return invalid("invalid scalar subquery metadata")
		}
		if err := descriptor.result.layout.Validate(); err != nil {
			return err
		}
		switch descriptor.kind {
		case ir.ScalarSubquery:
			if !descriptor.result.nullable || len(descriptor.comparisons) != 0 || descriptor.column.layout.Type.ID != 0 {
				return invalid("invalid scalar subquery result")
			}
		case ir.ExistsSubquery:
			if descriptor.result.nullable || descriptor.result.layout.Type.Storage != model.TypeBoolean || len(descriptor.comparisons) != 0 || descriptor.column.layout.Type.ID != 0 {
				return invalid("invalid EXISTS result")
			}
		case ir.ArraySubquery:
			if descriptor.result.nullable || descriptor.result.layout.Type.Storage != model.TypeArray || len(descriptor.result.layout.Children) != 1 || len(descriptor.comparisons) != 0 {
				return invalid("invalid ARRAY subquery result")
			}
			if err := descriptor.column.layout.Validate(); err != nil {
				return err
			}
			if descriptor.nestedArray {
				if descriptor.column.layout.Type.Storage != model.TypeArray || !model.TypeLayoutsEqual(descriptor.column.layout, descriptor.result.layout) {
					return invalid("invalid nested ARRAY subquery layout")
				}
			} else if !model.TypeLayoutsEqual(descriptor.column.layout, descriptor.result.layout.Children[0]) {
				return invalid("ARRAY subquery element layout mismatch")
			}
		case ir.TupleSubquery:
			if descriptor.result.nullable || descriptor.result.layout.Type.Storage != model.TypeComposite || len(descriptor.result.layout.Children) != len(descriptor.columns) || len(descriptor.comparisons) != 0 || descriptor.column.layout.Type.ID != 0 {
				return invalid("invalid tuple subquery result")
			}
			for i, column := range descriptor.columns {
				if err := column.layout.Validate(); err != nil {
					return err
				}
				if !model.TypeLayoutsEqual(column.layout, descriptor.result.layout.Children[i]) {
					return invalid("tuple subquery child layout mismatch")
				}
			}
		case ir.AnySubquery, ir.AllSubquery, ir.RowSubquery:
			validStrategy := descriptor.rowComparison >= ir.RowCompareLess && descriptor.rowComparison <= ir.RowCompareNotEqual
			if descriptor.result.layout.Type.Storage != model.TypeBoolean || !descriptor.result.nullable || len(descriptor.comparisons) == 0 ||
				len(descriptor.comparisons) > 1 && descriptor.rowComparison == 0 || descriptor.kind == ir.RowSubquery && descriptor.rowComparison == 0 ||
				descriptor.rowComparison != 0 && !validStrategy {
				return invalid("invalid row-comparison subquery")
			}
			for _, comparison := range descriptor.comparisons {
				if comparison.left == nil || !comparison.left.verified || comparison.left.OutputCount() != 1 || comparison.call == nil {
					return invalid("invalid subquery comparison")
				}
				if err := comparison.right.layout.Validate(); err != nil {
					return err
				}
				if err := comparison.result.layout.Validate(); err != nil {
					return err
				}
				booleanResult := descriptor.rowComparison == 0 || descriptor.rowComparison == ir.RowCompareEqual || descriptor.rowComparison == ir.RowCompareNotEqual
				if (booleanResult && comparison.result.layout.Type.Storage != model.TypeBoolean) || (!booleanResult && (comparison.result.layout.Type.ID != 23 || comparison.result.layout.Type.Storage != model.TypeInt64 || len(comparison.result.layout.Children) != 0)) {
					return invalid("subquery comparison result type")
				}
			}
		}
		for i, capture := range descriptor.captures {
			if capture.slot == 0 || capture.value.once {
				return invalid("invalid scalar subquery capture")
			}
			if err := capture.value.layout.Validate(); err != nil {
				return err
			}
			for _, prior := range descriptor.captures[:i] {
				if prior.slot == capture.slot {
					return invalid("duplicate scalar subquery capture")
				}
			}
		}
	}
	return nil
}

func (p *Program) verifySubquery(in instruction) error {
	if in.index < 0 || in.index >= len(p.subqueries) {
		return invalid("invalid scalar subquery index")
	}
	descriptor := p.subqueries[in.index]
	result := p.registers[in.dst]
	arguments := len(descriptor.captures)
	if len(in.args) != arguments || !model.TypeLayoutsEqual(result.layout, descriptor.result.layout) || result.nullable != descriptor.result.nullable {
		return invalid("scalar subquery result or capture mismatch")
	}
	for i, operand := range in.args[:len(descriptor.captures)] {
		actual, expected := p.registers[operand], descriptor.captures[i].value
		if operand != descriptor.captures[i].operand || !model.TypeLayoutsEqual(actual.layout, expected.layout) || actual.nullable != expected.nullable {
			return invalid("scalar subquery capture layout mismatch")
		}
	}
	return nil
}

func evalSubquery(e *evaluation, in instruction) error {
	if len(e.lanes()) == 0 {
		return nil
	}
	runtime := e.state.options.Subqueries
	if runtime == nil {
		return invalid("scalar subquery runtime missing")
	}
	descriptor := e.state.program.subqueries[in.index]
	args := e.args[:len(in.args)]
	for _, lane := range e.lanes() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		for i, r := range in.args {
			args[i] = e.state.registers[r].value(lane)
		}
		captures := args[:len(descriptor.captures)]
		var value model.Value
		var err error
		switch descriptor.kind {
		case ir.ScalarSubquery:
			value, err = e.invokeScalarSubquery(runtime, descriptor, captures)
		case ir.RowSubquery:
			value, err = e.invokeRowSubquery(runtime, descriptor, captures, lane)
		case ir.ArraySubquery:
			value, err = e.invokeArraySubquery(runtime, descriptor, captures)
		case ir.TupleSubquery:
			value, err = e.invokeTupleSubquery(runtime, descriptor, captures)
		default:
			value, err = e.invokeQuantified(runtime, descriptor, captures, lane)
		}
		if err != nil {
			return err
		}
		e.state.registers[in.dst].put(lane, value)
	}
	return e.ctx.Err()
}

func (e *evaluation) invokeScalarSubquery(runtime SubqueryRuntime, descriptor compiledSubquery, captures []model.Value) (model.Value, error) {
	consumed := false
	var consumeErr error
	var result model.Value
	err := runtime.EvaluateScalar(e.ctx, descriptor.id, captures, func(row SubqueryRow) error {
		if consumed {
			consumeErr = errors.Join(consumeErr, invalid("scalar subquery consumed more than once"))
			return consumeErr
		}
		consumed = true
		if len(row.Values) != 1 || !row.Present && !row.Values[0].IsNull {
			consumeErr = invalid("scalar subquery row contract")
			return consumeErr
		}
		result, consumeErr = e.copySubqueryValue(row.Values[0], descriptor.result)
		return consumeErr
	})
	if err != nil || consumeErr != nil || e.ctx.Err() != nil {
		return model.Value{}, errors.Join(err, consumeErr, e.ctx.Err())
	}
	if !consumed {
		return model.Value{}, invalid("scalar subquery did not consume a result")
	}
	return result, nil
}

func (e *evaluation) invokeTupleSubquery(runtime SubqueryRuntime, descriptor compiledSubquery, captures []model.Value) (model.Value, error) {
	consumed := false
	var consumeErr error
	result := model.Value{Type: model.TypeComposite}
	err := runtime.EvaluateScalar(e.ctx, descriptor.id, captures, func(row SubqueryRow) error {
		if consumed {
			consumeErr = errors.Join(consumeErr, invalid("tuple subquery consumed more than once"))
			return consumeErr
		}
		consumed = true
		if len(row.Values) != len(descriptor.columns) {
			consumeErr = invalid("tuple subquery row width")
			return consumeErr
		}
		if err := e.charge(uint64(len(row.Values)) * uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			consumeErr = err
			return consumeErr
		}
		result.ArrayVal = make([]model.Value, len(row.Values))
		for i, value := range row.Values {
			if !row.Present && !value.IsNull {
				consumeErr = invalid("tuple subquery absent row is not typed NULL")
				return consumeErr
			}
			copied, err := e.copySubqueryValue(value, descriptor.columns[i])
			if err != nil {
				consumeErr = err
				return consumeErr
			}
			result.ArrayVal[i] = copied
		}
		return nil
	})
	if err != nil || consumeErr != nil || e.ctx.Err() != nil {
		return model.Value{}, errors.Join(err, consumeErr, e.ctx.Err())
	}
	if !consumed {
		return model.Value{}, invalid("tuple subquery did not consume a result")
	}
	return result, nil
}

func (e *evaluation) copySubqueryValue(value model.Value, destination register) (model.Value, error) {
	if err := validateValue(e.ctx, value, destination.layout, 0); err != nil {
		return model.Value{}, err
	}
	if value.IsNull && !destination.nullable {
		return model.Value{}, invalid("non-nullable subquery column returned NULL")
	}
	if fixed(value.Type) || value.IsNull {
		return value, nil
	}
	bytes, err := valueBytes(e.ctx, value, e.remaining())
	if err != nil {
		return model.Value{}, err
	}
	if err := e.charge(bytes); err != nil {
		return model.Value{}, err
	}
	value, _, err = model.CopyValue(e.ctx, value, bytes)
	return value, err
}

func (e *evaluation) invokeRowSubquery(runtime SubqueryRuntime, descriptor compiledSubquery, captures []model.Value, lane int) (value model.Value, err error) {
	var states []*State
	defer func() {
		err = errors.Join(err, closeComparisonStates(states))
		if err != nil {
			value = model.Value{}
		}
	}()
	consumed := false
	var consumeErr error
	err = runtime.EvaluateScalar(e.ctx, descriptor.id, captures, func(row SubqueryRow) error {
		if consumed {
			consumeErr = errors.Join(consumeErr, invalid("row subquery consumed more than once"))
			return consumeErr
		}
		consumed = true
		if !row.Present {
			if consumeErr = e.validateEmptySubqueryRow(descriptor, row.Values); consumeErr != nil {
				return consumeErr
			}
			value = model.NullVal(model.TypeBoolean)
			return nil
		}
		states, consumeErr = e.comparisonStates(descriptor)
		if consumeErr != nil {
			return consumeErr
		}
		value, consumeErr = e.compareSubqueryRow(descriptor, states, row.Values, lane)
		return consumeErr
	})
	if err != nil || consumeErr != nil || e.ctx.Err() != nil {
		return model.Value{}, errors.Join(err, consumeErr, e.ctx.Err())
	}
	if !consumed {
		return model.Value{}, invalid("row subquery did not consume a result")
	}
	return value, nil
}

func (e *evaluation) validateEmptySubqueryRow(descriptor compiledSubquery, row []model.Value) error {
	if len(row) != len(descriptor.comparisons) {
		return invalid("empty subquery row width")
	}
	for i, value := range row {
		if !value.IsNull {
			return invalid("empty subquery row is not typed NULL")
		}
		if err := validateValue(e.ctx, value, descriptor.comparisons[i].right.layout, 0); err != nil {
			return err
		}
	}
	return nil
}

func (e *evaluation) invokeQuantified(runtime SubqueryRuntime, descriptor compiledSubquery, captures []model.Value, lane int) (value model.Value, err error) {
	result := model.BoolVal(descriptor.kind == ir.AllSubquery)
	var states []*State
	defer func() {
		err = errors.Join(err, closeComparisonStates(states))
		if err != nil {
			value = model.Value{}
		}
	}()
	stopped := false
	var visitErr error
	err = runtime.EvaluateRows(e.ctx, descriptor.id, captures, func(row SubqueryRow) (bool, error) {
		if stopped || visitErr != nil {
			visitErr = errors.Join(visitErr, invalid("subquery continued after stop"))
			return false, visitErr
		}
		if !row.Present {
			visitErr, stopped = invalid("streamed subquery row is absent"), true
			return false, visitErr
		}
		if descriptor.kind == ir.ExistsSubquery {
			result, stopped = model.BoolVal(true), true
			return false, nil
		}
		if states == nil {
			states, visitErr = e.comparisonStates(descriptor)
			if visitErr != nil {
				stopped = true
				return false, visitErr
			}
		}
		comparison, compareErr := e.compareSubqueryRow(descriptor, states, row.Values, lane)
		if compareErr != nil {
			visitErr, stopped = compareErr, true
			return false, visitErr
		}
		if comparison.IsNull {
			result = comparison
		} else if (comparison.IntVal != 0) == (descriptor.kind == ir.AnySubquery) {
			result, stopped = comparison, true
		}
		return !stopped, nil
	})
	if err != nil || visitErr != nil || e.ctx.Err() != nil {
		return model.Value{}, errors.Join(err, visitErr, e.ctx.Err())
	}
	return result, nil
}

func (e *evaluation) comparisonStates(descriptor compiledSubquery) ([]*State, error) {
	bytes := uint64(len(descriptor.comparisons)) * uint64(unsafe.Sizeof((*State)(nil)))
	if err := e.charge(bytes); err != nil {
		return nil, err
	}
	return make([]*State, len(descriptor.comparisons)), nil
}

func closeComparisonStates(states []*State) error {
	var err error
	for _, state := range states {
		err = errors.Join(err, state.Close())
	}
	return err
}

func (e *evaluation) compareSubqueryRow(descriptor compiledSubquery, states []*State, row []model.Value, lane int) (model.Value, error) {
	if len(row) != len(descriptor.comparisons) {
		return model.Value{}, invalid("subquery row width")
	}
	for i, value := range row {
		right := descriptor.comparisons[i].right
		if err := validateValue(e.ctx, value, right.layout, 0); err != nil {
			return model.Value{}, err
		}
		if value.IsNull && !right.nullable {
			return model.Value{}, invalid("non-nullable subquery column returned NULL")
		}
	}
	haveNull := false
	for i, comparison := range descriptor.comparisons {
		if err := e.ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if states[i] == nil {
			state, err := NewState(comparison.left, e.state.options)
			if err != nil {
				return model.Value{}, err
			}
			states[i] = state
		}
		selection := [1]uint32{uint32(e.input.Lane(lane))}
		input := columnar.Batch{Columns: e.input.Columns, Length: e.input.Length, Selection: selection[:], MaxRows: 1}
		var left [1]model.Value
		if err := states[i].EvalBorrowedRow(e.ctx, &input, e.parameters, left[:], e.mode); err != nil {
			return model.Value{}, err
		}
		result := model.NullVal(comparison.result.layout.Type.Storage)
		if !comparison.strict || !left[0].IsNull && !row[i].IsNull {
			args := [2]model.Value{left[0], row[i]}
			var err error
			result, err = e.invokeScalar(comparison.call, args[:], comparison.result, comparison.ownsResult)
			if err != nil {
				return model.Value{}, err
			}
		}
		switch descriptor.rowComparison {
		case 0:
			return result, nil
		case ir.RowCompareEqual:
			if result.IsNull {
				haveNull = true
			} else if result.IntVal == 0 {
				return model.BoolVal(false), nil
			}
		case ir.RowCompareNotEqual:
			if result.IsNull {
				haveNull = true
			} else if result.IntVal != 0 {
				return model.BoolVal(true), nil
			}
		default:
			if result.IsNull {
				return model.NullVal(model.TypeBoolean), nil
			}
			if result.IntVal != 0 {
				return model.BoolVal(applyRowOrdering(descriptor.rowComparison, result.IntVal)), nil
			}
		}
	}
	if haveNull {
		return model.NullVal(model.TypeBoolean), nil
	}
	switch descriptor.rowComparison {
	case ir.RowCompareEqual, ir.RowCompareLessEqual, ir.RowCompareGreaterEqual:
		return model.BoolVal(true), nil
	default:
		return model.BoolVal(false), nil
	}
}

func applyRowOrdering(kind ir.RowComparisonKind, comparison int64) bool {
	switch kind {
	case ir.RowCompareLess, ir.RowCompareLessEqual:
		return comparison < 0
	case ir.RowCompareGreaterEqual, ir.RowCompareGreater:
		return comparison > 0
	default:
		return false
	}
}

const subqueryArrayChunkValues = 32

type subqueryArrayChunk struct {
	next   *subqueryArrayChunk
	count  int
	values [subqueryArrayChunkValues]model.Value
}

type subqueryArrayAccumulator struct {
	first, last *subqueryArrayChunk
	count       uint64
	rows        uint64
	inner       []model.ArrayDimension
}

func (e *evaluation) appendSubqueryArrayValue(accumulator *subqueryArrayAccumulator, value model.Value) error {
	if accumulator.last == nil || accumulator.last.count == len(accumulator.last.values) {
		if err := e.charge(uint64(unsafe.Sizeof(subqueryArrayChunk{}))); err != nil {
			return err
		}
		chunk := &subqueryArrayChunk{}
		if accumulator.last == nil {
			accumulator.first = chunk
		} else {
			accumulator.last.next = chunk
		}
		accumulator.last = chunk
	}
	if !fixed(value.Type) && !value.IsNull {
		bytes, err := valueBytes(e.ctx, value, e.remaining())
		if err != nil {
			return err
		}
		if err := e.charge(bytes); err != nil {
			return err
		}
		value, _, err = model.CopyValue(e.ctx, value, bytes)
		if err != nil {
			return err
		}
	}
	accumulator.last.values[accumulator.last.count] = value
	accumulator.last.count++
	accumulator.count++
	return nil
}

func (e *evaluation) invokeArraySubquery(runtime SubqueryRuntime, descriptor compiledSubquery, captures []model.Value) (model.Value, error) {
	var accumulator subqueryArrayAccumulator
	stopped := false
	var visitErr error
	err := runtime.EvaluateRows(e.ctx, descriptor.id, captures, func(row SubqueryRow) (bool, error) {
		if stopped || visitErr != nil {
			visitErr = errors.Join(visitErr, invalid("ARRAY subquery continued after stop"))
			return false, visitErr
		}
		if !row.Present || len(row.Values) != 1 {
			visitErr, stopped = invalid("ARRAY subquery row width"), true
			return false, visitErr
		}
		child := row.Values[0]
		if visitErr = validateValue(e.ctx, child, descriptor.column.layout, 0); visitErr != nil {
			stopped = true
			return false, visitErr
		}
		if child.IsNull && !descriptor.column.nullable {
			visitErr, stopped = invalid("non-nullable ARRAY subquery column returned NULL"), true
			return false, visitErr
		}
		accumulator.rows++
		if descriptor.nestedArray {
			if child.IsNull {
				visitErr, stopped = errArraySubqueryNull, true
				return false, visitErr
			}
			if len(child.Dimensions) == 0 || len(child.ArrayVal) == 0 {
				visitErr, stopped = errArrayStackShape, true
				return false, visitErr
			}
			if len(child.Dimensions) >= 6 {
				visitErr, stopped = errArrayStackRank, true
				return false, visitErr
			}
			if accumulator.inner == nil {
				bytes := uint64(len(child.Dimensions)) * uint64(unsafe.Sizeof(model.ArrayDimension{}))
				if visitErr = e.charge(bytes); visitErr != nil {
					stopped = true
					return false, visitErr
				}
				accumulator.inner = slices.Clone(child.Dimensions)
			} else if !slices.Equal(accumulator.inner, child.Dimensions) {
				visitErr, stopped = errArrayStackShape, true
				return false, visitErr
			}
			for _, element := range child.ArrayVal {
				if visitErr = e.appendSubqueryArrayValue(&accumulator, element); visitErr != nil {
					stopped = true
					return false, visitErr
				}
			}
		} else if visitErr = e.appendSubqueryArrayValue(&accumulator, child); visitErr != nil {
			stopped = true
			return false, visitErr
		}
		return true, nil
	})
	if err != nil || visitErr != nil || e.ctx.Err() != nil {
		return model.Value{}, errors.Join(err, visitErr, e.ctx.Err())
	}
	if accumulator.rows == 0 {
		return model.Value{Type: model.TypeArray}, nil
	}
	if accumulator.rows >= math.MaxInt32 {
		return model.Value{}, errArrayStackSize
	}
	dimensions := 1
	if descriptor.nestedArray {
		dimensions += len(accumulator.inner)
		for _, dimension := range accumulator.inner {
			if int64(dimension.LowerBound)+int64(dimension.Length) > math.MaxInt32 {
				return model.Value{}, &model.Error{Code: "54000", Message: fmt.Sprintf("array lower bound is too large: %d", dimension.LowerBound)}
			}
		}
	}
	dimensionBytes := uint64(dimensions) * uint64(unsafe.Sizeof(model.ArrayDimension{}))
	width := uint64(unsafe.Sizeof(model.Value{}))
	if dimensionBytes >= math.MaxInt32 || accumulator.count > (math.MaxInt32-dimensionBytes)/width {
		return model.Value{}, errArrayStackSize
	}
	bytes := dimensionBytes + accumulator.count*width
	if err := e.charge(bytes); err != nil {
		return model.Value{}, err
	}
	result := model.Value{
		Type:       model.TypeArray,
		Dimensions: make([]model.ArrayDimension, dimensions),
		ArrayVal:   make([]model.Value, int(accumulator.count)),
	}
	result.Dimensions[0] = model.ArrayDimension{Length: int32(accumulator.rows), LowerBound: 1}
	copy(result.Dimensions[1:], accumulator.inner)
	offset := 0
	for chunk := accumulator.first; chunk != nil; chunk = chunk.next {
		copy(result.ArrayVal[offset:], chunk.values[:chunk.count])
		offset += chunk.count
	}
	return result, nil
}

