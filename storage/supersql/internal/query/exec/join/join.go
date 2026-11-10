// Package join implements bounded joins over typed, rewindable partitions.
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
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type Side uint8

const (
	Left Side = iota + 1
	Right
	Mark
)

// Column maps a child column, or Mark with Index zero, into the output.
type Column struct {
	Side  Side
	Index int
}
type Key struct {
	Left, Right int
	NullEqual   bool
}

// New owns both children on every outcome. Query, Encoder, Residual and
// Parameters are borrowed through Close; callers serialize Next and Close.
// MemoryBytes may be zero to force disk partitions. Other limits are positive.
// PartitionBytes limits each input partition, not the joined result. HashBytes
// bounds the fixed index; oversized/skewed partitions use rewindable replay.
// Replay never repartitions, so its depth is one, within every valid MaxDepth.
// Provision 2*MaxPartitions+1 query files and two query readers.
// WorkBytes covers join state and spools; input/output batches and residual
// expression state have their own limits and share the query admission ledger.
// Marks demand left rows incrementally and retain the complete right witness set.
// Every pair for a demanded left row is evaluated, even after a decisive mark,
// so residual effects do not depend on spilling or early truth discovery.
// An empty left input closes the right child without consuming it.
type Options struct {
	Kind                                     ir.JoinKind
	MarkKind                                 ir.SubqueryKind
	LeftLayouts, RightLayouts, OutputLayouts []model.TypeLayout
	Output                                   []Column
	Keys                                     []Key
	ExactLookup                              bool
	Residual                                 *expr.Program
	ResidualInputs                           []Column
	Parameters                               []expr.ParameterValue
	// Captures is borrowed unchanged through Close, like Residual and Parameters.
	Captures                                                                      *columnar.Batch
	Subqueries                                                                    expr.SubqueryRuntime
	Sequences                                                                     model.SequenceRuntime
	Relations                                                                     model.RelationRuntime
	Temporal                                                                      model.TemporalRuntime
	Settings                                                                      model.SettingsRuntime
	TextSearch                                                                    model.TextSearchRuntime
	Advisory                                                                      model.AdvisoryLockRuntime
	Native                                                                        model.NativeScalarRuntime
	Encoder                                                                       storage.KeyEncoder
	Query                                                                         *runtime.Query
	BatchRows                                                                     int
	BatchBytes, MaxRecordBytes, MemoryBytes, HashBytes, WorkBytes, PartitionBytes uint64
	MaxPartitions, MaxDepth                                                       int
}

type partition struct{ left, right *rowstore.Spool }
type entry struct {
	position rowstore.Position
	next     int
	matched  bool
}
type cursor struct {
	options                                                                 Options
	left, right                                                             storage.BatchCursor
	ctx                                                                     context.Context
	cancel                                                                  context.CancelFunc
	stopQuery                                                               func() bool
	claim, hashClaim                                                        columnar.Allocation
	metadata, hashBytes                                                     uint64
	partitions                                                              []partition
	partitioned                                                             *partitionedState
	part                                                                    int
	leftSchema, rightSchema                                                 []model.TypeLayout
	keyLayout                                                               []storage.KeyColumn
	keyValues, row, output, predicateRow                                    []model.Value
	builder, predicateBuilder                                               *columnar.Builder
	predicate                                                               *expr.State
	leftReader, rightReader                                                 *rowstore.Reader
	replacement                                                             *rowstore.Spool
	entries                                                                 []entry
	buckets                                                                 []int
	leftRow                                                                 []model.Value
	candidate                                                               int
	unmatched                                                               int
	mark                                                                    model.Value
	hashMode, leftMatched, scanning, draining, initialized, pending, closed bool
	markBatch                                                               columnar.Batch
	markIndex, leftDemand                                                   int
	markRow                                                                 []model.Value
	markReady, markEOF                                                      bool
	closeErr                                                                error
}

func resource(message string, cause error) error {
	return &model.Error{Code: "53200", Message: message, Cause: errors.Join(runtime.ErrBudget, cause)}
}

func resourceCause(err error) error {
	if errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, model.ErrValueBudget) || errors.Is(err, storage.ErrDecodeBudget) || errors.Is(err, io.ErrShortWrite) {
		return resource("join row exceeds resource limit", err)
	}
	return err
}

func New(ctx context.Context, left, right storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	c := &cursor{left: left, right: right, options: options}
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(resourceCause(err), c.Close())
	}
	return c, nil
}

