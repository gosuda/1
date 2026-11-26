package join

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// MergeOrder applies independently to each key; NULL placement does not reverse
// with Descending. Both children must deliver this complete ordering.
type MergeOrder struct {
	Descending bool
	NullsFirst bool
}

var errMergeOrder = &model.Error{Code: "XX000", Message: "merge join input is not ordered"}

type mergeInput struct {
	batch                     columnar.Batch
	row                       []model.Value
	index                     int
	partition, batchPartition uint32
	ready, eof                bool
}

type mergeCursor struct {
	cursor
	inputs                                [2]mergeInput
	group                                 *rowstore.Spool
	replay                                *rowstore.Reader
	matched                               *distinctset.Set
	groupKey                              []byte
	ordinal                               uint64
	setWork                               uint64
	groupPartition, outputPartition       uint32
	groupComplete, leftComplete, draining bool
}

type mergeWorkspace struct {
	base, fixed, metadata, set uint64
}

// CheckMergeSupport checks metadata, ordered key support and the complete work
// bound without taking ownership or reserving query memory.
func CheckMergeSupport(ctx context.Context, options Options, ordering []MergeOrder) error {
	_, err := checkMerge(ctx, options, ordering)
	return resourceCause(err)
}

func checkMerge(ctx context.Context, o Options, ordering []MergeOrder) (mergeWorkspace, error) {
	var work mergeWorkspace
	missing := o.Query == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRecordBytes < 128
	invalidBytes := o.HashBytes == 0 || o.WorkBytes == 0 || o.PartitionBytes == 0
	invalidLimits := o.MaxPartitions <= 0 || o.MaxDepth <= 0
	if missing || invalidBatch || invalidBytes || invalidLimits {
		return work, runtime.ErrInvalidConfig
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(o.Query.Context())); err != nil {
		return work, err
	}
	switch o.Kind {
	case ir.InnerJoin, ir.LeftJoin, ir.RightJoin, ir.FullJoin, ir.SemiJoin, ir.AntiJoin:
	default:
		return work, runtime.ErrInvalidConfig
	}
	if o.MarkKind != 0 || len(o.Keys) == 0 || len(ordering) != len(o.Keys) || len(o.Output) != len(o.OutputLayouts) {
		return work, runtime.ErrInvalidConfig
	}
	for _, n := range []uint64{o.BatchBytes, o.MaxRecordBytes, o.MemoryBytes, o.HashBytes, o.WorkBytes, o.PartitionBytes, uint64(o.MaxPartitions), uint64(o.MaxDepth), uint64(o.BatchRows)} {
		if n > uint64(math.MaxInt)/1024 {
			return work, runtime.ErrInvalidConfig
		}
	}
	semantic := cursor{options: o}
	for i, col := range o.Output {
		layout, err := semantic.layout(col)
		leftOnly := o.Kind == ir.SemiJoin || o.Kind == ir.AntiJoin
		wrongSide := leftOnly && col.Side != Left
		if err != nil || !model.TypeLayoutsEqual(layout, o.OutputLayouts[i]) || wrongSide {
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
	for i, key := range o.Keys {
		if key.Left < 0 || key.Left >= len(o.LeftLayouts) || key.Right < 0 || key.Right >= len(o.RightLayouts) || !model.TypeLayoutsEqual(o.LeftLayouts[key.Left], o.RightLayouts[key.Right]) {
			return work, runtime.ErrInvalidConfig
		}
		layout := o.LeftLayouts[key.Left]
		column := [1]storage.KeyColumn{{ColumnID: 1, Type: layout.Type, Children: layout.Children, Descending: ordering[i].Descending, NullsFirst: ordering[i].NullsFirst}}
		if _, err := o.Encoder.EncodePrefix(ctx, column[:], nil, o.MaxRecordBytes); err != nil {
			return work, err
		}
		if layout.Type.Storage == model.TypeNumeric {
			numeric++
		}
	}
	work.base = uint64(unsafe.Sizeof(mergeCursor{})) + 8192 + uint64(len(o.LeftLayouts)+len(o.RightLayouts)+len(o.OutputLayouts)+len(o.ResidualInputs)+len(o.Keys)+8)*512
	// Two copied lookahead rows, old/new ordered keys, group key, and numeric
	// encoder preparation coexist. Spool append and replay have separate claims.
	work.fixed = work.base + o.MaxRecordBytes*20 + numeric*(1<<20)
	work.metadata = work.base + 4096
	if semantic.preservesRight() {
		ordinal := [1]storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}, NullsFirst: true}}
		if _, err := o.Encoder.EncodePrefix(ctx, ordinal[:], nil, 64); err != nil {
			return work, err
		}
		work.set = o.HashBytes + 8192 + uint64(o.MaxDepth+2)*1024
	}
	spool := work.metadata + o.MemoryBytes + 2*(o.MaxRecordBytes*9+8192)
	if work.fixed > o.WorkBytes || spool > o.WorkBytes-work.fixed || work.set > o.WorkBytes-work.fixed-spool {
		return work, resource("admit merge join workspace", nil)
	}
	return work, nil
}

