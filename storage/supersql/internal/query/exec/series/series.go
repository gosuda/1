// Package series streams integer, numeric and timestamp series and array subscripts.
package series

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

var (
	errZeroStep           = &model.Error{Code: "22023", Message: "step size cannot equal zero"}
	errOrdinalityOverflow = &model.Error{Code: "22003", Message: "generate_series ordinality is out of range"}
)

type Options struct {
	Arguments  *expr.Program
	Layout     model.TypeLayout
	Ordinality bool
	Parameters []expr.ParameterValue
	Captures   *columnar.Batch
	Subqueries expr.SubqueryRuntime
	Sequences  model.SequenceRuntime
	Relations  model.RelationRuntime
	Temporal   model.TemporalRuntime
	Settings   model.SettingsRuntime
	TextSearch model.TextSearchRuntime
	Advisory  model.AdvisoryLockRuntime
	Native     model.NativeScalarRuntime
	Query      *runtime.Query
	BatchRows  int
	BatchBytes uint64
	WorkBytes  uint64 // Owned numeric or timestamptz state; zero admits no owned state.
	TimeZone   builtin.TimeZoneContext
}

type sourceKind uint8

const (
	integerSeries sourceKind = iota + 1
	arraySubscripts
	timestampSeries
	numericSeries
	timestamptzSeries
)

type ownedSequence interface {
	Peek(context.Context) (model.Value, bool, error)
	Advance() error
	Close() error
}

type cursor struct {
	options                             Options
	kind                                sourceKind
	life                                context.Context
	arguments                           *expr.State
	argumentColumns                     [4]columnar.Vector
	builder                             *columnar.Builder
	claim                               columnar.Allocation
	row                                 [2]model.Value
	current, stop, step, ordinal        int64
	minimum, maximum                    int64
	timestamp                           builtin.TimestampSeries
	sequence                            ownedSequence
	initialized, exhausted, closed, eof bool
	closeErr                            error
}

// New borrows Arguments, Parameters and Captures until the first Next finishes,
// and Query until Close. ctx governs the cursor lifetime; Next contexts govern
// only their invocation. Next and Close must be serialized.
func New(ctx context.Context, options Options) (storage.BatchCursor, error) {
	if options.Layout.Type.ID == 1114 {
		return newCursor(ctx, options, timestampSeries)
	}
	if options.Layout.Type.ID == 1184 {
		return newCursor(ctx, options, timestamptzSeries)
	}
	if options.Layout.Type.ID == 1700 {
		return newCursor(ctx, options, numericSeries)
	}
	return newCursor(ctx, options, integerSeries)
}

// NewSubscripts borrows arguments and owns resources for the same lifetimes as New.
// Arguments must return an array, an int4 dimension, and an optional bool direction.
func NewSubscripts(ctx context.Context, options Options) (storage.BatchCursor, error) {
	return newCursor(ctx, options, arraySubscripts)
}

