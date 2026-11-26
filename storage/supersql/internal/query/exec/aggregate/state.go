package aggregate

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type transition struct {
	value      model.Value
	count      int64
	floatCount float64
	squares    float64
	key        []byte
	moments    *builtin.NumericMoments
	paired     *pairedMoments
	arrayBytes uint64
	interval   builtin.IntervalAggregate
	json       *jsonAggregateState
	ranges     *rangeAggregateState
}

type group struct {
	key        string
	values     []model.Value
	states     []transition
	orderedSet *rowstore.Spool
}

func numeric(text string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: text} }
func overflow() error                 { return &model.Error{Code: "22003", Message: "aggregate value out of range"} }

func (c *cursor) newGroup(ctx context.Context, key []byte, row []model.Value) (*group, error) {
	g := &group{key: string(key), values: make([]model.Value, len(c.options.GroupColumns)), states: make([]transition, len(c.calls))}
	remaining := c.options.MaxRecordBytes * 8
	for i, column := range c.options.GroupColumns {
		value, used, err := model.CopyValue(ctx, row[column], remaining)
		if err != nil {
			return nil, err
		}
		g.values[i], remaining = value, remaining-used
	}
	for i, call := range c.calls {
		switch {
		case call.op == userDefinedAggregate && call.user.hasInitial:
			// A declared INITCOND starts every group's state, so the first row is
			// combined with it rather than replacing it.
			g.states[i] = call.user.initialTransition()
		default:
			g.states[i].value = model.NullVal(call.output.Storage)
		}
		if call.op.pairedMoments() {
			g.states[i].paired = new(pairedMoments)
		}
	}
	return g, nil
}

func qualifies(call compiledCall, row []model.Value) bool {
	return call.filter < 0 || (!row[call.filter].IsNull && row[call.filter].IntVal == 1)
}

func (c *cursor) accumulate(ctx context.Context, g *group, row []model.Value) error {
	first, end := 0, len(c.calls)
	if c.orderTag >= 0 {
		first = int(row[c.orderTag].IntVal)
		end = first + 1
	}
	for i := first; i < end; i++ {
		call := c.calls[i]
		if call.op.orderedSet() {
			if err := c.advanceOrderedSet(ctx, g, i, row); err != nil {
				return err
			}
			continue
		}
		if !qualifies(call, row) {
			continue
		}
		arguments := c.arguments[:call.arity]
		for j := range call.arity {
			arguments[j] = row[call.arguments[j]]
		}
		if !call.acceptsArguments(arguments) {
			continue
		}
		state := &g.states[i]
		if err := c.advance(ctx, call, state, arguments); err != nil {
			return err
		}
	}
	return nil
}

