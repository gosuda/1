// Package projectset expands target-list set-returning calls over a streaming input.
package projectset

import (
	"context"
	"errors"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

type Source struct {
	Arguments *expr.Program
	Rows      builtin.RowSourcePlan
	Result    model.TypeLayout
	Output    int
	Strict    bool
}

type Options struct {
	Input       storage.BatchCursor
	Passthrough *expr.Program
	PassthroughOutputs []int
	Sources     []Source
	Output      []model.TypeLayout
	Parameters  []expr.ParameterValue
	Captures    *columnar.Batch
	Subqueries  expr.SubqueryRuntime
	Sequences   model.SequenceRuntime
	Relations   model.RelationRuntime
	Temporal    model.TemporalRuntime
	Settings    model.SettingsRuntime
	TextSearch  model.TextSearchRuntime
	Advisory    model.AdvisoryLockRuntime
	Native      model.NativeScalarRuntime
	SessionIntrospection model.SessionIntrospection
	CatalogMetadata      model.CatalogMetadataIntrospection
	Query       *runtime.Query
	BatchRows   int
	BatchBytes  uint64
}

type sourceState struct {
	spec      Source
	arguments *expr.State
	values    []model.Value
	iterator  builtin.RowSourceIterator
	done      bool
}

type cursor struct {
	options     Options
	life        context.Context
	call        context.Context
	cancel      context.CancelCauseFunc
	stopQuery   func() bool
	passthrough *expr.State
	sources     []sourceState
	builder     *columnar.Builder
	claim       columnar.Allocation
	input       columnar.Batch
	rowInput    columnar.Batch
	selection   [1]uint32
	baseValues  []model.Value
	row         []model.Value
	inputRow    int
	pending     bool
	inputEOF    bool
	closed      bool
	closeErr    error
}

// New borrows plans, programs, layouts, parameters, captures and Query until
// Close. It retains one input batch and one row from each live source while an
// output row is pending across backpressure.
func New(ctx context.Context, options Options) (_ storage.BatchCursor, err error) {
	if options.Input == nil || options.Query == nil || len(options.Sources) == 0 || options.BatchRows <= 0 || options.BatchBytes == 0 || options.BatchBytes > uint64(math.MaxInt) {
		return nil, runtime.ErrInvalidConfig
	}
	if len(options.PassthroughOutputs) != 0 && options.Passthrough == nil {
		return nil, expr.ErrInvalid
	}
	if options.Passthrough != nil && options.Passthrough.OutputCount() != len(options.PassthroughOutputs) {
		return nil, expr.ErrInvalid
	}
	argumentValues := 0
	for _, source := range options.Sources {
		if source.Rows == nil || source.Output < 0 || source.Output >= len(options.Output) || source.Result.Type.Storage == model.TypeAny {
			return nil, runtime.ErrInvalidConfig
		}
		if source.Arguments != nil {
			if source.Arguments.OutputCount() > math.MaxInt-argumentValues {
				return nil, runtime.ErrBudget
			}
			argumentValues += source.Arguments.OutputCount()
		}
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(options.Query.Context())); err != nil {
		return nil, err
	}
	base := uint64(unsafe.Sizeof(cursor{})) + uint64(len(options.Sources))*uint64(unsafe.Sizeof(sourceState{}))
	base += uint64(len(options.Output)+len(options.PassthroughOutputs)+argumentValues)*uint64(unsafe.Sizeof(model.Value{}))
	if base > uint64(options.Query.Usage().Work.Limit) {
		return nil, runtime.ErrBudget
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(base)
	if err != nil {
		return nil, err
	}
	call, cancel := context.WithCancelCause(ctx)
	query := options.Query.Context()
	stopQuery := context.AfterFunc(query, func() { cancel(context.Cause(query)) })
	c := &cursor{
		options: options, life: ctx, call: call, cancel: cancel, stopQuery: stopQuery, claim: claim,
		sources: make([]sourceState, len(options.Sources)), baseValues: make([]model.Value, len(options.PassthroughOutputs)), row: make([]model.Value, len(options.Output)),
		input: columnar.Batch{Columns: make([]columnar.Vector, 0, len(options.Output))},
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, c.Close())
		}
	}()
	stateOptions := expr.StateOptions{
		Captures: options.Captures, Subqueries: options.Subqueries, Sequences: options.Sequences,
		Relations: options.Relations, Temporal: options.Temporal, Settings: options.Settings, TextSearch: options.TextSearch, Advisory: options.Advisory, Native: options.Native,
		WorkAllocator: options.Query.Allocator(runtime.WorkMemory), OutputAllocator: options.Query.Allocator(runtime.WorkMemory),
	}
	if options.Passthrough != nil {
		c.passthrough, err = expr.NewState(options.Passthrough, stateOptions)
		if err != nil {
			return nil, err
		}
	}
	for i, source := range options.Sources {
		c.sources[i].spec = source
		if source.Arguments != nil {
			c.sources[i].values = make([]model.Value, source.Arguments.OutputCount())
			c.sources[i].arguments, err = expr.NewState(source.Arguments, stateOptions)
			if err != nil {
				return nil, err
			}
		}
	}
	c.builder, err = columnar.NewBuilder(options.Output, columnar.BuilderOptions{MaxRows: options.BatchRows, MaxBytes: options.BatchBytes, Allocator: options.Query.Allocator(runtime.OutputMemory)})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *cursor) check(ctx context.Context) error {
	return errors.Join(context.Cause(ctx), context.Cause(c.call), context.Cause(c.life), context.Cause(c.options.Query.Context()))
}

