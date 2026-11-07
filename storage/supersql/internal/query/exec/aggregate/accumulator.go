package aggregate

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

// AccumulatorOptions describes one aggregate without grouping, DISTINCT or ordering.
// InputLayouts contains the signature's arguments in SQL order (y, x for pairs).
// Query and Encoder are borrowed; Close releases the accumulator's work claim.
type AccumulatorOptions struct {
	Function       model.FunctionID
	InputCollation model.CollationID
	InputLayouts   []model.TypeLayout
	OutputLayout   model.TypeLayout
	Encoder        storage.KeyEncoder
	Query          *runtime.Query
	Builtin        builtin.ResolveOptions
	MaxRecordBytes uint64
	WorkBytes      uint64
	// Aggregates supplies SQL-defined aggregate declarations, exactly as in
	// Options, so a windowed aggregate resolves through the same seam.
	Aggregates AggregateResolver
}

// Accumulator uses the same transitions and finalization as grouped aggregation.
// Calls require serialization. Result is borrowed until Advance, Reset or Close.
type Accumulator struct {
	engine cursor
	group  group
	work   uint64
}

func NewAccumulator(ctx context.Context, options AccumulatorOptions) (*Accumulator, error) {
	if options.Query == nil || options.Encoder == nil || len(options.InputLayouts) > 2 || options.MaxRecordBytes < 128 || options.MaxRecordBytes > uint64(math.MaxInt)/128 {
		return nil, runtime.ErrInvalidConfig
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	work := uint64(4096) + options.MaxRecordBytes*40
	if isJSONAggregate(options.Function) {
		work += 4096
	}
	if isRangeAggregate(options.Function) {
		work += 4096
	}
	if isScalarAggregate(options.Function) {
		work += uint64(unsafe.Sizeof(scalarAggregatePlan{}))
	}
	if options.Function >= 2819 && options.Function <= 2829 {
		work += uint64(unsafe.Sizeof(pairedMoments{}))
	}
	numericInput := len(options.InputLayouts) == 1 && options.InputLayouts[0].Type.ID == 1700
	numericArithmetic := !isJSONAggregate(options.Function) && options.Function != 6291 && options.Function != 2335 && options.Function != 4053 && (options.OutputLayout.Type.ID == 1700 || numericInput)
	if numericArithmetic {
		work += max(4<<20, builtin.NumericMomentsWorkBytes)
	}
	if work > options.WorkBytes {
		return nil, resource("aggregate accumulator exceeds workspace", nil)
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(work)
	if err != nil {
		return nil, err
	}
	a := &Accumulator{work: work}
	a.engine.claim = claim
	if err := a.configure(ctx, options); err != nil {
		return nil, errors.Join(err, a.Close())
	}
	if err := a.Reset(); err != nil {
		return nil, errors.Join(err, a.Close())
	}
	return a, nil
}

func (a *Accumulator) configure(ctx context.Context, options AccumulatorOptions) error {
	c := &a.engine
	c.options = Options{Query: options.Query, Encoder: options.Encoder, Builtin: options.Builtin, MaxRecordBytes: options.MaxRecordBytes, Aggregates: options.Aggregates}
	c.layouts = make([]model.TypeLayout, len(options.InputLayouts))
	remaining := uint64(2048)
	arguments := make([]int, len(c.layouts))
	for i, layout := range options.InputLayouts {
		owned, used, err := model.CopyTypeLayout(ctx, layout, remaining)
		if err != nil {
			return err
		}
		c.layouts[i], remaining = owned, remaining-used
		arguments[i] = i
	}
	output, used, err := model.CopyTypeLayout(ctx, options.OutputLayout, remaining)
	if err != nil {
		return err
	}
	remaining -= used
	c.options.OutputLayouts = []model.TypeLayout{output}
	call, err := compileCall(ctx, Call{Function: options.Function, InputCollation: options.InputCollation, Arguments: arguments}, output, c.layouts, &c.options)
	if err != nil {
		return err
	}
	call.key, _, err = storage.CopyKeyLayout(ctx, call.key, remaining)
	if err != nil {
		return err
	}
	// The accumulator drives the same cursor methods as grouped aggregation, so
	// its per-row scratch is sized to this single call, matching cursor.configure.
	c.arguments = make([]model.Value, call.arity)
	c.directValues = make([]model.Value, call.directCount)
	c.calls = []compiledCall{call}
	c.options.Builtin = builtin.ResolveOptions{}
	c.output = make([]model.Value, 1)
	a.group.states = make([]transition, 1)
	// A zero-argument aggregate such as count(*) has no input type to compare.
	if call.arity != 0 && call.op.numericArithmetic() && (call.output.ID == 1700 || call.inputs[0].ID == 1700) {
		if c.numericAdd, err = builtin.Resolve(1724, builtin.ResolveOptions{}); err != nil {
			return err
		}
		if c.numericDivide, err = builtin.Resolve(1727, builtin.ResolveOptions{}); err != nil {
			return err
		}
		if c.numericTypmod, err = builtin.Resolve(1703, builtin.ResolveOptions{}); err != nil {
			return err
		}
	}
	return c.check(ctx)
}

func (a *Accumulator) WorkBytes() uint64 { return a.work }

func (a *Accumulator) Reset() error {
	if !a.engine.closed {
		a.group.clearArrays()
		a.group.clearJSON()
		a.group.clearRanges()
		if err := a.group.releaseNumeric(); err != nil {
			return err
		}
		paired := a.group.states[0].paired
		if a.engine.calls[0].op.pairedMoments() {
			if paired == nil {
				paired = new(pairedMoments)
			} else {
				*paired = pairedMoments{}
			}
		}
		call := a.engine.calls[0]
		a.group.states[0] = transition{value: model.NullVal(call.output.Storage), paired: paired}
		if call.op == userDefinedAggregate {
			// The declared transition state — INITCOND or NULL — is the
			// accumulator's starting state, matching grouped aggregation.
			a.group.states[0].value = call.user.initialTransition().value
		}
		clear(a.engine.output)
	}
	return nil
}

// Advance borrows exactly the signature's arguments for this call only.
// JSON aggregates inspect NULLs themselves; string_agg permits a NULL delimiter.
func (a *Accumulator) Advance(ctx context.Context, arguments []model.Value) (err error) {
	if a.engine.closed {
		return runtime.ErrClosed
	}
	defer func() {
		if err != nil && (a.engine.calls[0].op == jsonAggregate || a.engine.calls[0].op.rangeAggregation()) {
			err = errors.Join(err, a.Close())
		}
	}()
	if err := a.engine.check(ctx); err != nil {
		return err
	}
	call := a.engine.calls[0]
	if len(arguments) != call.arity {
		return columnar.ErrValueLayout
	}
	for i, value := range arguments {
		if !value.IsNull && value.Type != call.inputs[i].Storage {
			return columnar.ErrValueLayout
		}
	}
	if call.op == anyValue || call.op == minimum || call.op == maximum {
		if err := validateArgumentValue(ctx, arguments[0], a.engine.layouts[0]); err != nil {
			return err
		}
	}
	if !call.acceptsArguments(arguments) {
		return nil
	}
	next := a.group.states[0]
	if err := a.engine.advance(ctx, call, &next, arguments); err != nil {
		return err
	}
	a.group.states[0] = next
	return nil
}

func (a *Accumulator) Result(ctx context.Context) (result model.Value, err error) {
	if a.engine.closed {
		return model.Value{}, runtime.ErrClosed
	}
	defer func() {
		if err != nil && (a.engine.calls[0].op == jsonAggregate || a.engine.calls[0].op.rangeAggregation()) {
			err = errors.Join(err, a.Close())
		}
	}()
	if err := a.engine.check(ctx); err != nil {
		return model.Value{}, err
	}
	values, err := a.engine.finish(ctx, &a.group)
	if err != nil {
		return model.Value{}, err
	}
	return values[0], nil
}

func validateArgumentValue(ctx context.Context, value model.Value, layout model.TypeLayout) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if value.IsNull {
		return nil
	}
	if value.Type != layout.Type.Storage {
		return columnar.ErrValueLayout
	}
	switch value.Type {
	case model.TypeArray:
		if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
			return err
		}
		for _, child := range value.ArrayVal {
			if err := validateArgumentValue(ctx, child, layout.Children[0]); err != nil {
				return err
			}
		}
	case model.TypeComposite:
		if len(value.ArrayVal) != len(layout.Children) {
			return columnar.ErrValueLayout
		}
		for i, child := range value.ArrayVal {
			if err := validateArgumentValue(ctx, child, layout.Children[i]); err != nil {
				return err
			}
		}
	case model.TypeBoolean:
		if value.IntVal != 0 && value.IntVal != 1 {
			return columnar.ErrValueLayout
		}
	}
	return nil
}

func (a *Accumulator) Close() error {
	a.group.clearArrays()
	a.group.clearJSON()
	a.group.clearRanges()
	err := a.group.releaseNumeric()
	a.group = group{}
	return errors.Join(err, a.engine.Close())
}
