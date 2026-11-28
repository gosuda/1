package aggregate

import (
	"bytes"
	"context"
	"io"
	"math"
	"sort"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type percentilePoint struct {
	row    int64
	output int
	upper  bool
}

type orderedRows struct {
	reader     *rowstore.Reader
	pending    rowstore.Position
	hasPending bool
	tag        int
}

func orderedSetOperation(function model.FunctionID) (operation, bool) {
	switch function {
	case 3972, 3978:
		return percentileDiscrete, true
	case 3974, 3976, 3980, 3982:
		return percentileContinuous, true
	case 3984:
		return modeAggregate, true
	case 3986:
		return hypotheticalRank, true
	case 3988:
		return hypotheticalPercentRank, true
	case 3990:
		return hypotheticalCumeDist, true
	case 3992:
		return hypotheticalDenseRank, true
	}
	return 0, false
}

func (op operation) orderedSet() bool {
	return op >= percentileDiscrete && op <= hypotheticalDenseRank
}

func (op operation) hypotheticalSet() bool {
	return op >= hypotheticalRank && op <= hypotheticalDenseRank
}

func compileOrderedSetCall(ctx context.Context, call Call, outputLayout model.TypeLayout, layouts []model.TypeLayout, options *Options) (compiledCall, bool, error) {
	op, ordered := orderedSetOperation(call.Function)
	if !ordered {
		return compiledCall{}, false, nil
	}
	result := newCompiledCall(len(call.Arguments), outputLayout.Type)
	result.op = op
	result.directCount = len(call.DirectLayouts)
	result.arrayPercentile = call.Function == 3978 || call.Function == 3980 || call.Function == 3982
	result.directEvaluator = call.DirectEvaluator
	unsupported := func(reason string) (compiledCall, bool, error) {
		return result, true, &UnsupportedError{Function: call.Function, Reason: reason}
	}
	if call.Distinct {
		return unsupported("DISTINCT ordered-set aggregate")
	}
	if len(call.Arguments) == 0 || len(call.Arguments) != len(call.OrderBy) {
		return result, true, columnar.ErrValueLayout
	}
	if op.hypotheticalSet() {
		if len(call.DirectLayouts) != len(call.Arguments) {
			return result, true, columnar.ErrValueLayout
		}
	} else if op == modeAggregate {
		if len(call.DirectLayouts) != 0 || len(call.Arguments) != 1 {
			return result, true, columnar.ErrValueLayout
		}
	} else if len(call.DirectLayouts) != 1 || len(call.Arguments) != 1 {
		return result, true, columnar.ErrValueLayout
	}
	if err := outputLayout.Validate(); err != nil {
		return result, true, err
	}
	for _, layout := range call.DirectLayouts {
		if err := layout.Validate(); err != nil {
			return result, true, err
		}
	}
	for i, column := range call.Arguments {
		if column < 0 || column >= len(layouts) || call.OrderBy[i].Column != column {
			return result, true, columnar.ErrValueLayout
		}
		if op.hypotheticalSet() && !model.TypeLayoutsEqual(call.DirectLayouts[i], layouts[column]) {
			return result, true, columnar.ErrValueLayout
		}
	}
	argument := layouts[call.Arguments[0]]
	if result.arrayPercentile && len(outputLayout.Children) == 1 {
		result.element = outputLayout.Children[0].Type.Storage
	}
	result.inputs[0] = argument.Type
	float8 := func(layout model.TypeLayout) bool {
		return layout.Type.ID == 701 && layout.Type.Storage == model.TypeFloat64 && len(layout.Children) == 0
	}
	float8Array := func(layout model.TypeLayout) bool {
		return layout.Type.ID == 1022 && layout.Type.Storage == model.TypeArray && len(layout.Children) == 1 && float8(layout.Children[0])
	}
	interval := func(layout model.TypeLayout) bool {
		return layout.Type.ID == 1186 && layout.Type.Storage == model.TypeInterval && len(layout.Children) == 0
	}
	sameElement := func(left, right model.TypeLayout) bool {
		left.Type.Modifier, right.Type.Modifier = -1, -1
		return model.TypeLayoutsEqual(left, right)
	}
	switch call.Function {
	case 3972, 3984:
		if call.Function == 3972 && !float8(call.DirectLayouts[0]) || !sameElement(argument, outputLayout) {
			return result, true, columnar.ErrValueLayout
		}
	case 3974:
		if !float8(call.DirectLayouts[0]) || !float8(argument) || !float8(outputLayout) {
			return result, true, columnar.ErrValueLayout
		}
	case 3976:
		if !float8(call.DirectLayouts[0]) || !interval(argument) || !interval(outputLayout) {
			return result, true, columnar.ErrValueLayout
		}
	case 3978:
		if !float8Array(call.DirectLayouts[0]) || outputLayout.Type.Storage != model.TypeArray || len(outputLayout.Children) != 1 || !sameElement(argument, outputLayout.Children[0]) {
			return result, true, columnar.ErrValueLayout
		}
	case 3980:
		if !float8Array(call.DirectLayouts[0]) || !float8(argument) || !float8Array(outputLayout) {
			return result, true, columnar.ErrValueLayout
		}
	case 3982:
		if !float8Array(call.DirectLayouts[0]) || !interval(argument) || outputLayout.Type.ID != 1187 || outputLayout.Type.Storage != model.TypeArray || len(outputLayout.Children) != 1 || !interval(outputLayout.Children[0]) {
			return result, true, columnar.ErrValueLayout
		}
	case 3986, 3992:
		if outputLayout.Type.ID != 20 || outputLayout.Type.Storage != model.TypeInt64 || len(outputLayout.Children) != 0 {
			return result, true, columnar.ErrValueLayout
		}
	case 3988, 3990:
		if !float8(outputLayout) {
			return result, true, columnar.ErrValueLayout
		}
	}
	if call.FilterColumn != nil {
		column := *call.FilterColumn
		if column < 0 || column >= len(layouts) || layouts[column].Type.ID != 16 || layouts[column].Type.Storage != model.TypeBoolean {
			return result, true, columnar.ErrValueLayout
		}
		result.filter = column
	}
	result.order = append([]OrderKey(nil), call.OrderBy...)
	result.orderKey = make([]storage.KeyColumn, len(call.OrderBy))
	for i, key := range call.OrderBy {
		layout := layouts[key.Column]
		result.orderKey[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: layout.Type, Children: layout.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	if _, err := options.Encoder.EncodePrefix(ctx, result.orderKey, nil, options.MaxRecordBytes); err != nil {
		return result, true, err
	}
	return result, true, nil
}

func (c *cursor) advanceOrderedSet(ctx context.Context, g *group, index int, row []model.Value) error {
	call := c.calls[index]
	if !qualifies(call, row) {
		return nil
	}
	if !call.op.hypotheticalSet() && row[call.order[0].Column].IsNull {
		return nil
	}
	state := &g.states[index]
	if state.count == math.MaxInt64 {
		return overflow()
	}
	if g.orderedSet == nil {
		var err error
		g.orderedSet, err = c.newSpool(ctx, c.layouts, true)
		if err != nil {
			return err
		}
	}
	if _, err := g.orderedSet.AppendRow(ctx, row); err != nil {
		return err
	}
	state.count++
	return nil
}

func (r *orderedRows) visit(ctx context.Context, index int, visit func(rowstore.Position, []model.Value) error) error {
	if r == nil || r.reader == nil {
		return nil
	}
	for {
		var position rowstore.Position
		var row []model.Value
		var err error
		if r.hasPending {
			position = r.pending
			r.hasPending = false
			row, err = r.reader.Read(ctx, position)
		} else {
			position, row, err = r.reader.NextRow(ctx)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		tag := int(row[r.tag].IntVal)
		if tag > index {
			r.pending = position
			r.hasPending = true
			return nil
		}
		if tag < index {
			return columnar.ErrValueLayout
		}
		if err := visit(position, row); err != nil {
			return err
		}
	}
}

func percentileError() error {
	return &model.Error{Code: "22003", Message: "percentile value must be between 0 and 1"}
}

func percentileFraction(value model.Value) (float64, bool, error) {
	if value.IsNull {
		return 0, true, nil
	}
	if value.Type != model.TypeFloat64 {
		return 0, false, columnar.ErrValueLayout
	}
	if math.IsNaN(value.FloatVal) || value.FloatVal < 0 || value.FloatVal > 1 {
		return 0, false, percentileError()
	}
	return value.FloatVal, false, nil
}

func discreteRow(fraction float64, count int64) int64 {
	row := int64(math.Ceil(fraction*float64(count))) - 1
	return max(row, 0)
}

func continuousRows(fraction float64, count int64) (int64, int64, float64) {
	position := fraction * float64(count-1)
	lower := int64(math.Floor(position))
	upper := int64(math.Ceil(position))
	return lower, upper, position - float64(lower)
}

func (c *cursor) interpolate(ctx context.Context, call compiledCall, lower, upper model.Value, proportion float64) (model.Value, error) {
	if proportion == 0 {
		return lower, nil
	}
	if call.output.Storage == model.TypeFloat64 || call.element == model.TypeFloat64 {
		return model.Float64Val(lower.FloatVal + proportion*(upper.FloatVal-lower.FloatVal)), nil
	}
	budget := builtin.CallBudget{OutputBytes: int64(c.options.MaxRecordBytes), MemoryBytes: c.options.MaxRecordBytes * 8}
	difference, err := c.intervalSubtract.Call(ctx, []model.Value{upper, lower}, budget)
	if err != nil {
		return model.Value{}, err
	}
	scaled, err := c.intervalMultiply.Call(ctx, []model.Value{difference, model.Float64Val(proportion)}, budget)
	if err != nil {
		return model.Value{}, err
	}
	return c.intervalAdd.Call(ctx, []model.Value{lower, scaled}, budget)
}

func (c *cursor) finishPercentile(ctx context.Context, rows *orderedRows, index int, call compiledCall, state *transition, direct model.Value) (model.Value, error) {
	arrayResult := call.arrayPercentile
	if arrayResult && state.count == 0 {
		return model.NullVal(model.TypeArray), nil
	}
	if !arrayResult {
		fraction, null, err := percentileFraction(direct)
		if err != nil {
			return model.NullVal(call.output.Storage), err
		}
		if null {
			err = rows.visit(ctx, index, func(rowstore.Position, []model.Value) error { return nil })
			return model.NullVal(call.output.Storage), err
		}
		if state.count == 0 {
			return model.NullVal(call.output.Storage), nil
		}
		lower, upper, proportion := discreteRow(fraction, state.count), int64(0), float64(0)
		if call.op == percentileContinuous {
			lower, upper, proportion = continuousRows(fraction, state.count)
		} else {
			upper = lower
		}
		var low, high model.Value
		rowIndex := int64(0)
		err = rows.visit(ctx, index, func(_ rowstore.Position, row []model.Value) error {
			if rowIndex == lower {
				if call.op == percentileDiscrete {
					owned, _, err := model.CopyValue(ctx, row[call.order[0].Column], c.options.MaxRecordBytes)
					if err != nil {
						return err
					}
					low = owned
				} else {
					low = row[call.order[0].Column]
				}
			}
			if rowIndex == upper {
				high = row[call.order[0].Column]
			}
			rowIndex++
			return nil
		})
		if err != nil {
			return model.Value{}, err
		}
		if call.op == percentileContinuous {
			return c.interpolate(ctx, call, low, high, proportion)
		}
		return low, nil
	}
	if direct.IsNull {
		err := rows.visit(ctx, index, func(rowstore.Position, []model.Value) error { return nil })
		return model.NullVal(model.TypeArray), err
	}
	if direct.Type != model.TypeArray {
		return model.Value{}, columnar.ErrValueLayout
	}
	if err := model.ValidateArrayShape(direct.Dimensions, uint64(len(direct.ArrayVal))); err != nil {
		return model.Value{}, err
	}
	points := make([]percentilePoint, 0, len(direct.ArrayVal)*2)
	proportions := make([]float64, len(direct.ArrayVal))
	result := model.Value{Type: model.TypeArray, Dimensions: append([]model.ArrayDimension(nil), direct.Dimensions...), ArrayVal: make([]model.Value, len(direct.ArrayVal))}
	for i := range result.ArrayVal {
		result.ArrayVal[i] = model.NullVal(call.element)
		fraction, null, err := percentileFraction(direct.ArrayVal[i])
		if err != nil {
			return model.Value{}, err
		}
		if null {
			continue
		}
		lower, upper, proportion := discreteRow(fraction, state.count), int64(0), float64(0)
		if call.op == percentileContinuous {
			lower, upper, proportion = continuousRows(fraction, state.count)
		} else {
			upper = lower
		}
		points = append(points, percentilePoint{row: lower, output: i})
		if upper != lower {
			points = append(points, percentilePoint{row: upper, output: i, upper: true})
		}
		proportions[i] = proportion
	}
	sort.Slice(points, func(i, j int) bool { return points[i].row < points[j].row })
	upperValues := make([]model.Value, len(result.ArrayVal))
	base := uint64(len(result.ArrayVal))*uint64(unsafe.Sizeof(model.Value{})) + uint64(len(result.Dimensions))*uint64(unsafe.Sizeof(model.ArrayDimension{}))
	if base > c.options.MaxRecordBytes {
		return model.Value{}, model.ErrValueBudget
	}
	remaining := c.options.MaxRecordBytes - base
	next := 0
	rowIndex := int64(0)
	if err := rows.visit(ctx, index, func(_ rowstore.Position, row []model.Value) error {
		for next < len(points) && points[next].row == rowIndex {
			point := points[next]
			value := row[call.order[0].Column]
			if call.op == percentileDiscrete {
				owned, used, err := model.CopyValue(ctx, value, remaining)
				if err != nil {
					return err
				}
				result.ArrayVal[point.output], remaining = owned, remaining-used
			} else if point.upper {
				upperValues[point.output] = value
			} else {
				result.ArrayVal[point.output] = value
			}
			next++
		}
		rowIndex++
		return nil
	}); err != nil {
		return model.Value{}, err
	}
	if call.op == percentileContinuous {
		for i, proportion := range proportions {
			if result.ArrayVal[i].IsNull {
				continue
			}
			upper := result.ArrayVal[i]
			if proportion != 0 {
				upper = upperValues[i]
			}
			value, err := c.interpolate(ctx, call, result.ArrayVal[i], upper, proportion)
			if err != nil {
				return model.Value{}, err
			}
			result.ArrayVal[i] = value
		}
	}
	return result, nil
}

func (c *cursor) finishMode(ctx context.Context, rows *orderedRows, index int, call compiledCall) (model.Value, error) {
	var lastKey []byte
	var runStart, bestStart rowstore.Position
	var run, best int64
	first := true
	if err := rows.visit(ctx, index, func(position rowstore.Position, row []model.Value) error {
		key := row[rows.tag+1].BytesVal
		if first || !bytes.Equal(lastKey, key) {
			if !first && run > best {
				best, bestStart = run, runStart
			}
			lastKey = append(lastKey[:0], key...)
			runStart, run, first = position, 0, false
		}
		run++
		return nil
	}); err != nil {
		return model.Value{}, err
	}
	if first {
		return model.NullVal(call.output.Storage), nil
	}
	if run > best {
		bestStart = runStart
	}
	row, err := rows.reader.Read(ctx, bestStart)
	if err != nil {
		return model.Value{}, err
	}
	value, _, err := model.CopyValue(ctx, row[call.order[0].Column], c.options.MaxRecordBytes)
	return value, err
}

func (c *cursor) finishHypothetical(ctx context.Context, rows *orderedRows, index int, call compiledCall, direct []model.Value, count int64) (model.Value, error) {
	key, err := c.options.Encoder.EncodePrefix(ctx, call.orderKey, direct, c.options.MaxRecordBytes)
	if err != nil {
		return model.Value{}, err
	}
	var before, through, dense int64
	var previous []byte
	if err := rows.visit(ctx, index, func(_ rowstore.Position, row []model.Value) error {
		regular := row[rows.tag+1].BytesVal
		comparison := bytes.Compare(regular, key)
		if comparison < 0 {
			before++
			if previous == nil || !bytes.Equal(previous, regular) {
				dense++
				previous = append(previous[:0], regular...)
			}
		}
		if comparison <= 0 {
			through++
		}
		return nil
	}); err != nil {
		return model.Value{}, err
	}
	switch call.op {
	case hypotheticalRank:
		if before == math.MaxInt64 {
			return model.Value{}, overflow()
		}
		return model.Int64Val(before + 1), nil
	case hypotheticalPercentRank:
		if count == 0 {
			return model.Float64Val(0), nil
		}
		return model.Float64Val(float64(before) / float64(count)), nil
	case hypotheticalCumeDist:
		return model.Float64Val((float64(through) + 1) / (float64(count) + 1)), nil
	case hypotheticalDenseRank:
		if dense == math.MaxInt64 {
			return model.Value{}, overflow()
		}
		return model.Int64Val(dense + 1), nil
	}
	return model.Value{}, columnar.ErrValueLayout
}

func (c *cursor) finishOrderedSet(ctx context.Context, rows *orderedRows, index int, call compiledCall, state *transition, direct []model.Value) (model.Value, error) {
	switch call.op {
	case percentileDiscrete, percentileContinuous:
		return c.finishPercentile(ctx, rows, index, call, state, direct[0])
	case modeAggregate:
		return c.finishMode(ctx, rows, index, call)
	case hypotheticalRank, hypotheticalPercentRank, hypotheticalCumeDist, hypotheticalDenseRank:
		return c.finishHypothetical(ctx, rows, index, call, direct, state.count)
	}
	return model.Value{}, columnar.ErrValueLayout
}

func (g *group) closeOrderedSet() error {
	if g.orderedSet == nil {
		return nil
	}
	err := g.orderedSet.Close()
	g.orderedSet = nil
	return err
}

func openOrderedRows(ctx context.Context, g *group, tag int) (*orderedRows, error) {
	if g.orderedSet == nil {
		return &orderedRows{tag: tag}, nil
	}
	if err := g.orderedSet.Seal(ctx); err != nil {
		return nil, err
	}
	reader, err := g.orderedSet.Reader(ctx)
	if err != nil {
		return nil, err
	}
	return &orderedRows{reader: reader, tag: tag}, nil
}

func (r *orderedRows) Close() error {
	if r == nil || r.reader == nil {
		return nil
	}
	err := r.reader.Close()
	r.reader = nil
	return err
}