func newCursor(ctx context.Context, options Options, kind sourceKind) (storage.BatchCursor, error) {
	if options.Query == nil || options.Arguments == nil || options.BatchRows <= 0 || options.BatchBytes == 0 || options.BatchBytes > uint64(math.MaxInt) {
		return nil, runtime.ErrInvalidConfig
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(options.Query.Context())); err != nil {
		return nil, err
	}
	supportedWidth := options.Layout.Type.ID == 23 || options.Layout.Type.ID == 20
	supportedStorage := options.Layout.Type.Storage == model.TypeInt64
	if kind == timestampSeries {
		supportedWidth = options.Layout.Type.ID == 1114
		supportedStorage = options.Layout.Type.Storage == model.TypeTimestamp
	}
	if kind == timestamptzSeries {
		supportedWidth = options.Layout.Type == (model.Type{ID: 1184, Modifier: -1, Storage: model.TypeTimestamp})
		supportedStorage = options.Layout.Type.Storage == model.TypeTimestamp
	}
	if kind == numericSeries {
		supportedWidth = options.Layout.Type == (model.Type{ID: 1700, Modifier: -1, Storage: model.TypeNumeric})
		supportedStorage = options.Layout.Type.Storage == model.TypeNumeric
	}
	if !supportedStorage || !supportedWidth || len(options.Layout.Children) != 0 {
		return nil, columnar.ErrValueLayout
	}
	count := options.Arguments.OutputCount()
	if count != 2 && count != 3 && !(kind == timestamptzSeries && count == 4) {
		return nil, expr.ErrInvalid
	}
	switch kind {
	case timestampSeries, timestamptzSeries:
		if count < 3 || (kind == timestampSeries && count != 3) {
			return nil, expr.ErrInvalid
		}
		for i := range 2 {
			typ, ok := options.Arguments.OutputType(i)
			if !ok || typ != options.Layout.Type {
				return nil, columnar.ErrValueLayout
			}
		}
		step, _ := options.Arguments.OutputType(2)
		if step.ID != 1186 || step.Storage != model.TypeInterval {
			return nil, columnar.ErrValueLayout
		}
		if count == 4 {
			zone, _ := options.Arguments.OutputType(3)
			if zone.ID != 25 || zone.Modifier != -1 || zone.Storage != model.TypeString {
				return nil, columnar.ErrValueLayout
			}
		}
	case numericSeries:
		for i := range count {
			if !options.Arguments.OutputLayoutMatches(i, options.Layout) {
				return nil, columnar.ErrValueLayout
			}
		}
	case integerSeries:
		for i := range count {
			typ, ok := options.Arguments.OutputType(i)
			if !ok || typ != options.Layout.Type {
				return nil, columnar.ErrValueLayout
			}
		}
	case arraySubscripts:
		int4 := model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}
		array, _ := options.Arguments.OutputType(0)
		dimension, _ := options.Arguments.OutputType(1)
		if options.Layout.Type != int4 || array.Storage != model.TypeArray || dimension != int4 {
			return nil, columnar.ErrValueLayout
		}
		if count == 3 {
			reverse, _ := options.Arguments.OutputType(2)
			if reverse != (model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}) {
				return nil, columnar.ErrValueLayout
			}
		}
	default:
		return nil, runtime.ErrInvalidConfig
	}
	base := uint64(unsafe.Sizeof(cursor{})) + uint64(unsafe.Sizeof(expr.State{})) + uint64(unsafe.Sizeof(columnar.Builder{}))
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(base)
	if err != nil {
		return nil, err
	}
	options.Layout.Children = nil
	c := &cursor{options: options, kind: kind, life: ctx, claim: claim, minimum: math.MinInt64, maximum: math.MaxInt64}
	if err := c.configure(); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func (c *cursor) configure() error {
	var err error
	c.arguments, err = expr.NewState(c.options.Arguments, expr.StateOptions{
		Captures:        c.options.Captures,
		Subqueries:      c.options.Subqueries,
		Sequences:       c.options.Sequences,
		Relations:       c.options.Relations,
		Temporal:        c.options.Temporal,
		Settings:        c.options.Settings,
		TextSearch:      c.options.TextSearch,
		Advisory:        c.options.Advisory,
		Native:          c.options.Native,
		WorkAllocator:   c.options.Query.Allocator(runtime.WorkMemory),
		OutputAllocator: c.options.Query.Allocator(runtime.WorkMemory),
	})
	if err != nil {
		return err
	}
	layouts := [2]model.TypeLayout{c.options.Layout, {Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}}
	columns := 1
	if c.options.Ordinality {
		columns++
	}
	c.builder, err = columnar.NewBuilder(layouts[:columns], columnar.BuilderOptions{
		MaxRows:   c.options.BatchRows,
		MaxBytes:  c.options.BatchBytes,
		Allocator: c.options.Query.Allocator(runtime.OutputMemory),
	})
	if c.options.Layout.Type.ID == 23 {
		c.minimum, c.maximum = math.MinInt32, math.MaxInt32
	}
	return err
}

func (c *cursor) releaseArguments() error {
	var err error
	if c.arguments != nil {
		err = c.arguments.Close()
		c.arguments = nil
	}
	clear(c.argumentColumns[:])
	c.options.Arguments = nil
	c.options.Parameters = nil
	c.options.Captures = nil
	c.options.Subqueries = nil
	c.options.TimeZone = builtin.TimeZoneContext{}
	return err
}

