package aggregate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type cursor struct {
	options       Options
	input         storage.BatchCursor
	ctx           context.Context
	cancel        context.CancelFunc
	stopQuery     func() bool
	claim         columnar.Allocation
	builder       *columnar.Builder
	layouts       []model.TypeLayout
	calls         []compiledCall
	groupKey      []storage.KeyColumn
	keyValues     []model.Value
	row           []model.Value
	output        []model.Value
	pending       []model.Value
	lookup        map[string]*group
	groups        []*group
	ordered       *group
	groupIndex    int
	maxGroups     int
	perGroup      uint64
	metadata      uint64
	sortWork      uint64
	hasDistinct   bool
	hasNumeric    bool
	hasOrder      bool
	hasOrderedSet bool
	sourceOrdinal int
	orderTag      int
	numericAdd    builtin.Callable
	numericDivide builtin.Callable
	numericTypmod builtin.Callable
	intervalSubtract builtin.Callable
	intervalMultiply builtin.Callable
	intervalAdd      builtin.Callable
	// directValues and arguments are per-row scratch reused across the whole scan;
	// their width is the plan's widest compiled call, so no call allocates per row.
	directValues    []model.Value
	arguments       []model.Value
	partition     *rowstore.Spool
	partitioned   *partitionedState
	deferred      *rowstore.Spool
	reader        *rowstore.Reader
	batch         columnar.Batch
	lane          int
	passes        int
	initialized   bool
	exhausted     bool
	globalEmitted bool
	closed        bool
	closeErr      error
}

