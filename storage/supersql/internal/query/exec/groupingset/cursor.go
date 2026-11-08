package groupingset

import (
	"context"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type cursor struct {
	options     Options
	input       storage.BatchCursor
	ctx         context.Context
	cancel      context.CancelFunc
	stopQuery   func() bool
	claim       columnar.Allocation
	spool       *rowstore.Spool
	inner       storage.BatchCursor
	replay      *replayCursor
	partitioned *partitionedState
	builder     *columnar.Builder
	passGroups  []int
	passLayouts []model.TypeLayout
	passDirectColumns []int
	row         []model.Value
	masks       []model.Value
	present     []bool
	batch       columnar.Batch
	set         int
	lane        int
	pending     bool
	closed      bool
	eof         bool
	closeErr    error
}

func closeDirectEvaluators(options aggregate.Options) error {
	if !options.OwnDirectEvaluators {
		return nil
	}
	var err error
	for _, call := range options.Calls {
		if call.DirectEvaluator != nil {
			err = errors.Join(err, call.DirectEvaluator.Close())
		}
	}
	return err
}

func (c *cursor) check(ctx context.Context) error {
	return errors.Join(context.Cause(ctx), context.Cause(c.ctx), context.Cause(c.options.Aggregate.Query.Context()))
}

func (c *cursor) freeze(ctx context.Context) error {
	var err error
	c.spool, err = rowstore.New(ctx, c.options.Aggregate.Query, c.options.Aggregate.InputLayouts, c.options.Spool)
	if err != nil {
		return err
	}
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		c.batch.Reset()
		c.batch.MaxRows = c.options.Aggregate.BatchRows
		err := c.input.Next(ctx, &c.batch)
		if err == io.EOF {
			input := c.input
			c.input = nil
			c.batch.Reset()
			c.closeErr = errors.Join(c.closeErr, input.Close())
			if c.closeErr != nil {
				return c.closeErr
			}
			return c.spool.Seal(ctx)
		}
		if err != nil {
			return err
		}
		if err := c.batch.Validate(max(c.batch.Length, c.options.Aggregate.BatchRows)); err != nil {
			return err
		}
		if c.batch.Rows() > c.options.Aggregate.BatchRows {
			return columnar.ErrInvalidBatch
		}
		if c.partitioned != nil {
			if err := c.authenticatePartition(ctx); err != nil {
				return err
			}
		}
		if _, err := c.spool.AppendBatch(ctx, &c.batch); err != nil {
			return err
		}
	}
}

func (c *cursor) passOptions(set []uint32) aggregate.Options {
	a := c.options.Aggregate
	for i, index := range set {
		c.passGroups[i] = a.GroupColumns[index]
		c.passLayouts[i] = a.OutputLayouts[index]
		if s := c.partitioned; s != nil {
			for key, ordinal := range s.indices {
				if int(index) == ordinal {
					s.passIndices[key] = i
				}
			}
		}
	}
	copy(c.passLayouts[len(set):], a.OutputLayouts[len(a.GroupColumns):len(a.GroupColumns)+len(a.Calls)])
	a.GroupColumns = c.passGroups[:len(set)]
	if len(a.DirectGroupColumns) != 0 {
		for i := range c.passDirectColumns {
			c.passDirectColumns[i] = -1
		}
		for i, index := range set {
			c.passDirectColumns[index] = i
		}
		a.DirectGroupColumns = c.passDirectColumns
	}
	a.OwnDirectEvaluators = false
	a.OutputLayouts = c.passLayouts[:len(set)+len(a.Calls)]
	// Contiguous full keys do not promise contiguous keys for a proper subset.
	a.OrderedGroups = a.OrderedGroups && len(set) == len(c.options.Aggregate.GroupColumns)
	return a
}

func (c *cursor) openPass(ctx context.Context) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	set := c.options.Sets[c.set]
	a := c.passOptions(set)
	if len(c.masks) != 0 {
		clear(c.present)
		for _, index := range set {
			c.present[index] = true
		}
		for i, args := range c.options.Groupings {
			if err := c.check(ctx); err != nil {
				return err
			}
			var mask int64
			for _, index := range args {
				mask <<= 1
				if !c.present[index] {
					mask |= 1
				}
			}
			c.masks[i] = model.Int64Val(mask)
		}
	}
	var input storage.BatchCursor
	if c.partitioned != nil {
		replay := &partitionedReplayCursor{replayCursor: replayCursor{ctx: c.ctx, rows: a.BatchRows, partitioned: c.partitioned, encoder: a.Encoder, maxRecordBytes: a.MaxRecordBytes}}
		c.replay, input = &replay.replayCursor, replay
	} else {
		c.replay = &replayCursor{ctx: c.ctx, rows: a.BatchRows}
		input = c.replay
	}
	var err error
	c.replay.reader, err = c.spool.Reader(ctx)
	if err != nil {
		return err
	}
	c.replay.builder, err = columnar.NewBuilder(a.InputLayouts, columnar.BuilderOptions{MaxRows: a.BatchRows, MaxBytes: a.BatchBytes, Allocator: a.Query.Allocator(runtime.OutputMemory)})
	if err != nil {
		return err
	}
	// The aggregate retains this context across Next calls; a per-call context
	// would be canceled as soon as the first output batch was returned.
	if s := c.partitioned; s != nil {
		c.inner, err = aggregate.NewPartitioned(c.ctx, input.(aggregate.PartitionedInput), aggregate.Partitioning{Count: s.count, KeyIndices: s.passIndices}, a)
	} else {
		c.inner, err = aggregate.New(c.ctx, input, a)
	}
	return err
}