func (c *cursor) initialize(ctx context.Context) (err error) {
	defer func() { err = errors.Join(err, c.releaseArguments()) }()
	call, cancel := context.WithCancelCause(ctx)
	life := c.life
	stopLife := context.AfterFunc(life, func() { cancel(context.Cause(life)) })
	queryContext := c.options.Query.Context()
	stopQuery := context.AfterFunc(queryContext, func() { cancel(context.Cause(queryContext)) })
	defer func() { stopLife(); stopQuery(); cancel(nil) }()
	input := columnar.Batch{Length: 1}
	if c.kind == arraySubscripts {
		return c.initializeSubscripts(call, &input)
	}
	out := columnar.Batch{Columns: c.argumentColumns[:0], MaxRows: 1}
	if err := c.arguments.Eval(call, &input, c.options.Parameters, &out, expr.Direct); err != nil {
		return err
	}
	c.initialized = true
	for i := range out.Columns {
		if out.Columns[i].IsNull(0) {
			c.exhausted = true
			return nil
		}
	}
	if c.kind == timestampSeries {
		c.timestamp, err = builtin.NewTimestampSeries(out.Columns[0].Int64[0], out.Columns[1].Int64[0], out.Columns[2].Intervals[0])
		c.ordinal = 1
		return err
	}
	if c.kind == timestamptzSeries {
		var explicitZone *string
		if len(out.Columns) == 4 {
			vector := &out.Columns[3]
			payload := vector.Ranges[vector.ValueIndex(0)]
			data := vector.Data[int(payload.Offset) : int(payload.Offset)+int(payload.Length)]
			name := unsafe.String(unsafe.SliceData(data), len(data))
			explicitZone = &name
		}
		c.sequence, err = builtin.NewTimestamptzSeries(call, out.Columns[0].Int64[0], out.Columns[1].Int64[0], out.Columns[2].Intervals[0], builtin.TimestamptzSeriesOptions{
			TimeZone: c.options.TimeZone, ExplicitZone: explicitZone,
			MemoryBytes: c.options.WorkBytes,
			Allocator:   c.options.Query.Allocator(runtime.WorkMemory),
		})
		c.ordinal = 1
		return err
	}
	if c.kind == numericSeries {
		values := [3]string{"", "", "1"}
		for i := range out.Columns {
			vector := &out.Columns[i]
			payload := vector.Ranges[vector.ValueIndex(0)]
			data := vector.Data[int(payload.Offset) : int(payload.Offset)+int(payload.Length)]
			// Construction parses these borrowed bytes before releaseArguments.
			values[i] = unsafe.String(unsafe.SliceData(data), len(data))
		}
		c.sequence, err = builtin.NewNumericSeries(call, values[0], values[1], values[2], builtin.NumericSeriesOptions{
			MemoryBytes: c.options.WorkBytes,
			ValueBytes:  c.options.BatchBytes,
			Allocator:   c.options.Query.Allocator(runtime.WorkMemory),
		})
		c.ordinal = 1
		return err
	}
	c.current, c.stop, c.step = out.Columns[0].Int64[0], out.Columns[1].Int64[0], 1
	if len(out.Columns) == 3 {
		c.step = out.Columns[2].Int64[0]
	}
	if c.step == 0 {
		return errZeroStep
	}
	c.ordinal = 1
	c.exhausted = (c.step > 0 && c.current > c.stop) || (c.step < 0 && c.current < c.stop)
	return nil
}

func (c *cursor) initializeSubscripts(ctx context.Context, input *columnar.Batch) error {
	var values [3]model.Value
	defer clear(values[:])
	arguments := values[:c.options.Arguments.OutputCount()]
	if err := c.arguments.EvalBorrowedRow(ctx, input, c.options.Parameters, arguments, expr.Direct); err != nil {
		return err
	}
	c.initialized = true
	for _, argument := range arguments {
		if argument.IsNull {
			c.exhausted = true
			return nil
		}
	}
	dimensions, dimension := arguments[0].Dimensions, arguments[1].IntVal
	if dimension < 1 || dimension > int64(len(dimensions)) {
		c.exhausted = true
		return nil
	}
	bounds := dimensions[dimension-1]
	c.current, c.stop, c.step = int64(bounds.LowerBound), int64(bounds.LowerBound)+int64(bounds.Length)-1, 1
	if len(arguments) == 3 && arguments[2].IntVal != 0 {
		c.current, c.stop, c.step = c.stop, c.current, -1
	}
	c.ordinal = 1
	return nil
}