// New validates all calls without pulling input. If OwnDirectEvaluators is set,
// evaluator ownership transfers on every outcome. Next and Close are serialized
// by the caller; returned batches are borrowed until either method is called.
func New(ctx context.Context, input storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	c := &cursor{input: input, options: options, sourceOrdinal: -1, orderTag: -1}
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func closeDirectEvaluators(calls []Call) error {
	var err error
	for _, call := range calls {
		if call.DirectEvaluator != nil {
			err = errors.Join(err, call.DirectEvaluator.Close())
		}
	}
	return err
}

func (c *cursor) check(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if c.ctx != nil {
		if err := context.Cause(c.ctx); err != nil {
			return err
		}
	}
	if c.options.Query != nil {
		return context.Cause(c.options.Query.Context())
	}
	return nil
}

func (c *cursor) newSpool(ctx context.Context, layouts []model.TypeLayout, memory bool) (*rowstore.Spool, error) {
	memoryBytes := uint64(0)
	if memory {
		memoryBytes = c.options.PartitionMemoryBytes
	}
	return rowstore.New(ctx, c.options.Query, layouts, rowstore.Limits{MemoryBytes: memoryBytes, MaxBytes: math.MaxInt, MaxRows: math.MaxInt64, MaxRecordBytes: c.options.MaxRecordBytes, MetadataBytes: c.metadata, DecodeBytes: c.options.MaxRecordBytes * 8, MaxReaders: 1})
}

func (c *cursor) groupBytes(ctx context.Context, row []model.Value) ([]byte, error) {
	for i, column := range c.options.GroupColumns {
		c.keyValues[i] = row[column]
	}
	key, err := c.options.Encoder.EncodePrefix(ctx, c.groupKey, c.keyValues[:len(c.groupKey)], c.options.MaxRecordBytes)
	if err != nil {
		return nil, err
	}
	if s := c.partitioned; s != nil {
		if uint64(cap(key)) > c.options.MaxRecordBytes {
			return nil, resource("aggregate grouping key exceeds resource limit", nil)
		}
		if s.input.reuseGroupKey && !s.input.closed && c.reader == nil {
			if err := s.validateGroup(ctx, c.options.Encoder, c.keyValues, key, c.options.MaxRecordBytes); err != nil {
				return nil, err
			}
		}
	}
	return key, nil
}

func (c *cursor) inputRow(ctx context.Context) ([]model.Value, error) {
	if c.reader != nil {
		_, row, err := c.reader.NextRow(ctx)
		return row, err
	}
	for c.lane >= c.batch.Rows() {
		if err := c.check(ctx); err != nil {
			return nil, err
		}
		c.batch.Reset()
		c.batch.MaxRows = c.options.BatchRows
		if err := c.input.Next(ctx, &c.batch); err != nil {
			return nil, err
		}
		if err := c.batch.Validate(max(c.batch.Length, c.options.BatchRows)); err != nil {
			return nil, err
		}
		if c.batch.Rows() > c.options.BatchRows {
			return nil, columnar.ErrInvalidBatch
		}
		if len(c.batch.Columns) != len(c.layouts) {
			return nil, columnar.ErrValueLayout
		}
		for i := range c.layouts {
			if !c.batch.Columns[i].MatchesLayout(c.layouts[i]) {
				return nil, columnar.ErrValueLayout
			}
		}
		c.lane = 0
	}
	remaining := c.options.MaxRecordBytes * 8
	for i := range c.layouts {
		value, used, err := columnar.CopyValue(ctx, &c.batch.Columns[i], c.batch.Lane(c.lane), remaining)
		if err != nil {
			return nil, err
		}
		c.row[i], remaining = value, remaining-used
	}
	c.lane++
	return c.row[:len(c.layouts)], nil
}

func (c *cursor) initialize(ctx context.Context) error {
	if c.hasOrder && c.hasDistinct {
		if err := c.preserveSourceOrder(ctx); err != nil {
			return err
		}
	}
	if c.hasDistinct {
		if err := c.prepareDistinct(ctx); err != nil {
			return err
		}
	}
	if c.hasOrder {
		if err := c.prepareOrder(ctx); err != nil {
			return err
		}
		c.initialized = true
		return nil
	}
	if c.options.OrderedGroups {
		if c.partition != nil {
			var err error
			c.reader, err = c.partition.Reader(ctx)
			if err != nil {
				return err
			}
		}
		c.initialized = true
		return nil
	}
	c.initialized = true
	return c.loadPartition(ctx)
}

func (c *cursor) partitionRow(ctx context.Context, row []model.Value) error {
	key, err := c.groupBytes(ctx, row)
	if err != nil {
		return err
	}
	if g, ok := c.lookup[string(key)]; ok {
		return c.accumulate(ctx, g, row)
	}
	if len(c.groups) == c.maxGroups {
		if c.deferred == nil {
			c.deferred, err = c.newSpool(ctx, c.layouts, false)
			if err != nil {
				return err
			}
		}
		_, err := c.deferred.AppendRow(ctx, row)
		return err
	}
	g, err := c.newGroup(ctx, key, row)
	if err != nil {
		return err
	}
	c.lookup[g.key] = g
	c.groups = append(c.groups, g)
	return c.accumulate(ctx, g, row)
}

func (c *cursor) loadPartition(ctx context.Context) error {
	c.passes++
	if c.passes > c.options.MaxPartitions {
		return resource("aggregate deferred partition limit", nil)
	}
	for _, g := range c.groups {
		g.clearArrays()
		g.clearRanges()
		if err := g.closeOrderedSet(); err != nil {
			return err
		}
		if err := g.releaseNumeric(); err != nil {
			return err
		}
	}
	clear(c.lookup)
	clear(c.groups)
	c.groups = c.groups[:0]
	c.groupIndex = 0
	if c.partition != nil {
		var err error
		c.reader, err = c.partition.Reader(ctx)
		if err != nil {
			return err
		}
	}
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		row, err := c.inputRow(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := c.partitionRow(ctx, row); err != nil {
			return err
		}
	}
	if c.input != nil {
		if err := c.input.Close(); err != nil {
			c.input = nil
			return err
		}
		c.input = nil
		c.batch.Reset()
	}
	if c.reader != nil {
		if err := c.reader.Close(); err != nil {
			c.reader = nil
			return err
		}
		c.reader = nil
	}
	if c.partition != nil {
		if err := c.partition.Close(); err != nil {
			c.partition = nil
			return err
		}
		c.partition = nil
	}
	clear(c.row)
	clear(c.keyValues)
	if c.deferred != nil {
		if err := c.deferred.Seal(ctx); err != nil {
			return err
		}
		c.partition, c.deferred = c.deferred, nil
	} else {
		c.exhausted = true
	}
	// Every pass admits at least one complete group, including a skewed hot key;
	// no hash-bit recursion can repeatedly repartition the same hot group.
	if len(c.groups) == 0 && len(c.groupKey) == 0 && !c.globalEmitted {
		g, err := c.newGroup(ctx, nil, nil)
		if err != nil {
			return err
		}
		c.groups = append(c.groups, g)
	}
	c.globalEmitted = true
	return nil
}

func (c *cursor) nextHash(ctx context.Context) ([]model.Value, error) {
	for c.groupIndex == len(c.groups) {
		if c.exhausted {
			return nil, io.EOF
		}
		if err := c.loadPartition(ctx); err != nil {
			return nil, err
		}
	}
	g := c.groups[c.groupIndex]
	c.groupIndex++
	return c.finishRetiredGroup(ctx, g)
}

func (c *cursor) nextOrdered(ctx context.Context) ([]model.Value, error) {
	for !c.exhausted {
		row, err := c.inputRow(ctx)
		if err == io.EOF {
			c.exhausted = true
			if c.ordered != nil {
				g := c.ordered
				c.ordered = nil
				c.globalEmitted = true
				return c.finishRetiredGroup(ctx, g)
			}
			break
		}
		if err != nil {
			return nil, err
		}
		key, err := c.groupBytes(ctx, row)
		if err != nil {
			return nil, err
		}
		if c.ordered == nil {
			c.ordered, err = c.newGroup(ctx, key, row)
			if err != nil {
				return nil, err
			}
		} else if !bytes.Equal(key, []byte(c.ordered.key)) {
			old := c.ordered
			next, err := c.newGroup(ctx, key, row)
			if err != nil {
				return nil, err
			}
			// The successor must become the open group before the retired one is
			// finished; otherwise every following row re-detects the same key
			// transition and re-emits the retired group.
			c.ordered = next
			if err := c.accumulate(ctx, next, row); err != nil {
				old.clearJSON()
				old.clearRanges()
				return nil, errors.Join(err, old.releaseNumeric(), old.closeOrderedSet())
			}
			c.globalEmitted = true
			return c.finishRetiredGroup(ctx, old)
		}
		if err := c.accumulate(ctx, c.ordered, row); err != nil {
			return nil, err
		}
	}
	if !c.globalEmitted && len(c.groupKey) == 0 {
		c.globalEmitted = true
		g, err := c.newGroup(ctx, nil, nil)
		if err != nil {
			return nil, err
		}
		return c.finishRetiredGroup(ctx, g)
	}
	return nil, io.EOF
}

func (c *cursor) finishRetiredGroup(ctx context.Context, g *group) (values []model.Value, err error) {
	defer g.clearJSON()
	defer func() { err = errors.Join(err, g.closeOrderedSet()) }()
	defer g.clearRanges()
	defer func() { err = errors.Join(err, g.releaseNumeric()) }()
	return c.finish(ctx, g)
}

func (c *cursor) finishNext(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	if errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, model.ErrValueBudget) || errors.Is(err, storage.ErrDecodeBudget) || errors.Is(err, io.ErrShortWrite) {
		err = resource("aggregate row exceeds resource limit", err)
	}
	return errors.Join(err, c.Close())
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if c.closed {
		return runtime.ErrClosed
	}
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, c.Close())
	}
	out.Reset()
	defer func() { err = c.finishNext(err) }()
	if err := c.check(ctx); err != nil {
		return err
	}
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	if !c.initialized {
		if err := c.initialize(callCtx); err != nil {
			return err
		}
	}
	c.builder.Reset()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	var partition uint32
	for c.builder.Rows() < limit {
		if err := c.check(callCtx); err != nil {
			return err
		}
		if c.pending == nil {
			var row []model.Value
			var err error
			if c.options.OrderedGroups {
				row, err = c.nextOrdered(callCtx)
			} else {
				row, err = c.nextHash(callCtx)
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			c.pending = row
		}
		if s := c.partitioned; s != nil {
			if c.builder.Rows() > 0 && partition != s.pending {
				break
			}
			partition = s.pending
		}
		if err := c.builder.Append(callCtx, c.pending); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
				break
			}
			return err
		}
		c.pending = nil
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
	if c.input != nil {
		c.closeErr = errors.Join(c.closeErr, c.input.Close())
		c.input = nil
	}
	if c.reader != nil {
		c.closeErr = errors.Join(c.closeErr, c.reader.Close())
		c.reader = nil
	}
	if c.partition != nil {
		c.closeErr = errors.Join(c.closeErr, c.partition.Close())
		c.partition = nil
	}
	if c.deferred != nil {
		c.closeErr = errors.Join(c.closeErr, c.deferred.Close())
		c.deferred = nil
	}
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	c.batch.Reset()
	for _, g := range c.groups {
		g.clearArrays()
		g.clearJSON()
		g.clearRanges()
		c.closeErr = errors.Join(c.closeErr, g.closeOrderedSet())
		c.closeErr = errors.Join(c.closeErr, g.releaseNumeric())
	}
	if c.ordered != nil {
		c.ordered.clearArrays()
		c.ordered.clearJSON()
		c.ordered.clearRanges()
		c.closeErr = errors.Join(c.closeErr, c.ordered.closeOrderedSet())
		c.closeErr = errors.Join(c.closeErr, c.ordered.releaseNumeric())
	}
	if c.options.OwnDirectEvaluators {
		if c.options.Calls != nil {
			c.closeErr = errors.Join(c.closeErr, closeDirectEvaluators(c.options.Calls))
		} else {
			for _, call := range c.calls {
				if call.directEvaluator != nil {
					c.closeErr = errors.Join(c.closeErr, call.directEvaluator.Close())
				}
			}
		}
	}
	c.lookup, c.groups, c.ordered = nil, nil, nil
	c.layouts, c.calls, c.groupKey = nil, nil, nil
	c.options.InputLayouts, c.options.OutputLayouts, c.options.GroupColumns, c.options.DirectGroupColumns, c.options.Calls = nil, nil, nil, nil, nil
	c.options.DirectGroupCount = 0
	c.options.Builtin = builtin.ResolveOptions{}
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	if s := c.partitioned; s != nil {
		if s.input.source != nil {
			c.closeErr = errors.Join(c.closeErr, s.input.Close())
		}
		s.indices, s.layout, s.values = nil, nil, nil
		s.input.owner = nil
		if s.claim != nil {
			s.claim.Release()
			s.claim = nil
		}
	}
	return c.closeErr
}
