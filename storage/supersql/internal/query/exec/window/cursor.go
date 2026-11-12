package window

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

var errDirectory = errors.New("window directory has nonuniform records")
var errInputOrder = errors.New("window input is not ordered within its partition")

var directoryLayout = []model.TypeLayout{{Type: model.Type{ID: 17, Storage: model.TypeBytes}}}

type directory struct {
	spool  *rowstore.Spool
	reader *rowstore.Reader
	stride uint64
}

// Fixed-width byte payloads keep random access O(1), while rowstore checksums
// protect both directory records and their referenced data. Every stride is
// checked on append; no codec framing size is embedded in this package.
func (d *directory) append(ctx context.Context, a, b uint64) error {
	var payload [16]byte
	binary.LittleEndian.PutUint64(payload[:8], a)
	binary.LittleEndian.PutUint64(payload[8:], b)
	before := d.spool.Bytes()
	pos, err := d.spool.AppendRow(ctx, []model.Value{model.BytesVal(payload[:])})
	if err != nil {
		return err
	}
	size := d.spool.Bytes() - before
	if d.stride == 0 {
		d.stride = size
	}
	if size != d.stride || uint64(pos) != (d.spool.Rows()-1)*d.stride {
		return errDirectory
	}
	return nil
}

func (d *directory) read(ctx context.Context, index int64) (uint64, uint64, error) {
	if index < 0 || uint64(index) >= d.spool.Rows() {
		return 0, 0, io.EOF
	}
	row, err := d.reader.Read(ctx, rowstore.Position(uint64(index)*d.stride))
	if err != nil {
		return 0, 0, err
	}
	if len(row) != 1 || row[0].IsNull || len(row[0].BytesVal) != 16 {
		return 0, 0, errDirectory
	}
	return binary.LittleEndian.Uint64(row[0].BytesVal[:8]), binary.LittleEndian.Uint64(row[0].BytesVal[8:]), nil
}

type cursor struct {
	input                                   storage.BatchCursor
	options                                 Options
	ctx                                     context.Context
	cancel                                  context.CancelFunc
	stopQuery                               func() bool
	claim                                   columnar.Allocation
	metadata                                uint64
	builder                                 *columnar.Builder
	partitionKeys, orderKeys                []storage.KeyColumn
	keyValues, inputValues, current, output []model.Value
	tiles                                   []tileState
	aggregates                              []aggregateState
	// aggregateArguments is per-row scratch reused across every frame replay; its
	// width is the widest aggregate call in the plan, so no call allocates per row.
	aggregateArguments                      []model.Value
	batch                                   columnar.Batch
	lane                                    int
	hasInput, inputEOF                      bool
	data                                    *rowstore.Spool
	reader                                  *rowstore.Reader
	rows, groups                            directory
	position                                int64
	startOffset, endOffset                  int64
	rangeOffsets                            [2]float64
	rangeValues                             [2]model.Value
	rangeCallables                          [2]builtin.Callable
	rangeFloat                              bool
	rangeNumeric                            bool
	rangeTemporal                           bool
	rangeMetadata                           uint64
	numericRanges                           [2]*builtin.NumericRange
	frameSpans                              [3]span
	frameCount                              int
	frameReady                              bool
	pending                                 bool
	closed                                  bool
	closeErr                                error
}

