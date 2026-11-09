package exchange

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type source struct {
	cursor            storage.BatchCursor
	builder           *columnar.Builder
	input, batch      columnar.Batch
	row               []model.Value
	selection         []uint32
	inputIndex, index int
	future            *runtime.Future
	err               error
	eof               bool
}

// Sources owns its inputs. Next calls are serialized; Close may interrupt Next.
// Views remain borrowed until the next Next on the same index, or Close.
// The query and its task group remain borrowed.
type Sources struct {
	mu              sync.Mutex
	options         Options
	budget          sourceBudget
	inputs          []source
	ctx             context.Context
	cancel          context.CancelCauseFunc
	stopQuery       func() bool
	queue, metadata columnar.Allocation
	parallel        bool
	closed          bool
	closeErr        error
}

// NewSources takes every input even on failure; construction never pulls one.
func NewSources(ctx context.Context, inputs []storage.BatchCursor, options Options) (*Sources, error) {
	budget, err := checkSources(ctx, len(inputs), options)
	if err == nil {
		for _, input := range inputs {
			if input == nil {
				err = runtime.ErrInvalidConfig
				break
			}
		}
	}
	if err != nil {
		return nil, errors.Join(err, closeSourceInputs(inputs))
	}
	metadata, err := options.Query.Allocator(runtime.WorkMemory).Reserve(budget.metadata)
	if err != nil {
		return nil, errors.Join(err, closeSourceInputs(inputs))
	}
	queue, err := options.Query.Allocator(runtime.WorkMemory).Reserve(budget.queue)
	if err != nil {
		metadata.Release()
		return nil, errors.Join(err, closeSourceInputs(inputs))
	}
	s := &Sources{options: options, budget: budget, metadata: metadata, queue: queue, inputs: make([]source, len(inputs)), parallel: options.ParallelEligible && len(inputs) >= 2 && options.EstimatedRows >= 65536}
	s.ctx, s.cancel = context.WithCancelCause(ctx)
	s.stopQuery = context.AfterFunc(options.Query.Context(), func() { s.cancel(context.Cause(options.Query.Context())) })
	for i, input := range inputs {
		s.inputs[i].cursor = input
	}
	s.options.Layouts = make([]model.TypeLayout, len(options.Layouts))
	for i, layout := range options.Layouts {
		s.options.Layouts[i], _, err = model.CopyTypeLayout(ctx, layout, options.MetadataBytes)
		if err != nil {
			return nil, errors.Join(err, s.Close())
		}
	}
	for i := range s.inputs {
		input := &s.inputs[i]
		input.builder, err = columnar.NewBuilder(s.options.Layouts, columnar.BuilderOptions{MaxRows: options.BatchRows, MaxBytes: budget.slot})
		if err != nil {
			return nil, errors.Join(err, s.Close())
		}
		input.input.Columns = make([]columnar.Vector, 0, len(options.Layouts))
		input.batch.Columns = make([]columnar.Vector, 0, len(options.Layouts))
		input.row = make([]model.Value, len(options.Layouts))
		input.selection = make([]uint32, options.BatchRows)
	}
	return s, nil
}

func closeSourceInputs(inputs []storage.BatchCursor) error {
	var err error
	for _, input := range inputs {
		if input != nil {
			err = errors.Join(err, input.Close())
		}
	}
	return err
}

// pull executes at most one child Next and never waits for a consumer.
func (s *Sources) pull(ctx context.Context, input *source, demand int) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	input.batch.Reset()
	input.builder.Reset()
	input.index = 0
	if input.inputIndex == input.input.Rows() {
		input.input.Reset()
		input.input.MaxRows = demand
		input.inputIndex = 0
		err := input.cursor.Next(ctx, &input.input)
		if err != nil {
			return err
		}
		if err := input.input.Validate(max(input.input.Length, demand)); err != nil {
			return err
		}
		if input.input.Rows() == 0 {
			return input.builder.Publish(&input.batch)
		}
		if input.input.Rows() > demand || len(input.input.Columns) != len(s.options.Layouts) {
			return columnar.ErrInvalidBatch
		}
		for i, layout := range s.options.Layouts {
			if !input.input.Columns[i].MatchesLayout(layout) {
				return columnar.ErrValueLayout
			}
		}
	}
	for input.inputIndex < input.input.Rows() && input.builder.Rows() < demand {
		if err := copySourceRow(ctx, &input.input, input.inputIndex, input.row, s.budget.slot); err != nil {
			return exchangeCopyError(err)
		}
		err := input.builder.Append(ctx, input.row)
		clear(input.row)
		if errors.Is(err, columnar.ErrBatchBudget) && input.builder.Rows() > 0 {
			break
		}
		if err != nil {
			return exchangeCopyError(err)
		}
		input.inputIndex++
	}
	return input.builder.Publish(&input.batch)
}

func exchangeCopyError(err error) error {
	if errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, model.ErrValueBudget) {
		return exchangeBudget("copy exchange row", err)
	}
	return err
}

func (s *Sources) run(ctx context.Context, input *source, demand int) {
	err := s.pull(ctx, input, demand)
	if err == io.EOF {
		input.eof = true
		return
	}
	input.err = err
}

