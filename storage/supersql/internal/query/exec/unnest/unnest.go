// Package unnest streams array elements, multirange components, and function rows.
package unnest

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

var errRecordLayout = &model.Error{Code: "42804", Message: "function return row and query-specified return row do not match"}

type Options struct {
	Arguments *expr.Program
	Element   model.TypeLayout
	// Multirange is the declared input identity; zero selects native array unnest.
	Multirange           model.TypeID
	Rows                 builtin.RowSourcePlan
	Output               []model.TypeLayout
	Ordinality           bool
	Parameters           []expr.ParameterValue
	Captures             *columnar.Batch
	Subqueries           expr.SubqueryRuntime
	Sequences            model.SequenceRuntime
	Relations            model.RelationRuntime
	Temporal             model.TemporalRuntime
	Settings             model.SettingsRuntime
	TextSearch           model.TextSearchRuntime
	Advisory             model.AdvisoryLockRuntime
	Native               model.NativeScalarRuntime
	SessionIntrospection model.SessionIntrospection
	CatalogMetadata      model.CatalogMetadataIntrospection
	Query                *runtime.Query
	BatchRows            int
	BatchBytes           uint64
}

type cursor struct {
	options      Options
	life         context.Context
	arguments    *expr.State
	builder      *columnar.Builder
	claim        columnar.Allocation
	elements     []model.Value
	multirange   builtin.MultirangeIterator
	source       model.Value
	rows         builtin.RowSourceIterator
	rowArguments []model.Value
	rowReady     bool
	row          []model.Value
	index        int
	dataColumns  int
	initialized  bool
	done         bool
	closed       bool
	eof          bool
	closeErr     error
}