// New validates every function and frame before reading input. Next and Close
// require caller serialization; published batches are borrowed until either call.
func New(ctx context.Context, input storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	c := &cursor{input: input, options: options}
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
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

func (c *cursor) inputRow(ctx context.Context) error {
	if c.hasInput {
		return nil
	}
	if c.inputEOF {
		return io.EOF
	}
	for c.lane >= c.batch.Rows() {
		if err := c.check(ctx); err != nil {
			return err
		}
		c.batch.Reset()
		c.batch.MaxRows = c.options.BatchRows
		err := c.input.Next(ctx, &c.batch)
		if err == io.EOF {
			c.inputEOF = true
			err = c.input.Close()
			c.input = nil
			if err != nil {
				return err
			}
			return io.EOF
		}
		if err != nil {
			return err
		}
		if err := c.batch.Validate(c.options.BatchRows); err != nil {
			return err
		}
		if len(c.batch.Columns) != len(c.options.InputLayouts) {
			return columnar.ErrValueLayout
		}
		for i, layout := range c.options.InputLayouts {
			if !c.batch.Columns[i].MatchesLayout(layout) {
				return columnar.ErrValueLayout
			}
		}
		c.lane = 0
	}
	remaining := c.options.MaxRecordBytes * 8
	for i := range c.inputValues {
		value, used, err := columnar.CopyValue(ctx, &c.batch.Columns[i], c.batch.Lane(c.lane), remaining)
		if err != nil {
			return err
		}
		c.inputValues[i], remaining = value, remaining-used
	}
	c.lane++
	c.hasInput = true
	return nil
}

func (c *cursor) key(ctx context.Context, partition bool) ([]byte, error) {
	keys := c.orderKeys
	if partition {
		keys = c.partitionKeys
		for i, col := range c.options.PartitionColumns {
			c.keyValues[i] = c.inputValues[col]
		}
	} else {
		for i, key := range c.options.OrderBy {
			c.keyValues[i] = c.inputValues[key.Column]
		}
	}
	return c.options.Encoder.EncodePrefix(ctx, keys, c.keyValues[:len(keys)], c.options.MaxRecordBytes)
}

func (c *cursor) newSpool(ctx context.Context, layouts []model.TypeLayout) (*rowstore.Spool, error) {
	o := c.options
	return rowstore.New(ctx, o.Query, layouts, rowstore.Limits{MemoryBytes: o.PartitionMemoryBytes / 3, MaxBytes: o.MaxPartitionBytes, MaxRows: o.MaxPartitionRows, MaxRecordBytes: o.MaxRecordBytes, MetadataBytes: c.metadata, DecodeBytes: o.MaxRecordBytes * 8, MaxReaders: 1})
}

func (c *cursor) partitionLimit() error {
	if c.data.Bytes()+c.rows.spool.Bytes()+c.groups.spool.Bytes() > c.options.MaxPartitionBytes {
		return resource("window partition exceeds byte limit", nil)
	}
	return nil
}

func (c *cursor) loadPartition(ctx context.Context) error {
	if err := c.closePartition(); err != nil {
		return err
	}
	if err := c.inputRow(ctx); err != nil {
		return err
	}
	partition, err := c.key(ctx, true)
	if err != nil {
		return err
	}
	if c.data, err = c.newSpool(ctx, c.options.InputLayouts); err != nil {
		return err
	}
	if c.rows.spool, err = c.newSpool(ctx, directoryLayout); err != nil {
		return err
	}
	if c.groups.spool, err = c.newSpool(ctx, directoryLayout); err != nil {
		return err
	}
	var previous []byte
	var groupStart, group uint64
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		order, err := c.key(ctx, false)
		if err != nil {
			return err
		}
		count := c.data.Rows()
		if count > 0 {
			comparison := bytes.Compare(previous, order)
			if comparison > 0 {
				return errInputOrder
			}
			if comparison != 0 {
				if err := c.groups.append(ctx, groupStart, count); err != nil {
					return err
				}
				groupStart, group = count, group+1
			}
		}
		previous = order
		pos, err := c.data.AppendRow(ctx, c.inputValues)
		if err != nil {
			return err
		}
		if err := c.rows.append(ctx, uint64(pos), group); err != nil {
			return err
		}
		if err := c.partitionLimit(); err != nil {
			return err
		}
		c.hasInput = false
		if err := c.inputRow(ctx); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		next, err := c.key(ctx, true)
		if err != nil {
			return err
		}
		if !bytes.Equal(partition, next) {
			break
		}
	}
	if err := c.groups.append(ctx, groupStart, c.data.Rows()); err != nil {
		return err
	}
	if err := c.partitionLimit(); err != nil {
		return err
	}
	for _, spool := range []*rowstore.Spool{c.data, c.rows.spool, c.groups.spool} {
		if err := spool.Seal(ctx); err != nil {
			return err
		}
	}
	if c.reader, err = c.data.Reader(ctx); err != nil {
		return err
	}
	if c.rows.reader, err = c.rows.spool.Reader(ctx); err != nil {
		return err
	}
	if c.groups.reader, err = c.groups.spool.Reader(ctx); err != nil {
		return err
	}
	c.position = 0
	clear(c.tiles)
	for i := range c.aggregates {
		state := &c.aggregates[i]
		if state.accumulator != nil {
			if err := state.accumulator.Reset(); err != nil {
				return err
			}
			state.valid = false
		}
	}
	return nil
}