func (c *cursor) advance(ctx context.Context, call compiledCall, state *transition, arguments []model.Value) error {
	if state.count == math.MaxInt64 && !(call.combining && call.op == count) {
		return overflow()
	}
	if call.op == jsonAggregate {
		return c.advanceJSONAggregate(ctx, call, state, arguments)
	}
	for i, value := range arguments {
		if value.IsNull {
			continue
		}
		switch call.inputs[i].ID {
		case 16:
			if value.IntVal != 0 && value.IntVal != 1 {
				return columnar.ErrValueLayout
			}
		case 21:
			if value.IntVal < math.MinInt16 || value.IntVal > math.MaxInt16 {
				return overflow()
			}
		case 23:
			if value.IntVal < math.MinInt32 || value.IntVal > math.MaxInt32 {
				return overflow()
			}
		case 700:
			if !math.IsNaN(value.FloatVal) && float64(float32(value.FloatVal)) != value.FloatVal {
				return columnar.ErrValueLayout
			}
		}
	}
	if call.op == userDefinedAggregate {
		return c.advanceUserAggregate(ctx, call.user, state, arguments)
	}
	if call.op.arrayAggregation() {
		return c.advanceArrayAggregate(ctx, call, state, arguments[0])
	}
	if call.op.rangeAggregation() {
		return c.advanceRangeAggregate(ctx, call, state, arguments[0])
	}
	if call.op == stringAggregate {
		return c.advanceStringAggregate(ctx, state, arguments)
	}
	if call.op.pairedMoments() {
		return advancePairedStatistics(state, arguments[0].FloatVal, arguments[1].FloatVal)
	}
	var value model.Value
	if call.arity != 0 {
		value = arguments[0]
	}
	if call.combining && call.op == count {
		if value.IntVal < 0 || state.count > math.MaxInt64-value.IntVal {
			return overflow()
		}
		state.count += value.IntVal
		state.value = model.Int64Val(state.count)
		return nil
	}
	if call.output.ID == 1186 && (call.op == sum || call.op == average) {
		return state.interval.Advance(value.IntervalVal)
	}
	if call.scalar != nil {
		return c.advanceScalarAggregate(ctx, call, state, value)
	}
	if call.op == anyValue {
		if !state.value.IsNull {
			return nil
		}
		owned, _, err := model.CopyValue(ctx, value, c.options.MaxRecordBytes)
		if err != nil {
			return err
		}
		state.value = owned
		return nil
	}
	if call.op.statistical() && call.output.ID == 1700 {
		return c.advanceNumericStatistics(ctx, state, value)
	}
	floatAverage := call.op == average && call.output.ID == 701
	if floatAverage || call.op.statistical() {
		return advanceFloatStatistics(state, value.FloatVal)
	}
	state.count++
	if call.op == count || call.op == regrCount {
		state.value = model.Int64Val(state.count)
		return nil
	}
	if call.op == bitwiseAnd || call.op == bitwiseOr || call.op == bitwiseXor {
		if state.value.IsNull {
			state.value = value
			return nil
		}
		switch call.op {
		case bitwiseAnd:
			state.value.IntVal &= value.IntVal
		case bitwiseOr:
			state.value.IntVal |= value.IntVal
		case bitwiseXor:
			state.value.IntVal ^= value.IntVal
		}
		return nil
	}
	if call.op == minimum || call.op == maximum {
		if state.value.IsNull {
			owned, _, err := model.CopyValue(ctx, value, c.options.MaxRecordBytes)
			if err != nil {
				return err
			}
			state.value = owned
			return nil
		}
		if state.key == nil {
			key, err := c.options.Encoder.EncodePrefix(ctx, call.key, []model.Value{state.value}, c.options.MaxRecordBytes)
			if err != nil {
				return err
			}
			state.key = key
		}
		key, err := c.options.Encoder.EncodePrefix(ctx, call.key, []model.Value{value}, c.options.MaxRecordBytes)
		if err != nil {
			return err
		}
		comparison := bytes.Compare(key, state.key)
		newMinimum := call.op == minimum && comparison < 0
		newMaximum := call.op == maximum && comparison > 0
		if newMinimum || newMaximum {
			owned, _, err := model.CopyValue(ctx, value, c.options.MaxRecordBytes)
			if err != nil {
				return err
			}
			state.value, state.key = owned, key
		}
		return nil
	}
	if call.output.ID == 1700 {
		if call.inputs[0].ID != 1700 {
			value = numeric(strconv.FormatInt(value.IntVal, 10))
		}
		left := state.value
		if left.IsNull {
			left = numeric("0")
		}
		result, err := c.numericAdd.Call(ctx, []model.Value{left, value}, builtin.CallBudget{OutputBytes: int64(c.options.MaxRecordBytes), MemoryBytes: builtin.NumericMomentsWorkBytes})
		if err != nil {
			return err
		}
		state.value = result
		return nil
	}
	if call.output.ID == 20 {
		left, right := state.value.IntVal, value.IntVal
		positiveOverflow := right > 0 && left > math.MaxInt64-right
		negativeOverflow := right < 0 && left < math.MinInt64-right
		if positiveOverflow || negativeOverflow {
			return overflow()
		}
		state.value = model.Int64Val(left + right)
		return nil
	}
	if call.op == sum && state.value.IsNull {
		state.value = value
		return nil
	}
	left, right := state.value.FloatVal, value.FloatVal
	if state.value.IsNull {
		left = 0
	}
	result := left + right
	if call.output.ID == 700 {
		result = float64(float32(result))
	}
	if math.IsInf(result, 0) && !math.IsInf(left, 0) && !math.IsInf(right, 0) {
		return overflow()
	}
	state.value = model.Float64Val(result)
	return nil
}