// NewMerge owns both children on every outcome; other collaborators are
// borrowed as in New. It retains only the current right equality group, bounded
// by MemoryBytes and PartitionBytes. Right/full matches use HashBytes of
// resident ordinal membership and MaxDepth spill levels. At most MaxDepth+3
// files and one reader coexist (one file for joins without right preservation).
func NewMerge(ctx context.Context, left, right storage.BatchCursor, options Options, ordering []MergeOrder) (storage.BatchCursor, error) {
	c := &mergeCursor{cursor: cursor{left: left, right: right, options: options}}
	if left == nil || right == nil {
		return nil, errors.Join(runtime.ErrInvalidConfig, c.Close())
	}
	work, err := checkMerge(ctx, options, ordering)
	if err != nil {
		return nil, errors.Join(resourceCause(err), c.Close())
	}
	if err := c.configureMerge(ctx, work, ordering); err != nil {
		return nil, errors.Join(resourceCause(err), c.Close())
	}
	return c, nil
}

func (c *mergeCursor) configureMerge(ctx context.Context, work mergeWorkspace, ordering []MergeOrder) error {
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(c.options.Query.Context(), c.cancel)
	c.metadata, c.setWork = work.metadata, work.set
	var err error
	c.claim, err = c.options.Query.Allocator(runtime.WorkMemory).Reserve(work.fixed)
	if err != nil {
		return err
	}
	if err := c.configureRows(ctx, work.base); err != nil {
		return err
	}
	for i, order := range ordering {
		c.keyLayout[i].Descending = order.Descending
		c.keyLayout[i].NullsFirst = order.NullsFirst
	}
	c.inputs[0].row = make([]model.Value, len(c.leftSchema))
	c.inputs[1].row = make([]model.Value, len(c.rightSchema))
	return nil
}

func (c *mergeCursor) load(ctx context.Context, side Side) error {
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
		if c.partitioned != nil {
			input.batchPartition = (*child).(PartitionedInput).Partition()
			if input.batchPartition >= c.partitioned.count {
				return errInputPartition
			}
		}
	}
	previous := input.row[0].BytesVal
	lane := input.batch.Lane(input.index)
	input.index++
	if c.partitioned != nil {
		c.partitioned.input = input.batchPartition
	}
	if err := c.prepareRow(ctx, &input.batch, lane, side, input.row); err != nil {
		return err
	}
	key := input.row[0].BytesVal
	if uint64(cap(key)) > c.options.MaxRecordBytes {
		return resource("merge join key exceeds record limit", nil)
	}
	if previous != nil && bytes.Compare(previous, key) > 0 {
		return errMergeOrder
	}
	if side == Left {
		if err := c.checkRecord(ctx, input.row); err != nil {
			return err
		}
	}
	input.partition = input.batchPartition
	input.ready = true
	return nil
}

// Unspooled rows obey the replay frame limit without retaining another encoding.
func (c *mergeCursor) checkRecord(ctx context.Context, row []model.Value) error {
	return storage.EncodeValue(ctx, io.Discard, model.Value{Type: model.TypeComposite, ArrayVal: row}, c.options.MaxRecordBytes-32)
}