func (c *cursor) nextRow(ctx context.Context) error {
	c.frameReady = false
	if c.data == nil || uint64(c.position) == c.data.Rows() {
		if err := c.loadPartition(ctx); err != nil {
			return err
		}
	}
	pos, group, err := c.rows.read(ctx, c.position)
	if err != nil {
		return err
	}
	row, err := c.reader.Read(ctx, rowstore.Position(pos))
	if err != nil {
		return err
	}
	remaining := c.options.MaxRecordBytes * 8
	for i, value := range row {
		owned, used, err := model.CopyValue(ctx, value, remaining)
		if err != nil {
			return err
		}
		c.current[i], remaining = owned, remaining-used
	}
	start, end, err := c.groups.read(ctx, int64(group))
	if err != nil {
		return err
	}
	for i, col := range c.options.OutputColumns {
		c.output[i] = c.current[col]
	}
	for i, call := range c.options.Calls {
		var value model.Value
		var err error
		if c.aggregates[i].accumulator != nil {
			value, err = c.aggregate(ctx, call, &c.aggregates[i], int64(group), int64(start), int64(end))
		} else {
			value, err = c.evaluate(ctx, call, &c.tiles[i], int64(group), int64(start), int64(end))
		}
		if err != nil {
			return err
		}
		c.output[len(c.options.OutputColumns)+i] = value
	}
	c.position++
	return nil
}

func (c *cursor) finishNext(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	if errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, model.ErrValueBudget) || errors.Is(err, storage.ErrDecodeBudget) || errors.Is(err, io.ErrShortWrite) {
		err = resource("window row exceeds resource limit", err)
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
	c.builder.Reset()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for c.builder.Rows() < limit {
		if err := c.check(callCtx); err != nil {
			return err
		}
		if !c.pending {
			if err := c.nextRow(callCtx); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			c.pending = true
		}
		if err := c.builder.Append(callCtx, c.output); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
				break
			}
			return err
		}
		c.pending = false
	}
	if c.builder.Rows() == 0 {
		if err := c.Close(); err != nil {
			return err
		}
		return io.EOF
	}
	return c.builder.Publish(out)
}

func (c *cursor) closePartition() error {
	c.frameReady = false
	var err error
	for _, reader := range []*rowstore.Reader{c.reader, c.rows.reader, c.groups.reader} {
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
	}
	for _, spool := range []*rowstore.Spool{c.data, c.rows.spool, c.groups.spool} {
		if spool != nil {
			err = errors.Join(err, spool.Close())
		}
	}
	c.reader, c.data = nil, nil
	c.rows, c.groups = directory{}, directory{}
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
	if c.input != nil {
		c.closeErr = errors.Join(c.closeErr, c.input.Close())
		c.input = nil
	}
	c.closeErr = errors.Join(c.closeErr, c.closePartition())
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	for i := range c.aggregates {
		if c.aggregates[i].accumulator != nil {
			c.closeErr = errors.Join(c.closeErr, c.aggregates[i].accumulator.Close())
		}
	}
	c.aggregates = nil
	for i, bound := range c.numericRanges {
		if bound != nil {
			c.closeErr = errors.Join(c.closeErr, bound.Close())
			c.numericRanges[i] = nil
		}
	}
	c.batch.Reset()
	c.partitionKeys, c.orderKeys = nil, nil
	c.keyValues, c.inputValues, c.current, c.output = nil, nil, nil, nil
	c.tiles = nil
	c.options.InputLayouts, c.options.OutputLayouts = nil, nil
	c.options.OutputColumns, c.options.PartitionColumns, c.options.OrderBy, c.options.Calls = nil, nil, nil, nil
	c.rangeCallables = [2]builtin.Callable{}
	c.rangeValues = [2]model.Value{}
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}