func (c *cursor) finish(ctx context.Context, g *group) (values []model.Value, err error) {
	copy(c.output, g.values)
	var ordered *orderedRows
	if c.hasOrderedSet {
		ordered, err = openOrderedRows(ctx, g, c.orderTag)
		if err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, ordered.Close()) }()
	}
	for i, call := range c.calls {
		state := &g.states[i]
		if call.op.orderedSet() {
			var direct []model.Value
			if call.directCount != 0 {
				direct = c.directValues[:call.directCount]
				clear(direct)
				if call.directEvaluator == nil {
					return nil, runtime.ErrInvalidConfig
				}
				if err := call.directEvaluator.Evaluate(ctx, g.values, c.options.DirectGroupColumns, direct); err != nil {
					clear(direct)
					return nil, err
				}
			}
			value, finalErr := c.finishOrderedSet(ctx, ordered, i, call, state, direct)
			clear(direct)
			if finalErr != nil {
				return nil, finalErr
			}
			c.output[len(g.values)+i] = value
			continue
		}
		value := state.value
		if call.op == userDefinedAggregate {
			var direct []model.Value
			if call.directCount != 0 {
				// Direct arguments are evaluated per group from the grouping row,
				// exactly as the ordered-set path does, because a declaration's
				// final function receives them.
				direct = c.directValues[:call.directCount]
				clear(direct)
				if call.directEvaluator == nil {
					return nil, runtime.ErrInvalidConfig
				}
				if directErr := call.directEvaluator.Evaluate(ctx, g.values, c.options.DirectGroupColumns, direct); directErr != nil {
					clear(direct)
					return nil, directErr
				}
			}
			value, err = c.finishUserAggregate(ctx, call.user, state, direct)
			clear(direct)
			if err != nil {
				return nil, err
			}
		}
		if call.op.rangeAggregation() {
			var err error
			value, err = c.finishRangeAggregate(ctx, call, state)
			if err != nil {
				return nil, err
			}
		}
		if call.op == jsonAggregate {
			var err error
			value, err = c.finishJSONAggregate(ctx, call, state)
			if err != nil {
				return nil, err
			}
		}
		if call.op.arrayAggregation() {
			if err := arrayAggregateResultBounds(value.Dimensions); err != nil {
				return nil, err
			}
		}
		if call.op == stringAggregate {
			var err error
			value, err = c.finishStringAggregate(ctx, call, state)
			if err != nil {
				return nil, err
			}
		}
		if call.op == count || call.op == regrCount {
			value = model.Int64Val(state.count)
		}
		if call.output.ID == 1186 && (call.op == sum || call.op == average) {
			var err error
			if call.op == sum {
				value, err = state.interval.Sum()
			} else {
				value, err = state.interval.Average()
			}
			if err != nil {
				return nil, err
			}
		}
		if call.op == average && state.count != 0 {
			if call.output.ID == 1700 {
				var err error
				value, err = c.numericDivide.Call(ctx, []model.Value{value, numeric(strconv.FormatInt(state.count, 10))}, builtin.CallBudget{OutputBytes: int64(c.options.MaxRecordBytes), MemoryBytes: builtin.NumericMomentsWorkBytes})
				if err != nil {
					return nil, err
				}
			} else {
				value = model.Float64Val(value.FloatVal / state.floatCount)
			}
		}
		if call.op.pairedMoments() {
			value = finishPairedStatistics(call.op, state.paired)
		}
		if call.op.statistical() {
			var err error
			value, err = c.finishStatistics(ctx, call, state)
			if err != nil {
				return nil, err
			}
		}
		if call.op != anyValue && !value.IsNull && call.output.ID == 1700 && call.output.Modifier >= 4 {
			var err error
			value, err = c.numericTypmod.Call(ctx, []model.Value{value, model.Int64Val(int64(call.output.Modifier))}, builtin.CallBudget{OutputBytes: int64(c.options.MaxRecordBytes), MemoryBytes: builtin.NumericMomentsWorkBytes})
			if err != nil {
				return nil, err
			}
		}
		c.output[len(g.values)+i] = value
	}
	if c.partitioned != nil {
		if err := c.partitioned.finishGroup(ctx, c.options.Encoder, g, c.options.MaxRecordBytes); err != nil {
			return nil, err
		}
	}
	return c.output, nil
}

func (call compiledCall) acceptsArguments(arguments []model.Value) bool {
	if call.op.arrayAggregation() || call.op == jsonAggregate {
		return true
	}
	if call.op == userDefinedAggregate {
		// A declared transition function may be non-strict, so NULL inputs are
		// real arguments; the compiled call applies the declaration's strictness.
		return true
	}
	if call.op == stringAggregate {
		return !arguments[0].IsNull
	}
	for _, value := range arguments {
		if value.IsNull {
			return false
		}
	}
	return true
}