func (s *Sources) schedule(input *source, demand int) error {
	future, err := s.options.Query.Tasks().TrySubmit(s.ctx, runtime.Task{Run: func(taskCtx context.Context) error {
		ctx, cancel := context.WithCancelCause(s.ctx)
		stop := context.AfterFunc(taskCtx, func() { cancel(context.Cause(taskCtx)) })
		defer stop()
		defer cancel(nil)
		s.run(ctx, input, demand)
		// Source failures are local. Returning them would cancel unrelated tasks.
		return nil
	}})
	if err != nil {
		return err
	}
	input.future = future
	return nil
}

func (s *Sources) join(ctx context.Context, input *source) error {
	if input.future == nil {
		return nil
	}
	err := input.future.Wait(ctx)
	if ctx.Err() != nil {
		s.cancel(context.Cause(ctx))
		return context.Cause(ctx)
	}
	input.future = nil
	if err != nil && input.err == nil {
		input.err = err
	}
	return err
}

func (s *Sources) next(ctx context.Context, index int, out *columnar.Batch) error {
	if s.closed {
		return errors.Join(runtime.ErrClosed, s.closeErr)
	}
	if out == nil || out.MaxRows < 0 || index < 0 || index >= len(s.inputs) {
		return runtime.ErrInvalidConfig
	}
	out.Reset()
	if err := errors.Join(context.Cause(ctx), context.Cause(s.ctx)); err != nil {
		return err
	}
	demand := s.options.BatchRows
	if out.MaxRows > 0 {
		demand = min(demand, out.MaxRows)
	}
	input := &s.inputs[index]
	for {
		if err := errors.Join(context.Cause(ctx), context.Cause(s.ctx)); err != nil {
			return err
		}
		if err := s.join(ctx, input); err != nil {
			return err
		}
		if input.err != nil {
			return input.err
		}
		if input.index < input.batch.Rows() {
			rows := min(demand, input.batch.Rows()-input.index)
			out.Columns = append(out.Columns[:0], input.batch.Columns...)
			out.Length = input.batch.Length
			if input.index != 0 || rows != input.batch.Rows() {
				for i := range rows {
					input.selection[i] = uint32(input.index + i)
				}
				out.Selection = input.selection[:rows]
			}
			input.index += rows
			return nil
		}
		if input.eof {
			return io.EOF
		}
		if s.parallel {
			// Prioritize the demanded source. Rejected speculative work owns no
			// resources and can wait for a later demand instead of occupying workers.
			err := s.schedule(input, demand)
			if err != nil && !errors.Is(err, runtime.ErrQueueFull) && !errors.Is(err, runtime.ErrBudget) {
				return err
			}
			if err == nil {
				s.prefetch(index, demand)
				continue
			}
		}
		pullCtx, cancel := context.WithCancelCause(s.ctx)
		stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
		s.run(pullCtx, input, demand)
		stop()
		cancel(nil)
	}
}

func (s *Sources) prefetch(exclude, demand int) {
	for i := range s.inputs {
		other := &s.inputs[i]
		busy := i == exclude || other.future != nil
		if busy || other.err != nil || other.eof || other.batch.Rows() != 0 {
			continue
		}
		if err := s.schedule(other, demand); err != nil {
			if !errors.Is(err, runtime.ErrQueueFull) && !errors.Is(err, runtime.ErrBudget) {
				other.err = err
			}
			return
		}
	}
}

func (s *Sources) Next(ctx context.Context, index int, out *columnar.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.next(ctx, index, out)
	if err != nil && err != io.EOF && !s.closed {
		s.cancel(err)
		err = errors.Join(err, s.closeLocked())
	}
	return err
}

func (s *Sources) closeLocked() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.cancel(runtime.ErrClosed)
	if s.stopQuery != nil {
		s.stopQuery()
		s.stopQuery = nil
	}
	for i := range s.inputs {
		input := &s.inputs[i]
		if input.future != nil {
			// The pull is abandoned; its execution result is not a cleanup error.
			// Task cleanup owns no resources: child Close below reports those errors.
			_ = input.future.Wait(context.Background())
			input.future = nil
		}
	}
	for i := range s.inputs {
		input := &s.inputs[i]
		if input.cursor != nil {
			s.closeErr = errors.Join(s.closeErr, input.cursor.Close())
			input.cursor = nil
		}
		input.input.Reset()
		input.batch.Reset()
		clear(input.row)
		if input.builder != nil {
			s.closeErr = errors.Join(s.closeErr, input.builder.Close())
		}
	}
	s.inputs = nil
	s.options.Layouts = nil
	if s.queue != nil {
		s.queue.Release()
		s.queue = nil
	}
	if s.metadata != nil {
		s.metadata.Release()
		s.metadata = nil
	}
	return s.closeErr
}

// Close joins pending pulls and returns resource-cleanup failures only. Next
// reports execution failures; abandoning a prefetched result does not replay it.
func (s *Sources) Close() error {
	s.cancel(runtime.ErrClosed)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}