func (c *cursor) advance() {
	overflowsUpper := c.step > 0 && c.current > c.maximum-c.step
	overflowsLower := c.step < 0 && c.current < c.minimum-c.step
	if overflowsUpper || overflowsLower {
		c.exhausted = true
		return
	}
	c.current += c.step
	c.exhausted = (c.step > 0 && c.current > c.stop) || (c.step < 0 && c.current < c.stop)
	c.advanceOrdinality()
}

func (c *cursor) advanceOrdinality() {
	if c.options.Ordinality {
		// Zero defers overflow until another output row is actually requested.
		if c.ordinal == math.MaxInt64 {
			c.ordinal = 0
		} else {
			c.ordinal++
		}
	}
}

func (c *cursor) check(ctx context.Context) error {
	return errors.Join(context.Cause(ctx), context.Cause(c.life), context.Cause(c.options.Query.Context()))
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out == nil || out.MaxRows < 0 || out == c.options.Captures {
		return errors.Join(columnar.ErrInvalidBatch, c.Close())
	}
	out.Reset()
	if c.closed {
		if c.eof {
			return io.EOF
		}
		return runtime.ErrClosed
	}
	defer func() {
		if err != nil {
			out.Reset()
			if cause := c.check(ctx); cause != nil {
				err = errors.Join(err, cause)
			}
			c.eof = err == io.EOF
			if cleanup := c.Close(); cleanup != nil {
				err = errors.Join(err, cleanup)
			}
		}
	}()
	if err := c.check(ctx); err != nil {
		return err
	}
	if !c.initialized {
		if err := c.initialize(ctx); err != nil {
			return err
		}
	}
	if c.exhausted {
		return io.EOF
	}
	sequenceContext := ctx
	if c.sequence != nil {
		call, cancel := context.WithCancelCause(ctx)
		life, queryContext := c.life, c.options.Query.Context()
		stopLife := context.AfterFunc(life, func() { cancel(context.Cause(life)) })
		stopQuery := context.AfterFunc(queryContext, func() { cancel(context.Cause(queryContext)) })
		defer func() { stopLife(); stopQuery(); cancel(nil) }()
		sequenceContext = call
	}
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	columns := 1
	if c.options.Ordinality {
		columns++
	}
	c.builder.Reset()
	for c.builder.Rows() < limit && !c.exhausted {
		if err := c.check(ctx); err != nil {
			return err
		}
		if c.options.Ordinality && c.ordinal == 0 {
			return errOrdinalityOverflow
		}
		c.row[0] = model.Int64Val(c.current)
		var nextTimestamp builtin.TimestampSeries
		if c.kind == timestampSeries {
			nextTimestamp = c.timestamp
			value, present, err := nextTimestamp.Next(ctx)
			if err != nil {
				return err
			}
			if !present {
				c.exhausted = true
				break
			}
			c.row[0] = value
		}
		if c.sequence != nil {
			c.row[0] = model.Value{}
			value, present, err := c.sequence.Peek(sequenceContext)
			if err != nil {
				return err
			}
			if !present {
				c.exhausted = true
				break
			}
			c.row[0] = value
		}
		if c.options.Ordinality {
			c.row[1] = model.Int64Val(c.ordinal)
		}
		if err := c.builder.Append(ctx, c.row[:columns]); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
				break
			}
			return err
		}
		if c.kind == timestampSeries {
			c.timestamp = nextTimestamp
			c.advanceOrdinality()
		} else if c.sequence != nil {
			c.row[0] = model.Value{}
			if err := c.sequence.Advance(); err != nil {
				return err
			}
			c.advanceOrdinality()
		} else {
			c.advance()
		}
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	if c.exhausted && c.builder.Rows() == 0 {
		return io.EOF
	}
	return c.builder.Publish(out)
}

func (c *cursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	c.closeErr = c.releaseArguments()
	clear(c.row[:])
	if c.sequence != nil {
		c.closeErr = errors.Join(c.closeErr, c.sequence.Close())
		c.sequence = nil
	}
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	c.options = Options{}
	c.life = nil
	return c.closeErr
}
