package window

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type aggregateState struct {
	accumulator *aggregate.Accumulator
	frame       span
	valid       bool
}

func (c *cursor) configureAggregates(ctx context.Context, remaining uint64) error {
	c.aggregates = make([]aggregateState, len(c.options.Calls))
	for i, call := range c.options.Calls {
		signature, ok := builtin.LookupSignature(call.Function)
		if !ok && c.options.Aggregates != nil {
			// A declared aggregate has no pinned signature: its result layout and
			// argument layouts come from the declaration.
			declaration, declared, err := c.options.Aggregates.ResolveAggregate(ctx, call.Function)
			if err != nil {
				return err
			}
			if declared {
				inputs := make([]model.TypeLayout, len(declaration.Arguments))
				for j := range inputs {
					inputs[j] = declaration.Arguments[j]
				}
				accumulator, err := aggregate.NewAccumulator(ctx, aggregate.AccumulatorOptions{
					Function: call.Function, InputCollation: call.InputCollation, InputLayouts: inputs,
					OutputLayout: c.options.OutputLayouts[len(c.options.OutputColumns)+i],
					Query:        c.options.Query, Encoder: c.options.Encoder,
					Builtin:        c.options.Builtin,
					MaxRecordBytes: c.options.MaxRecordBytes, WorkBytes: remaining,
					Aggregates: c.options.Aggregates,
				})
				if err != nil {
					return err
				}
				c.aggregates[i].accumulator = accumulator
				remaining -= accumulator.WorkBytes()
				continue
			}
		}
		if !ok || signature.Kind != 'a' {
			continue
		}
		inputs := make([]model.TypeLayout, len(call.Arguments))
		for j := range inputs {
			inputs[j] = call.argumentLayout(c.options.InputLayouts, j)
		}
		accumulator, err := aggregate.NewAccumulator(ctx, aggregate.AccumulatorOptions{
			Function: call.Function, InputCollation: call.InputCollation, InputLayouts: inputs,
			OutputLayout: c.options.OutputLayouts[len(c.options.OutputColumns)+i],
			Query:        c.options.Query, Encoder: c.options.Encoder,
			Builtin:        c.options.Builtin,
			MaxRecordBytes: c.options.MaxRecordBytes, WorkBytes: remaining,
		})
		if err != nil {
			var unsupported *aggregate.UnsupportedError
			if errors.As(err, &unsupported) {
				return &UnsupportedError{Function: call.Function, Reason: unsupported.Reason}
			}
			return err
		}
		c.aggregates[i].accumulator = accumulator
		remaining -= accumulator.WorkBytes()
	}
	return nil
}

func checkAggregateSupport(ctx context.Context, call Call, options Options, output model.TypeLayout) error {
	signature, ok := builtin.LookupSignature(call.Function)
	if !ok || signature.Kind != 'a' {
		return nil
	}
	inputs := make([]model.TypeLayout, len(call.Arguments))
	arguments := make([]int, len(inputs))
	for i := range inputs {
		inputs[i], arguments[i] = call.argumentLayout(options.InputLayouts, i), i
	}
	err := aggregate.CheckSupport(ctx, aggregate.Options{InputLayouts: inputs, OutputLayouts: []model.TypeLayout{output}, Calls: []aggregate.Call{{Function: call.Function, InputCollation: call.InputCollation, Arguments: arguments}}, Encoder: options.Encoder, Builtin: options.Builtin, MaxRecordBytes: options.MaxRecordBytes})
	var unsupported *aggregate.UnsupportedError
	if errors.As(err, &unsupported) {
		return &UnsupportedError{Function: call.Function, Reason: unsupported.Reason}
	}
	return err
}

func (c *cursor) aggregate(ctx context.Context, call Call, state *aggregateState, group, peerStart, peerEnd int64) (model.Value, error) {
	spans, count, err := c.frame(ctx, group, peerStart, peerEnd)
	if err != nil {
		return model.Value{}, err
	}
	// A fixed frame head consumes each row once as the tail grows, including
	// volatile arguments. Exclusions restart, as do moving heads without inverse transitions.
	reuse := c.options.Frame.Exclusion == ir.ExcludeNone && count == 1 && state.valid && spans[0].start == state.frame.start && spans[0].end >= state.frame.end
	endFrame := spans[0]
	if reuse {
		spans[0].start = state.frame.end
	} else {
		if err := state.accumulator.Reset(); err != nil {
			return model.Value{}, err
		}
	}
	for _, frame := range spans[:count] {
		for index := frame.start; index < frame.end; index++ {
			if err := c.check(ctx); err != nil {
				return model.Value{}, err
			}
			pos, _, err := c.rows.read(ctx, index)
			if err != nil {
				return model.Value{}, err
			}
			row, err := c.reader.Read(ctx, rowstore.Position(pos))
			if err != nil {
				return model.Value{}, err
			}
			if call.Filter != nil {
				filter, err := call.Filter.Eval(ctx, row)
				if err != nil {
					return model.Value{}, err
				}
				if filter.Type != model.TypeBoolean {
					return model.Value{}, columnar.ErrValueLayout
				}
				if filter.IsNull || filter.IntVal != 1 {
					continue
				}
			}
			arguments := c.aggregateArguments[:len(call.Arguments)]
			for i := range call.Arguments {
				arguments[i], err = c.argument(ctx, call, i, row)
				if err != nil {
					return model.Value{}, err
				}
			}
			if err := state.accumulator.Advance(ctx, arguments); err != nil {
				return model.Value{}, err
			}
		}
	}
	state.frame, state.valid = endFrame, count == 1
	return state.accumulator.Result(ctx)
}

func additionalAggregateArgumentCopies(call Call) int {
	if len(call.Expressions) < 2 {
		return 0
	}
	// A declared aggregate has no pinned signature; its expressions still carry
	// borrowed payloads across evaluation, so the same accounting applies.
	if signature, found := builtin.LookupSignature(call.Function); found && signature.Kind != 'a' {
		return 0
	}
	copies := 0
	for _, expression := range call.Expressions {
		if expression == nil {
			continue
		}
		switch expression.Layout().Type.Storage {
		case model.TypeString, model.TypeBytes, model.TypeNumeric, model.TypeJSONB, model.TypeUUID, model.TypeArray, model.TypeComposite, model.TypeVectorF32:
			copies++
		}
	}
	return max(0, copies-1)
}