// New borrows immutable layouts, Arguments, Parameters, Captures and Query until
// Close. ctx governs the cursor lifetime; Next contexts govern only that call.
// Next and Close must be serialized. Output payloads last until Next or Close.
// A row source may declare no arguments, in which case Arguments is nil and the
// provider opens with an empty argument row.
func New(ctx context.Context, options Options) (_ storage.BatchCursor, err error) {
	if options.Query == nil || options.BatchRows <= 0 || options.BatchBytes == 0 || options.BatchBytes > uint64(math.MaxInt) {
		return nil, runtime.ErrInvalidConfig
	}
	if options.Rows == nil && options.Arguments == nil {
		return nil, runtime.ErrInvalidConfig
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, context.Cause(ctx), context.Cause(options.Query.Context()))
		}
	}()
	if err := errors.Join(context.Cause(ctx), context.Cause(options.Query.Context())); err != nil {
		return nil, err
	}
	call, cancel := context.WithCancelCause(ctx)
	query := options.Query.Context()
	stopQuery := context.AfterFunc(query, func() { cancel(context.Cause(query)) })
	defer func() { stopQuery(); cancel(nil) }()
	if options.Rows == nil {
		if options.Arguments.OutputCount() != 1 {
			return nil, expr.ErrInvalid
		}
		array, ok := options.Arguments.OutputType(0)
		if !ok {
			return nil, columnar.ErrValueLayout
		}
		if options.Multirange != 0 {
			family, known := builtin.LookupRangeFamily(options.Multirange)
			validInput := known && options.Multirange == family.Multirange && array.ID == family.Multirange && array.Storage == model.TypeBytes
			validOutput := options.Element.Type.ID == family.Range && options.Element.Type.Storage == model.TypeBytes && len(options.Element.Children) == 0
			if !validInput || !validOutput || !options.Arguments.OutputLayoutMatches(0, model.TypeLayout{Type: array}) {
				return nil, columnar.ErrValueLayout
			}
		} else {
			children := [1]model.TypeLayout{options.Element}
			if array.Storage != model.TypeArray || !options.Arguments.OutputLayoutMatches(0, model.TypeLayout{Type: array, Children: children[:]}) {
				return nil, columnar.ErrValueLayout
			}
		}
	} else if options.Multirange != 0 {
		return nil, columnar.ErrValueLayout
	}
	columns := len(options.Output)
	if options.Ordinality {
		columns--
		if columns < 0 || options.Output[columns].Type != (model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}) || len(options.Output[columns].Children) != 0 {
			return nil, columnar.ErrValueLayout
		}
	}
	if options.Element.Type.Storage != model.TypeComposite && columns != 1 {
		return nil, columnar.ErrValueLayout
	}
	base, err := metadata(call, options)
	if err != nil {
		return nil, err
	}
	if options.Element.Type.Storage != model.TypeComposite {
		if err := compatible(call, options.Element, options.Output[0]); err != nil {
			if errors.Is(err, errRecordLayout) {
				return nil, columnar.ErrValueLayout
			}
			return nil, err
		}
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(base)
	if err != nil {
		return nil, err
	}
	c := &cursor{options: options, life: ctx, claim: claim, dataColumns: columns, row: make([]model.Value, len(options.Output))}
	if options.Arguments != nil {
		c.rowArguments = make([]model.Value, options.Arguments.OutputCount())
		c.arguments, err = expr.NewState(options.Arguments, expr.StateOptions{
			Captures:        options.Captures,
			Subqueries:      options.Subqueries,
			Sequences:       options.Sequences,
			Relations:       options.Relations,
			Temporal:        options.Temporal,
			Settings:        options.Settings,
			TextSearch:      options.TextSearch,
			Advisory:        options.Advisory,
			Native:          options.Native,
			WorkAllocator:   options.Query.Allocator(runtime.WorkMemory),
			OutputAllocator: options.Query.Allocator(runtime.WorkMemory),
		})
	}
	if err == nil {
		c.builder, err = columnar.NewBuilder(options.Output, columnar.BuilderOptions{
			MaxRows: options.BatchRows, MaxBytes: options.BatchBytes,
			Allocator: options.Query.Allocator(runtime.OutputMemory),
		})
	}
	if err == nil {
		err = c.check(ctx)
	}
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func metadata(ctx context.Context, options Options) (uint64, error) {
	limit := uint64(options.Query.Usage().Work.Limit)
	base := uint64(unsafe.Sizeof(cursor{})) + uint64(unsafe.Sizeof(expr.State{})) + uint64(unsafe.Sizeof(columnar.Builder{}))
	unit := uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(unsafe.Sizeof(columnar.Vector{}))
	if base > limit || uint64(len(options.Output)) > (limit-base)/unit {
		return 0, runtime.ErrBudget
	}
	base += uint64(len(options.Output)) * unit
	if options.Rows != nil {
		count := uint64(0)
		if options.Arguments != nil {
			count = uint64(options.Arguments.OutputCount())
		}
		if count > (limit-base)/uint64(unsafe.Sizeof(model.Value{})) {
			return 0, runtime.ErrBudget
		}
		base += count * uint64(unsafe.Sizeof(model.Value{}))
	}
	remaining := limit - base
	size, err := model.TypeLayoutBytes(ctx, options.Element, remaining)
	if err != nil {
		return 0, err
	}
	remaining -= size
	for _, layout := range options.Output {
		size, err = model.TypeLayoutBytes(ctx, layout, remaining)
		if err != nil {
			return 0, err
		}
		remaining -= size
	}
	return limit - remaining, nil
}

// Collation does not change representation; an unspecified query typmod accepts
// any source typmod. The layouts have already passed bounded depth validation.
func compatible(ctx context.Context, actual, expected model.TypeLayout) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	wrongModifier := expected.Type.Modifier >= 0 && actual.Type.Modifier != expected.Type.Modifier
	if actual.Type.ID != expected.Type.ID || actual.Type.Storage != expected.Type.Storage || wrongModifier || len(actual.Children) != len(expected.Children) {
		return errRecordLayout
	}
	for i := range actual.Children {
		if err := compatible(ctx, actual.Children[i], expected.Children[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *cursor) releaseArguments() error {
	c.done = true
	clear(c.row)
	c.elements = nil
	c.multirange.Close()
	c.source = model.Value{}
	c.rowReady = false
	var err error
	if c.rows != nil {
		err = c.rows.Close()
		c.rows = nil
	}
	clear(c.rowArguments)
	c.rowArguments = nil
	c.options.Rows = nil
	if c.arguments != nil {
		err = errors.Join(err, c.arguments.Close())
		c.arguments = nil
	}
	c.options.Arguments = nil
	c.options.Parameters = nil
	c.options.Captures = nil
	c.options.Subqueries = nil
	c.options.Element = model.TypeLayout{}
	return err
}

func (c *cursor) initialize(ctx context.Context) error {
	if c.options.Rows != nil {
		if c.arguments != nil {
			input := columnar.Batch{Length: 1}
			if err := c.arguments.EvalBorrowedRow(ctx, &input, c.options.Parameters, c.rowArguments, expr.Direct); err != nil {
				return err
			}
		}
		var err error
		c.rows, err = c.options.Rows.Open(ctx, c.rowArguments, builtin.RowSourceOptions{
			MaxInputBytes:        uint64(c.options.Query.Usage().Work.Limit),
			MaxRowBytes:          c.options.BatchBytes,
			MaxMemoryBytes:       uint64(c.options.Query.Usage().Work.Limit),
			Allocator:            c.options.Query.Allocator(runtime.WorkMemory),
			Temporal:             c.options.Temporal,
			Settings:             c.options.Settings,
			TextSearch:           c.options.TextSearch,
			Relations:            c.options.Relations,
			Native:               c.options.Native,
			SessionIntrospection: c.options.SessionIntrospection,
			CatalogMetadata:      c.options.CatalogMetadata,
		})
		c.initialized = err == nil
		return err
	}
	input := columnar.Batch{Length: 1}
	var values [1]model.Value
	defer clear(values[:])
	if err := c.arguments.EvalBorrowedRow(ctx, &input, c.options.Parameters, values[:], expr.Direct); err != nil {
		return err
	}
	c.initialized = true
	if c.options.Multirange != 0 {
		c.source = values[0]
		var err error
		c.multirange, err = builtin.NewMultirangeIterator(ctx, c.options.Multirange, c.source)
		return err
	}
	if !values[0].IsNull {
		c.elements = values[0].ArrayVal
	}
	if len(c.elements) == 0 {
		return c.releaseArguments()
	}
	if c.options.Element.Type.Storage == model.TypeComposite {
		return c.validateRecordSchema(ctx)
	}
	return nil
}

func (c *cursor) validateRecordSchema(ctx context.Context) error {
	// PostgreSQL checks the first non-NULL record while materializing the SRF,
	// before a LIMIT or another FROM function can observe an earlier NULL row.
	for i := range c.elements {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if c.elements[i].IsNull {
			continue
		}
		if len(c.elements[i].ArrayVal) != c.dataColumns || len(c.options.Element.Children) != c.dataColumns {
			return errRecordLayout
		}
		for j := range c.dataColumns {
			if err := compatible(ctx, c.options.Element.Children[j], c.options.Output[j]); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func (c *cursor) prepareRow(ctx context.Context) (bool, error) {
	if c.rowReady {
		return true, nil
	}
	if c.rows != nil {
		row, ok, err := c.rows.Next(ctx)
		if err != nil || !ok {
			return false, err
		}
		if len(row) != c.dataColumns {
			return false, columnar.ErrValueLayout
		}
		copy(c.row, row)
	} else if c.options.Multirange != 0 {
		component, ok, err := c.multirange.NextBorrowed(ctx)
		if err != nil || !ok {
			return false, err
		}
		c.row[0] = model.BytesVal(component)
	} else {
		if c.index == len(c.elements) {
			return false, nil
		}
		element := c.elements[c.index]
		if c.options.Element.Type.Storage != model.TypeComposite {
			c.row[0] = element
		} else if err := c.prepareRecord(ctx, element); err != nil {
			return false, err
		}
	}
	if c.options.Ordinality {
		c.row[c.dataColumns] = model.Int64Val(int64(c.index) + 1)
	}
	c.rowReady = true
	return true, nil
}

func (c *cursor) prepareRecord(ctx context.Context, element model.Value) error {
	if element.IsNull {
		for i := range c.dataColumns {
			c.row[i] = model.NullVal(c.options.Output[i].Type.Storage)
		}
		return nil
	}
	if len(element.ArrayVal) != c.dataColumns {
		return errRecordLayout
	}
	copy(c.row, element.ArrayVal)
	return nil
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
	call, cancel := context.WithCancelCause(ctx)
	life, query := c.life, c.options.Query.Context()
	stopLife := context.AfterFunc(life, func() { cancel(context.Cause(life)) })
	stopQuery := context.AfterFunc(query, func() { cancel(context.Cause(query)) })
	defer func() { stopLife(); stopQuery(); cancel(nil) }()
	if !c.initialized {
		if err := c.initialize(call); err != nil {
			return err
		}
	}
	if c.done {
		return io.EOF
	}
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	c.builder.Reset()
	for c.builder.Rows() < limit {
		if err := c.check(ctx); err != nil {
			return err
		}
		ready, err := c.prepareRow(call)
		if err != nil {
			return err
		}
		if !ready {
			if err := c.releaseArguments(); err != nil {
				return err
			}
			break
		}
		err = c.builder.Append(call, c.row)
		if err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
				break
			}
			return err
		}
		clear(c.row)
		c.rowReady = false
		c.index++
	}
	if c.arguments != nil && c.options.Rows == nil && c.options.Multirange == 0 && c.index == len(c.elements) {
		if err := c.releaseArguments(); err != nil {
			return err
		}
	}
	if c.builder.Rows() == 0 {
		return io.EOF
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	return c.builder.Publish(out)
}

func (c *cursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	c.closeErr = c.releaseArguments()
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	c.row = nil
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	c.options = Options{}
	c.life = nil
	return c.closeErr
}
