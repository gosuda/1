package join

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
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// LookupFactory opens an indexed probe for a complete left row, borrowed only
// during Open. Every returned cursor transfers ownership, even alongside an error.
// The factory and its immutable collaborators remain borrowed through Close.
type LookupFactory interface {
	Open(context.Context, []model.Value) (storage.BatchCursor, error)
}

type lookupInput struct {
	batch      columnar.Batch
	row        []model.Value
	index      int
	ready, eof bool
}

type lookupCursor struct {
	cursor
	factory LookupFactory
	inputs  [2]lookupInput
}

type lookupWorkspace struct {
	base, fixed uint64
}

// CheckLookupSupport validates metadata and work bounds without taking ownership
// or reserving query memory. Hash and partition limits do not apply to lookup.
func CheckLookupSupport(ctx context.Context, options Options) error {
	_, err := checkLookup(ctx, options)
	return resourceCause(err)
}

func checkLookup(ctx context.Context, o Options) (lookupWorkspace, error) {
	var work lookupWorkspace
	missingRuntime := o.Query == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0
	invalidWork := o.MaxRecordBytes < 128 || o.WorkBytes == 0
	if missingRuntime || invalidBatch || invalidWork {
		return work, runtime.ErrInvalidConfig
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(o.Query.Context())); err != nil {
		return work, err
	}
	switch o.Kind {
	case ir.InnerJoin, ir.LeftJoin, ir.SemiJoin, ir.AntiJoin:
	default:
		return work, runtime.ErrInvalidConfig
	}
	exact := o.ExactLookup && o.Kind == ir.InnerJoin && len(o.Keys) == 0 && o.Residual == nil
	if o.MarkKind != 0 || (!exact && len(o.Keys) == 0) || (o.ExactLookup && !exact) || len(o.Output) != len(o.OutputLayouts) {
		return work, runtime.ErrInvalidConfig
	}
	for _, n := range []uint64{o.BatchBytes, o.MaxRecordBytes, o.WorkBytes, uint64(o.BatchRows)} {
		if n > uint64(math.MaxInt)/1024 {
			return work, runtime.ErrInvalidConfig
		}
	}
	semantic := cursor{options: o}
	for i, col := range o.Output {
		layout, err := semantic.layout(col)
		leftOnly := o.Kind == ir.SemiJoin || o.Kind == ir.AntiJoin
		invalidSide := leftOnly && col.Side != Left
		if err != nil || !model.TypeLayoutsEqual(layout, o.OutputLayouts[i]) || invalidSide {
			return work, runtime.ErrInvalidConfig
		}
	}
	if o.Residual != nil {
		outputs := o.Residual.OutputLayouts()
		if len(outputs) != 1 || outputs[0].Type.Storage != model.TypeBoolean {
			return work, runtime.ErrInvalidConfig
		}
		for _, col := range o.ResidualInputs {
			if _, err := semantic.layout(col); err != nil {
				return work, err
			}
		}
	} else if len(o.ResidualInputs) != 0 || len(o.Parameters) != 0 {
		return work, runtime.ErrInvalidConfig
	}
	var numeric uint64
	for _, key := range o.Keys {
		if key.Left < 0 || key.Left >= len(o.LeftLayouts) || key.Right < 0 || key.Right >= len(o.RightLayouts) || !model.TypeLayoutsEqual(o.LeftLayouts[key.Left], o.RightLayouts[key.Right]) {
			return work, runtime.ErrInvalidConfig
		}
		layout := o.LeftLayouts[key.Left]
		column := [1]storage.KeyColumn{{ColumnID: 1, Type: layout.Type, Children: layout.Children, NullsFirst: true}}
		if _, err := o.Encoder.EncodePrefix(ctx, column[:], nil, o.MaxRecordBytes); err != nil {
			return work, err
		}
		if layout.Type.Storage == model.TypeNumeric {
			numeric++
		}
	}
	work.base = uint64(unsafe.Sizeof(lookupCursor{})) + 8192 + uint64(len(o.LeftLayouts)+len(o.RightLayouts)+len(o.OutputLayouts)+len(o.ResidualInputs)+len(o.Keys)+8)*512
	// Two owned rows, encoded keys, and copy/encoding scratch coexist. Batches
	// and residual expression state are admitted by their producing allocators.
	work.fixed = work.base + o.MaxRecordBytes*20 + numeric*(1<<20)
	if work.fixed > o.WorkBytes {
		return work, resource("admit lookup join workspace", nil)
	}
	return work, nil
}