func (c *cursor) closePass() error {
	c.batch.Reset()
	c.lane = 0
	clear(c.row)
	c.pending = false
	var err error
	if c.inner != nil {
		err = c.inner.Close()
		c.inner = nil
	}
	if c.replay != nil {
		err = errors.Join(err, c.replay.Close())
		c.replay = nil
	}
	return err
}

func (c *cursor) nextRow(ctx context.Context) error {
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		if c.set == len(c.options.Sets) {
			return io.EOF
		}
		if c.inner == nil {
			if err := c.openPass(ctx); err != nil {
				return err
			}
		}
		if c.lane < c.batch.Rows() {
			break
		}
		c.batch.Reset()
		c.batch.MaxRows = c.options.Aggregate.BatchRows
		err := c.inner.Next(ctx, &c.batch)
		if err == io.EOF {
			if err := c.closePass(); err != nil {
				c.closeErr = errors.Join(c.closeErr, err)
				return err
			}
			c.set++
			continue
		}
		if err != nil {
			return err
		}
		if err := c.batch.Validate(c.options.Aggregate.BatchRows); err != nil {
			return err
		}
		if len(c.batch.Columns) != len(c.options.Sets[c.set])+len(c.options.Aggregate.Calls) {
			return columnar.ErrValueLayout
		}
		c.lane = 0
	}
	clear(c.row)
	for i, layout := range c.options.Aggregate.OutputLayouts[:len(c.options.Aggregate.GroupColumns)] {
		c.row[i] = model.Value{Type: layout.Type.Storage, IsNull: true}
	}
	set := c.options.Sets[c.set]
	remaining := c.options.Aggregate.MaxRecordBytes * 8
	for i := range c.batch.Columns {
		value, used, err := columnar.CopyValue(ctx, &c.batch.Columns[i], c.batch.Lane(c.lane), remaining)
		if err != nil {
			return err
		}
		target := len(c.options.Aggregate.GroupColumns) + i - len(set)
		if i < len(set) {
			target = int(set[i])
		}
		c.row[target], remaining = value, remaining-used
	}
	copy(c.row[len(c.row)-len(c.masks):], c.masks)
	if c.partitioned != nil {
		c.partitioned.pending = c.inner.(aggregate.PartitionedCursor).Partition()
	}
	c.lane++
	c.pending = true
	return nil
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if c.closed {
		if c.eof {
			return io.EOF
		}
		return runtime.ErrClosed
	}
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, c.Close())
	}
	out.Reset()
	defer func() {
		if err != nil && err != io.EOF {
			if errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, model.ErrValueBudget) || errors.Is(err, storage.ErrDecodeBudget) || errors.Is(err, io.ErrShortWrite) {
				err = resource("grouping set row exceeds resource limit", err)
			}
			err = errors.Join(err, c.Close())
		}
	}()
	if err := c.check(ctx); err != nil {
		return err
	}
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	if c.spool == nil {
		if err := c.freeze(call); err != nil {
			return err
		}
	}
	c.builder.Reset()
	limit := c.options.Aggregate.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	var partition uint32
	for c.builder.Rows() < limit {
		if err := c.check(call); err != nil {
			return err
		}
		if !c.pending {
			if err := c.nextRow(call); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}
		if s := c.partitioned; s != nil {
			if c.builder.Rows() > 0 && partition != s.pending {
				break
			}
			partition = s.pending
		}
		if err := c.builder.Append(call, c.row); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
				break
			}
			return err
		}
		clear(c.row)
		c.pending = false
	}
	if c.builder.Rows() == 0 {
		if err := c.Close(); err != nil {
			return err
		}
		c.eof = true
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
	c.closeErr = errors.Join(c.closeErr, c.closePass())
	if c.input != nil {
		c.closeErr = errors.Join(c.closeErr, c.input.Close())
		c.input = nil
	}
	if c.spool != nil {
		c.closeErr = errors.Join(c.closeErr, c.spool.Close())
		c.spool = nil
	}
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	c.batch = columnar.Batch{}
	c.row, c.passGroups, c.passLayouts, c.passDirectColumns = nil, nil, nil, nil
	c.masks, c.present = nil, nil
	c.partitioned = nil
	c.closeErr = errors.Join(c.closeErr, closeDirectEvaluators(c.options.Aggregate))
	c.options = Options{}
	c.ctx, c.cancel, c.stopQuery = nil, nil, nil
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}