func (c *cursor) layout(col Column) (model.TypeLayout, error) {
	var layouts []model.TypeLayout
	switch col.Side {
	case Left:
		layouts = c.options.LeftLayouts
	case Right:
		layouts = c.options.RightLayouts
	default:
		return model.TypeLayout{}, runtime.ErrInvalidConfig
	}
	if col.Index < 0 || col.Index >= len(layouts) {
		return model.TypeLayout{}, runtime.ErrInvalidConfig
	}
	return layouts[col.Index], nil
}
func (c *cursor) configure(ctx context.Context) error {
	o := &c.options
	missingInput := c.left == nil || c.right == nil || o.Query == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRecordBytes < 128
	invalidWork := o.HashBytes == 0 || o.WorkBytes == 0 || o.PartitionBytes == 0
	if missingInput || invalidBatch || invalidWork || o.MaxPartitions <= 0 || o.MaxDepth <= 0 {
		return runtime.ErrInvalidConfig
	}
	switch o.Kind {
	case ir.InnerJoin, ir.CrossJoin, ir.LeftJoin, ir.RightJoin, ir.FullJoin, ir.SemiJoin, ir.AntiJoin:
	case ir.MarkJoin:
		if o.MarkKind != ir.ExistsSubquery && o.MarkKind != ir.AnySubquery && o.MarkKind != ir.AllSubquery {
			return runtime.ErrInvalidConfig
		}
	default:
		return runtime.ErrInvalidConfig
	}
	if o.Kind != ir.MarkJoin && o.MarkKind != 0 {
		return runtime.ErrInvalidConfig
	}
	if o.Kind == ir.CrossJoin && (len(o.Keys) != 0 || o.Residual != nil) {
		return runtime.ErrInvalidConfig
	}
	for _, n := range []uint64{o.BatchBytes, o.MaxRecordBytes, o.MemoryBytes, o.HashBytes, o.WorkBytes, o.PartitionBytes, uint64(o.MaxPartitions), uint64(o.BatchRows)} {
		if n > uint64(math.MaxInt)/128 {
			return runtime.ErrInvalidConfig
		}
	}
	if len(o.Output) != len(o.OutputLayouts) {
		return runtime.ErrInvalidConfig
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(o.Query.Context(), c.cancel)
	if err := c.check(ctx); err != nil {
		return err
	}
	n := o.MaxPartitions
	if len(o.Keys) == 0 || o.Kind == ir.MarkJoin {
		n = 1
	}
	base := uint64(unsafe.Sizeof(cursor{})) + 4096 + uint64(len(o.LeftLayouts)+len(o.RightLayouts)+len(o.OutputLayouts)+len(o.ResidualInputs)+len(o.Keys)+8)*512 + uint64(n)*64
	// This bound includes simultaneous append/decode scratch, partition metadata,
	// input row copies, encoder output, and the replacement used by outer replay.
	c.metadata = base + 4096
	spoolWork := c.metadata + o.MaxRecordBytes*9 + 8192
	fixed := base + o.MaxRecordBytes*18
	if fixed > o.WorkBytes || uint64(2*n+3) > (o.WorkBytes-fixed)/spoolWork {
		return resource("admit join partition workspace", nil)
	}
	available := o.WorkBytes - fixed - uint64(2*n+3)*spoolWork
	if o.MemoryBytes > available {
		return resource("admit join memory partitions", nil)
	}
	c.hashBytes = min(o.HashBytes, available-o.MemoryBytes)
	var err error
	c.claim, err = o.Query.Allocator(runtime.WorkMemory).Reserve(fixed)
	if err != nil {
		return err
	}
	if err := c.configureRows(ctx, base); err != nil {
		return err
	}
	c.partitions = make([]partition, n)
	c.row = make([]model.Value, max(len(c.leftSchema), len(c.rightSchema)))
	if o.Kind == ir.MarkJoin {
		c.markRow = make([]model.Value, len(c.leftSchema))
	}
	return nil
}

func (c *cursor) configureSchema(ctx context.Context, base uint64) error {
	o := &c.options
	var err error
	remaining := base
	copyLayouts := func(src []model.TypeLayout) ([]model.TypeLayout, error) {
		dst := make([]model.TypeLayout, len(src))
		for i, l := range src {
			owned, used, e := model.CopyTypeLayout(ctx, l, remaining)
			if e != nil {
				return nil, e
			}
			remaining -= used
			dst[i] = owned
		}
		return dst, nil
	}
	if o.LeftLayouts, err = copyLayouts(o.LeftLayouts); err != nil {
		return err
	}
	if o.RightLayouts, err = copyLayouts(o.RightLayouts); err != nil {
		return err
	}
	if o.OutputLayouts, err = copyLayouts(o.OutputLayouts); err != nil {
		return err
	}
	o.Output = append([]Column(nil), o.Output...)
	o.Keys = append([]Key(nil), o.Keys...)
	o.ResidualInputs = append([]Column(nil), o.ResidualInputs...)
	for i, col := range o.Output {
		if col.Side == Mark {
			invalidMark := o.Kind != ir.MarkJoin || col.Index != 0
			layout := o.OutputLayouts[i]
			invalidType := layout.Type.ID != 16 || layout.Type.Storage != model.TypeBoolean || len(layout.Children) != 0
			if invalidMark || invalidType {
				return runtime.ErrInvalidConfig
			}
			continue
		}
		l, e := c.layout(col)
		if e != nil {
			return e
		}
		if !model.TypeLayoutsEqual(l, o.OutputLayouts[i]) {
			return runtime.ErrInvalidConfig
		}
		if (o.Kind == ir.SemiJoin || o.Kind == ir.AntiJoin) && col.Side != Left {
			return runtime.ErrInvalidConfig
		}
		if o.Kind == ir.MarkJoin && col.Side == Right {
			return runtime.ErrInvalidConfig
		}
	}
	c.keyLayout = make([]storage.KeyColumn, len(o.Keys))
	c.keyValues = make([]model.Value, len(o.Keys))
	for i, k := range o.Keys {
		if k.Left < 0 || k.Left >= len(o.LeftLayouts) || k.Right < 0 || k.Right >= len(o.RightLayouts) || !model.TypeLayoutsEqual(o.LeftLayouts[k.Left], o.RightLayouts[k.Right]) {
			return runtime.ErrInvalidConfig
		}
		c.keyLayout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: o.LeftLayouts[k.Left].Type, Children: o.LeftLayouts[k.Left].Children, NullsFirst: true}
	}
	if _, err = o.Encoder.EncodePrefix(ctx, c.keyLayout, nil, o.MaxRecordBytes); err != nil {
		return err
	}
	header := []model.TypeLayout{{Type: model.Type{ID: 17, Storage: model.TypeBytes}}, {Type: model.Type{ID: 16, Storage: model.TypeBoolean}}, {Type: model.Type{ID: 16, Storage: model.TypeBoolean}}}
	c.leftSchema = append(append([]model.TypeLayout(nil), header...), o.LeftLayouts...)
	c.rightSchema = append(append([]model.TypeLayout(nil), header...), o.RightLayouts...)
	return nil
}