func (c *mergeCursor) beginGroup(ctx context.Context) error {
	if c.partitioned != nil && c.inputs[0].partition != c.inputs[1].partition {
		return errInputPartition
	}
	o := &c.options
	var err error
	c.group, err = rowstore.New(ctx, o.Query, c.rightSchema, rowstore.Limits{MemoryBytes: o.MemoryBytes, MaxBytes: o.PartitionBytes, MaxRows: o.PartitionBytes, MaxRecordBytes: o.MaxRecordBytes, MetadataBytes: c.metadata, DecodeBytes: o.MaxRecordBytes * 8, MaxReaders: 1})
	if err != nil {
		return err
	}
	if c.preservesRight() {
		c.matched, err = distinctset.New(c.ctx, distinctset.Options{Query: o.Query, Layouts: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}, Encoder: o.Encoder, MemoryBytes: o.HashBytes, WorkBytes: c.setWork, MaxKeyBytes: 64, MaxKeys: o.PartitionBytes, MaxLevels: o.MaxDepth})
		if err != nil {
			return err
		}
	}
	c.groupKey = c.inputs[0].row[0].BytesVal
	c.groupPartition = c.inputs[1].partition
	c.leftComplete, c.groupComplete, c.leftMatched = false, false, false
	c.ordinal = 0
	return nil
}

func (c *mergeCursor) closeReplay() error {
	if c.replay == nil {
		return nil
	}
	err := c.replay.Close()
	c.replay = nil
	return err
}

func (c *mergeCursor) closeGroup() error {
	err := c.closeReplay()
	if c.group != nil {
		err = errors.Join(err, c.group.Close())
		c.group = nil
	}
	if c.matched != nil {
		err = errors.Join(err, c.matched.Close())
		c.matched = nil
	}
	c.groupKey = nil
	c.draining = false
	return err
}

func (c *mergeCursor) candidate(ctx context.Context) ([]model.Value, error) {
	if c.replay != nil {
		_, row, err := c.replay.NextRow(ctx)
		if err == nil {
			c.ordinal++
			return row, nil
		}
		if err != io.EOF {
			return nil, err
		}
		if err := c.closeReplay(); err != nil {
			return nil, err
		}
	}
	if c.groupComplete {
		return nil, io.EOF
	}
	err := c.load(ctx, Right)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if err == io.EOF || !bytes.Equal(c.groupKey, c.inputs[1].row[0].BytesVal) {
		c.groupComplete = true
		return nil, io.EOF
	}
	if c.partitioned != nil && c.inputs[1].partition != c.groupPartition {
		return nil, errInputPartition
	}
	row := c.inputs[1].row
	if _, err := c.group.AppendRow(ctx, row); err != nil {
		return nil, err
	}
	c.inputs[1].ready = false
	c.ordinal++
	return row, nil
}

