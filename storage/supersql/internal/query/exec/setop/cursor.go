package setop

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
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type group struct {
	key    string
	row    []model.Value
	counts [2]uint64
}

type cursor struct {
	options     Options
	inputs      [2]storage.BatchCursor
	ctx         context.Context
	cancel      context.CancelFunc
	stopQuery   func() bool
	claim       columnar.Allocation
	builder     *columnar.Builder
	layouts     []model.TypeLayout
	keyLayout   []storage.KeyColumn
	orderLayout []storage.KeyColumn
	row         []model.Value
	keyValues   []model.Value
	pending     []model.Value
	lastKey     []byte
	lastOrder   []byte
	haveOrder   bool
	lookup      map[string]*group
	groups      []*group
	groupIndex  int
	maxGroups   int
	repeats     uint64
	metadata    uint64
	spillBytes  uint64
	partition   *rowstore.Spool
	deferred    *rowstore.Spool
	reader      *rowstore.Reader
	batch       columnar.Batch
	lane        int
	side        int
	passes      int
	initialized bool
	exhausted   bool
	closed      bool
	closeErr    error
}

func (c *cursor) unionAll() bool {
	return c.options.Mode == Binary && c.options.Kind == ir.Union && c.options.All
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

func (c *cursor) inputRow(ctx context.Context) ([]model.Value, error) {
	if c.reader != nil {
		_, row, err := c.reader.NextRow(ctx)
		return row, err
	}
	for c.lane >= c.batch.Rows() {
		if err := c.check(ctx); err != nil {
			return nil, err
		}
		if c.side >= len(c.inputs) || c.inputs[c.side] == nil {
			return nil, io.EOF
		}
		c.batch.Reset()
		c.batch.MaxRows = c.options.BatchRows
		err := c.inputs[c.side].Next(ctx, &c.batch)
		if err == io.EOF {
			input := c.inputs[c.side]
			c.inputs[c.side] = nil
			c.batch.Reset()
			c.lane = 0
			c.side++
			if err := input.Close(); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := c.batch.Validate(c.options.BatchRows); err != nil {
			return nil, err
		}
		if len(c.batch.Columns) != len(c.options.Layouts) {
			return nil, columnar.ErrValueLayout
		}
		for i, layout := range c.options.Layouts {
			if !c.batch.Columns[i].MatchesLayout(layout) {
				return nil, columnar.ErrValueLayout
			}
		}
		c.lane = 0
	}
	clear(c.row)
	remaining := c.options.MaxRecordBytes * 8
	for i := range c.options.Layouts {
		value, used, err := columnar.CopyValue(ctx, &c.batch.Columns[i], c.batch.Lane(c.lane), remaining)
		if err != nil {
			return nil, err
		}
		c.row[i], remaining = value, remaining-used
	}
	c.row[len(c.options.Layouts)] = model.Int64Val(int64(c.side))
	c.lane++
	return c.row, nil
}

func (c *cursor) key(ctx context.Context, row []model.Value) ([]byte, error) {
	for i, col := range c.options.DistinctColumns {
		c.keyValues[i] = row[col]
	}
	return c.options.Encoder.EncodePrefix(ctx, c.keyLayout, c.keyValues[:len(c.keyLayout)], c.options.MaxRecordBytes)
}

func (c *cursor) newGroup(ctx context.Context, key []byte, row []model.Value) (*group, error) {
	g := &group{key: string(key), row: make([]model.Value, len(c.options.Layouts))}
	remaining := c.options.MaxRecordBytes * 8
	for i := range g.row {
		value, used, err := model.CopyValue(ctx, row[i], remaining)
		if err != nil {
			return nil, err
		}
		g.row[i], remaining = value, remaining-used
	}
	return g, nil
}

func (c *cursor) deferRow(ctx context.Context, row []model.Value) error {
	if c.deferred == nil {
		if c.spillBytes >= c.options.MaxSpillBytes {
			return resource("set operation deferred byte limit", nil)
		}
		var err error
		c.deferred, err = rowstore.New(ctx, c.options.Query, c.layouts, rowstore.Limits{
			MemoryBytes: c.options.PartitionMemoryBytes, MaxBytes: c.options.MaxSpillBytes - c.spillBytes,
			MaxRows: math.MaxInt64, MaxRecordBytes: c.options.MaxRecordBytes,
			MetadataBytes: c.metadata, DecodeBytes: c.options.MaxRecordBytes * 8, MaxReaders: 1,
		})
		if err != nil {
			return err
		}
	}
	before := c.deferred.Bytes()
	_, err := c.deferred.AppendRow(ctx, row)
	c.spillBytes += c.deferred.Bytes() - before
	return err
}

func (c *cursor) loadPartition(ctx context.Context) error {
	if c.passes == c.options.MaxPasses {
		return resource("set operation deferred pass limit", nil)
	}
	c.passes++
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
		key, err := c.key(ctx, row)
		if err != nil {
			return err
		}
		g, ok := c.lookup[string(key)]
		if !ok {
			if len(c.groups) == c.maxGroups {
				if err := c.deferRow(ctx, row); err != nil {
					return err
				}
				continue
			}
			g, err = c.newGroup(ctx, key, row)
			if err != nil {
				return err
			}
			c.lookup[g.key] = g
			c.groups = append(c.groups, g)
		}
		side := row[len(c.options.Layouts)].IntVal
		if side < 0 || side > 1 {
			return columnar.ErrValueLayout
		}
		if g.counts[side] == math.MaxUint64 {
			return ErrMultiplicityOverflow
		}
		g.counts[side]++
	}
	if c.reader != nil {
		err := c.reader.Close()
		c.reader = nil
		if err != nil {
			return err
		}
	}
	if c.partition != nil {
		err := c.partition.Close()
		c.partition = nil
		if err != nil {
			return err
		}
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
	return nil
}

func (c *cursor) multiplicity(g *group) uint64 {
	left, right := g.counts[0], g.counts[1]
	if c.options.Mode == Distinct || c.options.Kind == ir.Union {
		return 1
	}
	if c.options.Kind == ir.Intersect {
		if c.options.All {
			return min(left, right)
		}
		if left > 0 && right > 0 {
			return 1
		}
		return 0
	}
	if c.options.All {
		if left > right {
			return left - right
		}
		return 0
	}
	if left > 0 && right == 0 {
		return 1
	}
	return 0
}

func (c *cursor) nextHash(ctx context.Context) ([]model.Value, error) {
	for {
		if err := c.check(ctx); err != nil {
			return nil, err
		}
		if c.repeats > 0 {
			c.repeats--
			return c.groups[c.groupIndex-1].row, nil
		}
		if c.groupIndex < len(c.groups) {
			g := c.groups[c.groupIndex]
			c.groupIndex++
			c.repeats = c.multiplicity(g)
			continue
		}
		if c.exhausted {
			return nil, io.EOF
		}
		if err := c.loadPartition(ctx); err != nil {
			return nil, err
		}
	}
}

func (c *cursor) nextOrdered(ctx context.Context) ([]model.Value, error) {
	for {
		if err := c.check(ctx); err != nil {
			return nil, err
		}
		row, err := c.inputRow(ctx)
		if err != nil {
			return nil, err
		}
		for i, key := range c.options.OrderedInput {
			c.keyValues[i] = row[key.Column]
		}
		order, err := c.options.Encoder.EncodePrefix(ctx, c.orderLayout, c.keyValues[:len(c.orderLayout)], c.options.MaxRecordBytes)
		if err != nil {
			return nil, err
		}
		if c.haveOrder && bytes.Compare(order, c.lastOrder) < 0 {
			return nil, ErrInputOrder
		}
		key, err := c.key(ctx, row)
		if err != nil {
			return nil, err
		}
		same := c.haveOrder && bytes.Equal(key, c.lastKey)
		c.lastOrder, c.lastKey, c.haveOrder = order, key, true
		if !same {
			return row[:len(c.options.Layouts)], nil
		}
	}
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
			if errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, model.ErrValueBudget) || errors.Is(err, storage.ErrDecodeBudget) || errors.Is(err, io.ErrShortWrite) {
				err = resource("set operation row exceeds resource limit", err)
			}
			err = errors.Join(err, c.Close())
		}
	}()
	if err := c.check(ctx); err != nil {
		return err
	}
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	if !c.initialized {
		if c.options.Mode != DistinctOn && !c.unionAll() {
			if err := c.loadPartition(callCtx); err != nil {
				return err
			}
		}
		c.initialized = true
	}
	c.builder.Reset()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for c.builder.Rows() < limit {
		if err := c.check(callCtx); err != nil {
			return err
		}
		if c.pending == nil {
			var row []model.Value
			var err error
			switch {
			case c.unionAll():
				row, err = c.inputRow(callCtx)
				if err == nil {
					row = row[:len(c.options.Layouts)]
				}
			case c.options.Mode == DistinctOn:
				row, err = c.nextOrdered(callCtx)
			default:
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
	return c.builder.Publish(out)
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
	for i, input := range c.inputs {
		if input != nil {
			c.closeErr = errors.Join(c.closeErr, input.Close())
			c.inputs[i] = nil
		}
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
	c.lookup, c.groups = nil, nil
	c.layouts, c.keyLayout, c.orderLayout = nil, nil, nil
	c.row, c.keyValues, c.pending = nil, nil, nil
	c.lastKey, c.lastOrder = nil, nil
	c.options.Layouts, c.options.DistinctColumns, c.options.OrderedInput = nil, nil, nil
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}