func (c *cursor) residualLayouts() ([]model.TypeLayout, error) {
	o := &c.options
	if o.Residual == nil {
		if len(o.ResidualInputs) != 0 || len(o.Parameters) != 0 {
			return nil, runtime.ErrInvalidConfig
		}
		return nil, nil
	}
	outputs := o.Residual.OutputLayouts()
	if len(outputs) != 1 || outputs[0].Type.Storage != model.TypeBoolean {
		return nil, runtime.ErrInvalidConfig
	}
	layouts := make([]model.TypeLayout, len(o.ResidualInputs))
	for i, col := range o.ResidualInputs {
		layout, err := c.layout(col)
		if err != nil {
			return nil, err
		}
		layouts[i] = layout
	}
	return layouts, nil
}

func (c *cursor) configureRows(ctx context.Context, base uint64) error {
	if err := c.configureSchema(ctx, base); err != nil {
		return err
	}
	o := &c.options
	var err error
	c.output = make([]model.Value, len(o.Output))
	c.builder, err = columnar.NewBuilder(o.OutputLayouts, columnar.BuilderOptions{MaxRows: o.BatchRows, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.OutputMemory)})
	if err != nil {
		return err
	}
	layouts, err := c.residualLayouts()
	if err != nil {
		return err
	}
	if o.Residual != nil {
		c.predicateRow = make([]model.Value, len(layouts))
		c.predicateBuilder, err = columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.WorkMemory)})
		if err != nil {
			return err
		}
		c.predicate, err = expr.NewState(o.Residual, expr.StateOptions{WorkAllocator: o.Query.Allocator(runtime.WorkMemory), OutputAllocator: o.Query.Allocator(runtime.WorkMemory), Captures: o.Captures, Subqueries: o.Subqueries, Sequences: o.Sequences, Relations: o.Relations, Temporal: o.Temporal, Settings: o.Settings, TextSearch: o.TextSearch, Advisory: o.Advisory, Native: o.Native})
		if err != nil {
			return err
		}
	}
	return nil
}
func (c *cursor) check(ctx context.Context) error {
	return errors.Join(context.Cause(ctx), context.Cause(c.ctx), context.Cause(c.options.Query.Context()))
}
func (c *cursor) newSpool(ctx context.Context, side Side) (*rowstore.Spool, error) {
	o := c.options
	schema := c.leftSchema
	if side == Right {
		schema = c.rightSchema
	}
	memory := o.MemoryBytes / uint64(2*len(c.partitions)+1)
	return rowstore.New(ctx, o.Query, schema, rowstore.Limits{MemoryBytes: memory, MaxBytes: o.PartitionBytes, MaxRows: math.MaxInt64, MaxRecordBytes: o.MaxRecordBytes, MetadataBytes: c.metadata, DecodeBytes: o.MaxRecordBytes * 8, MaxReaders: 1})
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
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
	if err = c.check(ctx); err != nil {
		return err
	}
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	c.leftDemand = limit
	if !c.initialized {
		if err = c.initialize(callCtx); err != nil {
			return err
		}
		c.initialized = true
	}
	c.builder.Reset()
	var batchPartition uint32
	for c.builder.Rows() < limit {
		if err = c.check(callCtx); err != nil {
			return err
		}
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
		if c.partitioned != nil {
			partition := uint32(c.part)
			if c.builder.Rows() > 0 && partition != batchPartition {
				break
			}
			batchPartition = partition
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
		if err = c.Close(); err != nil {
			return err
		}
		return io.EOF
	}
	if err = c.builder.Publish(out); err != nil {
		return err
	}
	if c.partitioned != nil {
		c.partitioned.published = batchPartition
	}
	return nil
}

func (c *cursor) closePartition() error {
	var err error
	if c.leftReader != nil {
		err = errors.Join(err, c.leftReader.Close())
		c.leftReader = nil
	}
	if c.rightReader != nil {
		err = errors.Join(err, c.rightReader.Close())
		c.rightReader = nil
	}
	if c.replacement != nil {
		err = errors.Join(err, c.replacement.Close())
		c.replacement = nil
	}
	c.leftRow = nil
	c.entries = nil
	c.buckets = nil
	if c.hashClaim != nil {
		c.hashClaim.Release()
		c.hashClaim = nil
	}
	if c.part < len(c.partitions) {
		p := &c.partitions[c.part]
		if p.left != nil {
			err = errors.Join(err, p.left.Close())
			p.left = nil
		}
		if p.right != nil {
			err = errors.Join(err, p.right.Close())
			p.right = nil
		}
	}
	c.scanning = false
	c.draining = false
	c.hashMode = false
	c.unmatched = 0
	return err
}
func (c *cursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	if c.stopQuery != nil {
		c.stopQuery()
	}
	if c.left != nil {
		c.closeErr = errors.Join(c.closeErr, c.left.Close())
		c.left = nil
	}
	if c.right != nil {
		c.closeErr = errors.Join(c.closeErr, c.right.Close())
		c.right = nil
	}
	c.closeErr = errors.Join(c.closeErr, c.closePartition())
	for i := range c.partitions {
		p := &c.partitions[i]
		if p.left != nil {
			c.closeErr = errors.Join(c.closeErr, p.left.Close())
		}
		if p.right != nil {
			c.closeErr = errors.Join(c.closeErr, p.right.Close())
		}
	}
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
	}
	if c.predicateBuilder != nil {
		c.closeErr = errors.Join(c.closeErr, c.predicateBuilder.Close())
	}
	if c.predicate != nil {
		c.closeErr = errors.Join(c.closeErr, c.predicate.Close())
	}
	c.partitions = nil
	c.leftSchema = nil
	c.rightSchema = nil
	c.keyLayout = nil
	if c.partitioned != nil {
		c.partitioned.indices = nil
		c.partitioned.layout = nil
		c.partitioned.values = nil
	}
	c.keyValues = nil
	c.row = nil
	c.output = nil
	c.predicateRow = nil
	c.options = Options{}
	c.markBatch.Reset()
	c.markRow = nil
	c.ctx, c.cancel, c.stopQuery = nil, nil, nil
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}