// NewLookup owns left on every outcome and at most one factory cursor at a time.
// Other collaborators are borrowed as in New. No right rows are cached between
// left rows; child requests are capped by remaining output demand.
func NewLookup(ctx context.Context, left storage.BatchCursor, factory LookupFactory, options Options) (storage.BatchCursor, error) {
	c := &lookupCursor{cursor: cursor{left: left, options: options}, factory: factory}
	if left == nil || factory == nil {
		return nil, errors.Join(runtime.ErrInvalidConfig, c.Close())
	}
	work, err := checkLookup(ctx, options)
	if err != nil {
		return nil, errors.Join(resourceCause(err), c.Close())
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(options.Query.Context(), c.cancel)
	c.claim, err = options.Query.Allocator(runtime.WorkMemory).Reserve(work.fixed)
	if err == nil {
		err = c.configureRows(ctx, work.base)
	}
	if err != nil {
		return nil, errors.Join(resourceCause(err), c.Close())
	}
	c.inputs[0].row = make([]model.Value, len(c.leftSchema))
	c.inputs[1].row = make([]model.Value, len(c.rightSchema))
	return c, nil
}

func (c *lookupCursor) closeProbe() error {
	input := &c.inputs[1]
	input.batch.Reset()
	clear(input.row)
	input.index, input.ready, input.eof = 0, false, true
	if c.right == nil {
		return nil
	}
	owner := c.right
	c.right = nil
	return owner.Close()
}

func (c *lookupCursor) load(ctx context.Context, side Side) error {
	input := &c.inputs[int(side)-1]
	if input.ready {
		return nil
	}
	if input.eof {
		return io.EOF
	}
	child, layouts := &c.left, c.options.LeftLayouts
	if side == Right {
		child, layouts = &c.right, c.options.RightLayouts
	}
	for input.index >= input.batch.Rows() {
		if err := c.check(ctx); err != nil {
			return err
		}
		input.batch.Reset()
		input.batch.MaxRows = c.leftDemand
		err := (*child).Next(ctx, &input.batch)
		if err == io.EOF {
			input.eof = true
			input.batch.Reset()
			owner := *child
			*child = nil
			if err := owner.Close(); err != nil {
				return err
			}
			return io.EOF
		}
		if err != nil {
			return err
		}
		// Native selections may address physical lanes beyond logical demand.
		if err := input.batch.Validate(max(input.batch.Length, c.leftDemand)); err != nil {
			return err
		}
		input.index = 0
		if input.batch.Rows() == 0 {
			continue
		}
		if input.batch.Rows() > c.leftDemand || len(input.batch.Columns) != len(layouts) {
			return columnar.ErrInvalidBatch
		}
		for i, layout := range layouts {
			if !input.batch.Columns[i].MatchesLayout(layout) {
				return columnar.ErrValueLayout
			}
		}
	}
	lane := input.batch.Lane(input.index)
	input.index++
	if err := c.prepareRow(ctx, &input.batch, lane, side, input.row); err != nil {
		return err
	}
	if err := storage.EncodeValue(ctx, io.Discard, model.Value{Type: model.TypeComposite, ArrayVal: input.row}, c.options.MaxRecordBytes-32); err != nil {
		return err
	}
	input.ready = true
	return nil
}

func (c *lookupCursor) finishLeft() error {
	left := &c.inputs[0]
	left.ready = false
	if !c.leftMatched && c.emitsUnmatchedLeft() {
		c.mapRow(c.output, c.options.Output, left.row, nil)
		return nil
	}
	return io.EOF
}

func (c *lookupCursor) next(ctx context.Context) error {
	left, right := &c.inputs[0], &c.inputs[1]
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		if !left.ready {
			if err := c.load(ctx, Left); err != nil {
				return err
			}
			c.leftMatched = false
			if left.row[1].IntVal == 0 {
				if c.finishLeft() == nil {
					return nil
				}
				continue
			}
			var err error
			c.right, err = c.factory.Open(ctx, left.row[3:])
			if err != nil {
				return errors.Join(err, c.closeProbe())
			}
			if c.right == nil {
				return runtime.ErrInvalidConfig
			}
			right.index, right.ready, right.eof = 0, false, false
		}
		err := c.load(ctx, Right)
		if err == io.EOF {
			if c.finishLeft() == nil {
				return nil
			}
			continue
		}
		if err != nil {
			return err
		}
		matched, err := c.matches(ctx, left.row, right.row)
		if err != nil {
			return err
		}
		right.ready = false
		if matched.IsNull || matched.IntVal == 0 {
			continue
		}
		c.leftMatched = true
		if c.options.Kind == ir.SemiJoin || c.options.Kind == ir.AntiJoin {
			if err := c.closeProbe(); err != nil {
				return err
			}
			left.ready = false
			if c.options.Kind == ir.AntiJoin {
				continue
			}
			c.mapRow(c.output, c.options.Output, left.row, nil)
			return nil
		}
		c.mapRow(c.output, c.options.Output, left.row, right.row)
		return nil
	}
}

func (c *lookupCursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if c.closed {
		return runtime.ErrClosed
	}
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, c.Close())
	}
	out.Reset()
	defer func() {
		if err != nil && err != io.EOF {
			err = errors.Join(resourceCause(err), c.Close())
		}
	}()
	if err := c.check(ctx); err != nil {
		return err
	}
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	c.builder.Reset()
	for c.builder.Rows() < limit {
		c.leftDemand = limit - c.builder.Rows()
		if !c.pending {
			if err = c.next(callCtx); err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			c.pending = true
		}
		err = c.builder.Append(callCtx, c.output)
		if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
			break
		}
		if err != nil {
			return err
		}
		c.pending = false
		clear(c.output)
	}
	if c.builder.Rows() == 0 {
		if err := c.Close(); err != nil {
			return err
		}
		return io.EOF
	}
	return c.builder.Publish(out)
}

func (c *lookupCursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closeErr = errors.Join(c.closeErr, c.closeProbe())
	for i := range c.inputs {
		c.inputs[i].batch.Reset()
		c.inputs[i].row = nil
	}
	c.factory = nil
	return c.cursor.Close()
}
