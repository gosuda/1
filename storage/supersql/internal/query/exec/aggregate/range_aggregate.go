package aggregate

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type rangeAggregatePlan struct {
	function builtin.Callable
	multi    bool
}

// Payload and end offsets preserve the input order through final normalization.
// Empty members remain entries: removing them can change numeric representatives
// selected by PostgreSQL's unstable range sort.
type rangeAggregateState struct {
	payload []byte
	ends    []int
}

func isRangeAggregate(id model.FunctionID) bool {
	return id == 4301 || id == 4389 || id == 4450 || id == 6227
}

func (op operation) rangeAggregation() bool {
	return op == rangeUnionAggregate || op == rangeIntersectAggregate
}

func compileRangeAggregate(ctx context.Context, id model.FunctionID, input, output model.TypeLayout, options builtin.ResolveOptions) (*rangeAggregatePlan, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	if err := output.Validate(); err != nil {
		return nil, err
	}
	family, ok := builtin.ResolveRangeFamily(options, input.Type.ID)
	multi := id == 4389 || id == 6227
	wantInput, wantOutput := family.Range, family.Multirange
	if multi {
		wantInput = family.Multirange
	}
	if id == 4450 {
		wantOutput = family.Range
	}
	validInput := ok && input.Type.ID == wantInput && input.Type.Storage == model.TypeBytes && len(input.Children) == 0
	validOutput := output.Type.ID == wantOutput && output.Type.Storage == model.TypeBytes && len(output.Children) == 0
	validMetadata := output.Type.Modifier == -1 && input.Type.Collation == family.Collation && output.Type.Collation == family.Collation
	if !validInput || !validOutput || !validMetadata {
		return nil, columnar.ErrValueLayout
	}
	function := model.FunctionID(3868)
	arguments := []model.TypeLayout{input, input}
	if id == 4389 {
		function = 4272
	}
	if id == 4301 || id == 6227 {
		function = 4282
		switch family.Range {
		case 3904:
			function = 4282
		case 3906:
			function = 4285
		case 3908:
			function = 4288
		case 3910:
			function = 4291
		case 3912:
			function = 4294
		case 3926:
			function = 4297
		}
		array, known := builtin.ResolveRangeArrayLayout(options, family.Range)
		if !known {
			return nil, columnar.ErrValueLayout
		}
		arguments = []model.TypeLayout{array}
	}
	callable, err := builtin.ResolveTyped(ctx, function, arguments, output, options, 4096)
	if err != nil {
		return nil, err
	}
	return &rangeAggregatePlan{function: callable, multi: multi}, nil
}

func (c *cursor) rangeBudget() builtin.CallBudget {
	// The remaining 8M of shared scratch admits finalization's borrowed headers.
	return builtin.CallBudget{OutputBytes: int64(c.options.MaxRecordBytes), MemoryBytes: c.options.MaxRecordBytes * 24}
}

func (c *cursor) appendRangeMember(ctx context.Context, state *rangeAggregateState, data []byte) error {
	limit := c.options.MaxRecordBytes * 6
	const offsetBytes = uint64(unsafe.Sizeof(int(0)))
	const valueBytes = uint64(unsafe.Sizeof(model.Value{}))
	count := len(state.ends) + 1
	if count > math.MaxInt32 || uint64(count) > c.options.MaxRecordBytes*8/valueBytes || uint64(len(data)) > limit || uint64(len(state.payload)) > limit-uint64(len(data)) {
		return resource("range_agg state exceeds byte limit", model.ErrValueBudget)
	}
	length := len(state.payload) + len(data)
	if uint64(count)*offsetBytes > limit-uint64(length) {
		return resource("range_agg state exceeds byte limit", model.ErrValueBudget)
	}
	payloadCapacity := max(length, cap(state.payload))
	endsCapacity := max(count, cap(state.ends))
	if uint64(payloadCapacity)+uint64(endsCapacity)*offsetBytes > limit {
		payloadCapacity, endsCapacity = length, count
	}
	if length > cap(state.payload) {
		payloadCapacity = min(int(limit-uint64(endsCapacity)*offsetBytes), max(length, cap(state.payload)*2))
	}
	if count > cap(state.ends) {
		endsCapacity = min(int((limit-uint64(payloadCapacity))/offsetBytes), max(count, cap(state.ends)*2))
	}
	if payloadCapacity != cap(state.payload) {
		next := make([]byte, len(state.payload), payloadCapacity)
		if err := c.copyRangeBytes(ctx, next, state.payload); err != nil {
			return err
		}
		state.payload = next
	}
	if endsCapacity != cap(state.ends) {
		next := make([]int, len(state.ends), endsCapacity)
		for start := 0; start < len(next); start += 256 {
			if err := c.check(ctx); err != nil {
				return err
			}
			copy(next[start:min(start+256, len(next))], state.ends[start:])
		}
		state.ends = next
	}
	buffer := state.payload[:length]
	if err := c.copyRangeBytes(ctx, buffer[len(state.payload):], data); err != nil {
		return err
	}
	state.payload = buffer
	state.ends = append(state.ends, length)
	return nil
}