func (c *cursor) nextInputRow(ctx context.Context) (bool, error) {
	for c.inputRow >= c.input.Rows() {
		if c.inputEOF {
			return false, nil
		}
		c.input.Reset()
		c.input.MaxRows = c.options.BatchRows
		if err := c.options.Input.Next(ctx, &c.input); err != nil {
			if errors.Is(err, io.EOF) {
				c.inputEOF = true
				return false, nil
			}
			return false, err
		}
		c.inputRow = 0
		if err := c.input.Validate(c.options.BatchRows); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (c *cursor) openOuter(ctx context.Context) error {
	lane := c.input.Lane(c.inputRow)
	c.selection[0] = uint32(lane)
	c.rowInput.Columns = c.input.Columns
	c.rowInput.Length = c.input.Length
	c.rowInput.Selection = c.selection[:]
	if c.passthrough != nil {
		if err := c.passthrough.EvalBorrowedRow(ctx, &c.rowInput, c.options.Parameters, c.baseValues, expr.Direct); err != nil {
			return err
		}
		for i, output := range c.options.PassthroughOutputs {
			c.row[output] = c.baseValues[i]
		}
	}
	for i := range c.sources {
		source := &c.sources[i]
		source.done = false
		if source.arguments != nil {
			if err := source.arguments.EvalBorrowedRow(ctx, &c.rowInput, c.options.Parameters, source.values, expr.Direct); err != nil {
				return errors.Join(err, c.closeOuter())
			}
		}
		if source.spec.Strict {
			strictNull := false
			for _, argument := range source.values {
				strictNull = strictNull || argument.IsNull
			}
			if strictNull {
				source.done = true
				continue
			}
		}
		var err error
		source.iterator, err = source.spec.Rows.Open(c.call, source.values, builtin.RowSourceOptions{
			MaxInputBytes: uint64(c.options.Query.Usage().Work.Limit), MaxRowBytes: c.options.BatchBytes,
			MaxMemoryBytes: uint64(c.options.Query.Usage().Work.Limit), Allocator: c.options.Query.Allocator(runtime.WorkMemory),
			Temporal: c.options.Temporal, Settings: c.options.Settings, TextSearch: c.options.TextSearch,
			Relations: c.options.Relations, Native: c.options.Native, SessionIntrospection: c.options.SessionIntrospection, CatalogMetadata: c.options.CatalogMetadata,
		})
		if err != nil {
			return errors.Join(err, c.closeOuter())
		}
		if source.iterator == nil {
			return errors.Join(runtime.ErrInvalidConfig, c.closeOuter())
		}
	}
	return nil
}

func (c *cursor) prepare(ctx context.Context) (bool, error) {
	if c.pending {
		return true, nil
	}
	produced := false
	for i := range c.sources {
		source := &c.sources[i]
		if source.done {
			c.row[source.spec.Output] = model.NullVal(source.spec.Result.Type.Storage)
			continue
		}
		values, ok, err := source.iterator.Next(ctx)
		if err != nil {
			return false, err
		}
		if !ok {
			source.done = true
			c.row[source.spec.Output] = model.NullVal(source.spec.Result.Type.Storage)
			continue
		}
		produced = true
		if source.spec.Result.Type.Storage == model.TypeComposite {
			if len(values) == 1 && values[0].Type == model.TypeComposite {
				c.row[source.spec.Output] = values[0]
			} else {
				if len(values) != len(source.spec.Result.Children) {
					return false, columnar.ErrValueLayout
				}
				c.row[source.spec.Output] = model.Value{Type: model.TypeComposite, ArrayVal: values}
			}
		} else {
			if len(values) != 1 || values[0].Type != source.spec.Result.Type.Storage {
				return false, columnar.ErrValueLayout
			}
			c.row[source.spec.Output] = values[0]
		}
	}
	if !produced {
		return false, c.closeOuter()
	}
	c.pending = true
	return true, nil
}

func (c *cursor) closeOuter() error {
	var err error
	for i := range c.sources {
		if c.sources[i].iterator != nil {
			err = errors.Join(err, c.sources[i].iterator.Close())
			c.sources[i].iterator = nil
		}
		c.sources[i].done = false
	}
	clear(c.row)
	c.pending = false
	c.inputRow++
	return err
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out == nil || out.MaxRows < 0 || out == c.options.Captures {
		return errors.Join(columnar.ErrInvalidBatch, c.Close())
	}
	out.Reset()
	if c.closed {
		return io.EOF
	}
	defer func() {
		if err != nil {
			out.Reset()
			if cause := c.check(ctx); cause != nil {
				err = errors.Join(err, cause)
			}
			if !errors.Is(err, columnar.ErrBatchBudget) || c.builder.Rows() == 0 {
				if cleanup := c.Close(); cleanup != nil {
					err = errors.Join(err, cleanup)
				}
			}
		}
	}()
	if err := c.check(ctx); err != nil {
		return err
	}
	c.builder.Reset()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for c.builder.Rows() < limit {
		available, err := c.nextInputRow(ctx)
		if err != nil {
			return err
		}
		if !available {
			break
		}
		if c.sources[0].iterator == nil && !c.sources[0].done && !c.pending {
			if err := c.openOuter(ctx); err != nil {
				return err
			}
		}
		ready, err := c.prepare(ctx)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		if err := c.builder.Append(ctx, c.row); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() != 0 {
				break
			}
			return err
		}
		c.pending = false
	}
	if c.builder.Rows() == 0 {
		return io.EOF
	}
	return c.builder.Publish(out)
}

func (c *cursor) Close() error {
	if c == nil || c.closed {
		return c.closeErr
	}
	c.closed = true
	c.cancel(nil)
	if c.stopQuery != nil {
		c.stopQuery()
		c.stopQuery = nil
	}
	c.closeErr = errors.Join(c.closeErr, c.closeOuter())
	if c.options.Input != nil {
		c.closeErr = errors.Join(c.closeErr, c.options.Input.Close())
		c.options.Input = nil
	}
	if c.passthrough != nil {
		c.closeErr = errors.Join(c.closeErr, c.passthrough.Close())
		c.passthrough = nil
	}
	for i := range c.sources {
		if c.sources[i].arguments != nil {
			c.closeErr = errors.Join(c.closeErr, c.sources[i].arguments.Close())
			c.sources[i].arguments = nil
		}
		clear(c.sources[i].values)
		c.sources[i].values = nil
	}
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	c.input.Reset()
	c.rowInput = columnar.Batch{}
	clear(c.baseValues)
	clear(c.row)
	c.baseValues, c.row, c.sources = nil, nil, nil
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}