func (c *mergeCursor) nextGroup(ctx context.Context) error {
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		if c.draining {
			_, row, err := c.replay.NextRow(ctx)
			if err != nil {
				return err
			}
			c.ordinal++
			ordinal := [1]model.Value{model.Int64Val(int64(c.ordinal))}
			matched, err := c.matched.Contains(ctx, ordinal[:])
			if err != nil {
				return err
			}
			if !matched {
				c.mapRow(c.output, c.options.Output, nil, row)
				c.outputPartition = c.groupPartition
				return nil
			}
			continue
		}
		if c.leftComplete {
			if err := c.closeReplay(); err != nil {
				return err
			}
			err := c.load(ctx, Left)
			if err != nil && err != io.EOF {
				return err
			}
			if err == io.EOF || !bytes.Equal(c.groupKey, c.inputs[0].row[0].BytesVal) {
				if !c.preservesRight() {
					return io.EOF
				}
				c.draining = true
			} else {
				if c.partitioned != nil && c.inputs[0].partition != c.groupPartition {
					return errInputPartition
				}
				c.leftComplete, c.leftMatched = false, false
			}
			c.replay, err = c.group.PrefixReader(ctx)
			if err != nil {
				return err
			}
			c.ordinal = 0
			continue
		}
		right, err := c.candidate(ctx)
		if err == io.EOF {
			c.leftComplete = true
			c.inputs[0].ready = false
			if !c.leftMatched && c.emitsUnmatchedLeft() {
				c.mapRow(c.output, c.options.Output, c.inputs[0].row, nil)
				c.outputPartition = c.inputs[0].partition
				return nil
			}
			continue
		}
		if err != nil {
			return err
		}
		truth, err := c.matches(ctx, c.inputs[0].row, right)
		if err != nil {
			return err
		}
		if truth.IsNull || truth.IntVal == 0 {
			continue
		}
		c.leftMatched = true
		if c.matched != nil {
			ordinal := [1]model.Value{model.Int64Val(int64(c.ordinal))}
			if _, err := c.matched.Insert(ctx, ordinal[:]); err != nil {
				return err
			}
		}
		switch c.options.Kind {
		case ir.AntiJoin, ir.SemiJoin:
			c.inputs[0].ready = false
			c.leftComplete = true
			if c.options.Kind == ir.AntiJoin {
				continue
			}
			c.mapRow(c.output, c.options.Output, c.inputs[0].row, nil)
		default:
			c.mapRow(c.output, c.options.Output, c.inputs[0].row, right)
		}
		c.outputPartition = c.groupPartition
		return nil
	}
}

func (c *mergeCursor) next(ctx context.Context) error {
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		if c.group != nil {
			err := c.nextGroup(ctx)
			if err != io.EOF {
				return err
			}
			if err := c.closeGroup(); err != nil {
				return err
			}
		}
		leftErr := c.load(ctx, Left)
		if leftErr != nil && leftErr != io.EOF {
			return leftErr
		}
		if leftErr == io.EOF && !c.preservesRight() {
			return io.EOF
		}
		rightErr := c.load(ctx, Right)
		if rightErr != nil && rightErr != io.EOF {
			return rightErr
		}
		if leftErr == io.EOF && rightErr == io.EOF {
			return io.EOF
		}
		left, right := c.inputs[0].row, c.inputs[1].row
		comparison := 0
		if leftErr == io.EOF {
			comparison = 1
		} else if rightErr == io.EOF {
			comparison = -1
		} else {
			comparison = bytes.Compare(left[0].BytesVal, right[0].BytesVal)
		}
		if comparison == 0 && left[1].IntVal != 0 && right[1].IntVal != 0 {
			if err := c.beginGroup(ctx); err != nil {
				return err
			}
			continue
		}
		if comparison <= 0 {
			c.inputs[0].ready = false
			if c.emitsUnmatchedLeft() {
				c.mapRow(c.output, c.options.Output, left, nil)
				c.outputPartition = c.inputs[0].partition
				return nil
			}
			if rightErr == io.EOF {
				return io.EOF
			}
			continue
		}
		if err := c.checkRecord(ctx, right); err != nil {
			return err
		}
		c.inputs[1].ready = false
		if c.preservesRight() {
			c.mapRow(c.output, c.options.Output, nil, right)
			c.outputPartition = c.inputs[1].partition
			return nil
		}
	}
}

func (c *mergeCursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
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
	var partition uint32
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
		if c.partitioned != nil && c.builder.Rows() > 0 && partition != c.outputPartition {
			break
		}
		err = c.builder.Append(callCtx, c.output)
		if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
			break
		}
		if err != nil {
			return err
		}
		partition = c.outputPartition
		c.pending = false
		clear(c.output)
	}
	if c.builder.Rows() == 0 {
		if err := c.Close(); err != nil {
			return err
		}
		return io.EOF
	}
	if err := c.builder.Publish(out); err != nil {
		return err
	}
	if c.partitioned != nil {
		c.partitioned.published = partition
	}
	return nil
}

func (c *mergeCursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closeErr = errors.Join(c.closeErr, c.closeGroup())
	for i := range c.inputs {
		c.inputs[i].batch.Reset()
		c.inputs[i].row = nil
	}
	return c.cursor.Close()
}