func (c *cursor) copyRangeBytes(ctx context.Context, dst, src []byte) error {
	for start := 0; start < len(src); start += stringAggregateCopyBytes {
		if err := c.check(ctx); err != nil {
			return err
		}
		copy(dst[start:min(start+stringAggregateCopyBytes, len(src))], src[start:])
	}
	return c.check(ctx)
}

func (c *cursor) advanceRangeAggregate(ctx context.Context, call compiledCall, state *transition, value model.Value) error {
	if !call.rangePlan.multi || call.op == rangeIntersectAggregate {
		if err := builtin.ValidateRangeValue(ctx, call.inputs[0].ID, value.BytesVal); err != nil {
			return err
		}
	}
	if call.op == rangeIntersectAggregate {
		var result model.Value
		var err error
		if state.value.IsNull {
			result, _, err = model.CopyValue(ctx, value, c.options.MaxRecordBytes)
		} else {
			arguments := [2]model.Value{state.value, value}
			result, err = call.rangePlan.function.Call(ctx, arguments[:], c.rangeBudget())
		}
		if err != nil {
			return err
		}
		if err := c.check(ctx); err != nil {
			return err
		}
		state.value = result
		state.count++
		return nil
	}
	if state.ranges == nil {
		state.ranges = new(rangeAggregateState)
	}
	if !call.rangePlan.multi {
		if err := c.appendRangeMember(ctx, state.ranges, value.BytesVal); err != nil {
			return err
		}
	} else {
		iterator, err := builtin.NewMultirangeIterator(ctx, call.inputs[0].ID, value)
		if err != nil {
			return err
		}
		defer iterator.Close()
		members := 0
		for {
			data, more, err := iterator.NextBorrowed(ctx)
			if err != nil {
				return err
			}
			if !more {
				break
			}
			if err := c.appendRangeMember(ctx, state.ranges, data); err != nil {
				return err
			}
			members++
		}
		if members == 0 {
			// Canonical encoded empty range, retained as PostgreSQL's transition does.
			empty := [1]byte{1}
			if err := c.appendRangeMember(ctx, state.ranges, empty[:]); err != nil {
				return err
			}
		}
	}
	state.count++
	state.value = model.NullVal(model.TypeBytes)
	return nil
}

func (c *cursor) finishRangeAggregate(ctx context.Context, call compiledCall, state *transition) (model.Value, error) {
	if call.op == rangeIntersectAggregate || state.count == 0 || !state.value.IsNull {
		return state.value, nil
	}
	s := state.ranges
	values := make([]model.Value, len(s.ends))
	defer clear(values)
	start := 0
	for i, end := range s.ends {
		if err := c.check(ctx); err != nil {
			return model.Value{}, err
		}
		values[i] = model.BytesVal(s.payload[start:end:end])
		start = end
	}
	argument := [1]model.Value{{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}}}
	result, err := call.rangePlan.function.Call(ctx, argument[:], c.rangeBudget())
	if err != nil {
		return model.Value{}, err
	}
	if err := c.check(ctx); err != nil {
		return model.Value{}, err
	}
	state.value = result
	return result, nil
}

func (g *group) clearRanges() {
	for i := range g.states {
		g.states[i].ranges = nil
		if g.states[i].value.Type == model.TypeBytes {
			g.states[i].value.BytesVal = nil
		}
	}
}
