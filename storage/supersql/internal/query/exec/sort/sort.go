// Package sort implements stable, bounded external ordering of typed batches.
package sort

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type Key struct {
	Column     int
	Descending bool
	NullsFirst bool
	// Zero uses the input layout's collation without changing payload metadata.
	InputCollation model.CollationID
}

// Limits must be positive; MaxRuns >= MergeFanIn >= 2. MaxRecordBytes includes
// the rowstore frame, encoded SQL key and stable ordinal. RunBytes bounds each
// unsorted row extent and each of its key/index arenas. WorkBytes bounds the
// conservative total of sort-owned work, excluding the input and output batch.
// MaxRuns bounds buffered on-disk run descriptors; MergeFanIn bounds each merge.
// Provision three query files and MergeFanIn query readers. Two tapes alternate
// balanced passes; a third file is needed only if the raw run spills.
// Layouts and Keys are copied. Encoder and Query are borrowed through Close.
type Options struct {
	Layouts        []model.TypeLayout
	Keys           []Key
	Encoder        storage.KeyEncoder
	Query          *runtime.Query
	BatchRows      int
	BatchBytes     uint64
	MaxRecordBytes uint64
	RunBytes       uint64
	MaxRuns        int
	MergeFanIn     int
	WorkBytes      uint64
}

type entry struct {
	key      []byte
	position rowstore.Position
	ordinal  int64
}

type cursor struct {
	options      Options
	layouts      []model.TypeLayout
	keyLayout    []storage.KeyColumn
	input        storage.BatchCursor
	ctx          context.Context
	cancel       context.CancelFunc
	stopQuery    func() bool
	builder      *columnar.Builder
	claim        columnar.Allocation
	metadata     uint64
	entries      []entry
	keys         []byte
	row          []model.Value
	keyValues    []model.Value
	raw          *rowstore.Spool
	tape         *rowstore.Spool
	runs         []run
	runCount     uint64
	headerBytes  uint64
	header       []model.Value
	headerData   [16]byte
	merge        *merger
	memoryReader *rowstore.Reader
	memoryIndex  int
	pending      []model.Value
	initialized  bool
	closed       bool
	closeErr     error
}

func resource(message string, cause error) error {
	return &model.Error{Code: "53200", Message: message, Cause: errors.Join(runtime.ErrBudget, cause)}
}

// New takes ownership of input even when validation fails. It does not read
// input until Next. Calls to Next and Close must be serialized by the caller.
func New(ctx context.Context, input storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	c := &cursor{input: input, options: options}
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func (c *cursor) configure(ctx context.Context) error {
	o := &c.options
	missingInput := c.input == nil || o.Query == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRecordBytes < 128
	invalidRuns := o.RunBytes == 0 || o.WorkBytes == 0 || o.MergeFanIn < 2 || o.MaxRuns < o.MergeFanIn
	if missingInput || invalidBatch || invalidRuns {
		return runtime.ErrInvalidConfig
	}
	for _, n := range []uint64{o.BatchBytes, o.MaxRecordBytes, o.RunBytes, o.WorkBytes, uint64(o.MaxRuns), uint64(o.MergeFanIn)} {
		if n > uint64(math.MaxInt)/64 {
			return runtime.ErrInvalidConfig
		}
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(o.Query.Context(), c.cancel)
	if err := c.check(ctx); err != nil {
		return err
	}
	// The owned schema is admitted before copying any nested layouts.
	base := uint64(unsafe.Sizeof(cursor{})) + uint64(len(o.Layouts)+2)*(uint64(unsafe.Sizeof(model.TypeLayout{}))+2*uint64(unsafe.Sizeof(model.Value{}))) + uint64(len(o.Keys))*(uint64(unsafe.Sizeof(Key{}))+uint64(unsafe.Sizeof(storage.KeyColumn{}))+uint64(unsafe.Sizeof(model.Value{}))) + uint64(o.MaxRuns)*uint64(unsafe.Sizeof(run{})) + uint64(o.MergeFanIn)*uint64(unsafe.Sizeof(mergeHead{}))*2
	if base > o.WorkBytes {
		return resource("admit sort schema", nil)
	}
	schemaBytes := o.WorkBytes / uint64(o.MaxRuns+o.MergeFanIn+3)
	initial := base + schemaBytes + o.RunBytes*2 + o.MaxRecordBytes*9
	if initial > o.WorkBytes {
		return resource("admit sort workspace", nil)
	}
	claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(initial)
	if err != nil {
		return err
	}
	c.claim = claim
	c.layouts = make([]model.TypeLayout, len(o.Layouts)+2)
	c.layouts[0] = model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes}}
	c.layouts[1] = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	for i, layout := range o.Layouts {
		owned, size, err := model.CopyTypeLayout(ctx, layout, schemaBytes)
		if err != nil {
			return err
		}
		base += size
		schemaBytes -= size
		c.layouts[i+2] = owned
	}
	o.Layouts = c.layouts[2:]
	keys := make([]Key, len(o.Keys))
	copy(keys, o.Keys)
	o.Keys = keys
	c.keyLayout = make([]storage.KeyColumn, len(o.Keys))
	for i, key := range o.Keys {
		if key.Column < 0 || key.Column >= len(o.Layouts) {
			return storage.ErrInvalidScanPlan
		}
		typ := o.Layouts[key.Column].Type
		if key.InputCollation != 0 && builtin.TypeCollatable(typ) {
			typ.Collation = key.InputCollation
		}
		c.keyLayout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: typ, Children: o.Layouts[key.Column].Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	keyLayout, keyBytes, err := storage.CopyKeyLayout(ctx, c.keyLayout, schemaBytes)
	if err != nil {
		return err
	}
	c.keyLayout = keyLayout
	base += keyBytes
	if _, err := o.Encoder.EncodePrefix(ctx, c.keyLayout, nil, o.MaxRecordBytes); err != nil {
		return err
	}
	c.metadata = base + 4096
	// DecodeValue includes nested headers; rowstore enforces this independent
	// bound even for payloads whose decoded representation is much larger.
	spoolWork := c.metadata + o.MaxRecordBytes*9 + 8192
	fixed := base + o.RunBytes*2 + o.MaxRecordBytes*9
	if spoolWork > o.WorkBytes/uint64(o.MaxRuns+o.MergeFanIn+3) || fixed > o.WorkBytes || o.RunBytes > o.WorkBytes-fixed || uint64(o.MaxRuns+o.MergeFanIn+3)*spoolWork > o.WorkBytes-fixed-o.RunBytes {
		return resource("sort work limit cannot admit runs and merge readers", nil)
	}
	if err := claim.Resize(fixed); err != nil {
		return err
	}
	capacity := o.RunBytes / uint64(unsafe.Sizeof(entry{}))
	if capacity == 0 {
		return resource("sort run cannot admit an index entry", nil)
	}
	c.entries = make([]entry, 0, int(capacity))
	c.keys = make([]byte, 0, int(o.RunBytes))
	c.row = make([]model.Value, len(c.layouts))
	c.keyValues = make([]model.Value, len(o.Keys))
	c.runs = make([]run, 0, o.MaxRuns)
	c.header = make([]model.Value, len(c.layouts))
	for i, layout := range c.layouts {
		c.header[i] = model.NullVal(layout.Type.Storage)
	}
	c.builder, err = columnar.NewBuilder(o.Layouts, columnar.BuilderOptions{MaxRows: o.BatchRows, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.OutputMemory)})
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

func (c *cursor) newSpool(ctx context.Context, memory bool) (*rowstore.Spool, error) {
	o := c.options
	limit, memoryBytes := uint64(math.MaxInt), uint64(0)
	if memory {
		limit, memoryBytes = o.RunBytes, o.RunBytes
	}
	return rowstore.New(ctx, o.Query, c.layouts, rowstore.Limits{MemoryBytes: memoryBytes, MaxBytes: limit, MaxRows: math.MaxInt64, MaxRecordBytes: o.MaxRecordBytes, MetadataBytes: c.metadata, DecodeBytes: o.MaxRecordBytes * 8, MaxReaders: o.MergeFanIn})
}

func (c *cursor) initialize(ctx context.Context) error {
	batch := columnar.Batch{MaxRows: c.options.BatchRows}
	var ordinal int64
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		err := c.input.Next(ctx, &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := batch.Validate(max(batch.Length, c.options.BatchRows)); err != nil {
			return err
		}
		if batch.Rows() > c.options.BatchRows {
			return columnar.ErrInvalidBatch
		}
		if batch.Rows() == 0 {
			batch.Reset()
			continue
		}
		if len(batch.Columns) != len(c.options.Layouts) {
			return columnar.ErrValueLayout
		}
		for i := range batch.Columns {
			if !batch.Columns[i].MatchesLayout(c.options.Layouts[i]) {
				return columnar.ErrValueLayout
			}
		}
		for i := range batch.Rows() {
			if err := c.check(ctx); err != nil {
				return err
			}
			if ordinal == math.MaxInt64 {
				return resource("sort input ordinal exhausted", nil)
			}
			clear(c.row)
			remaining := c.options.MaxRecordBytes * 8
			for column := range batch.Columns {
				value, used, err := columnar.CopyValue(ctx, &batch.Columns[column], batch.Lane(i), remaining)
				if err != nil {
					return err
				}
				remaining -= used
				c.row[column+2] = value
			}
			for k, key := range c.options.Keys {
				c.keyValues[k] = c.row[key.Column+2]
			}
			encoded, err := c.options.Encoder.EncodePrefix(ctx, c.keyLayout, c.keyValues, min(c.options.MaxRecordBytes, c.options.RunBytes))
			if err != nil {
				if errors.Is(err, columnar.ErrBatchBudget) {
					return resource("encode sort key", err)
				}
				return err
			}
			c.row[0], c.row[1] = model.BytesVal(encoded), model.Int64Val(ordinal)
			if err := c.append(ctx, encoded, ordinal); err != nil {
				return err
			}
			ordinal++
		}
		batch.Reset()
	}
	input := c.input
	c.input = nil
	if err := input.Close(); err != nil {
		return err
	}
	clear(c.row)
	clear(c.keyValues)
	if c.tape == nil && c.raw != nil {
		if err := c.sealRaw(ctx); err != nil {
			return err
		}
		reader, err := c.raw.Reader(ctx)
		if err != nil {
			return err
		}
		c.memoryReader = reader
		return nil
	}
	if c.raw != nil {
		if err := c.flush(ctx); err != nil {
			return err
		}
	}
	if c.tape == nil {
		return nil
	}
	if err := c.tape.Seal(ctx); err != nil {
		return err
	}
	for c.runCount > uint64(c.options.MergeFanIn) {
		if err := c.compact(ctx); err != nil {
			return err
		}
	}
	if _, err := c.loadRuns(ctx, 0, int(c.runCount)); err != nil {
		return err
	}
	m, err := c.openMerge(ctx, c.runs)
	if err != nil {
		return err
	}
	c.merge = m
	return nil
}

func (c *cursor) append(ctx context.Context, key []byte, ordinal int64) error {
	if len(c.entries) == cap(c.entries) || len(key) > cap(c.keys)-len(c.keys) {
		if err := c.flush(ctx); err != nil {
			return err
		}
	}
	if c.raw == nil {
		spool, err := c.newSpool(ctx, true)
		if err != nil {
			return err
		}
		c.raw = spool
	}
	position, err := c.raw.AppendRow(ctx, c.row)
	if errors.Is(err, runtime.ErrBudget) && len(c.entries) > 0 {
		if err := c.flush(ctx); err != nil {
			return err
		}
		spool, createErr := c.newSpool(ctx, true)
		if createErr != nil {
			return createErr
		}
		c.raw = spool
		position, err = c.raw.AppendRow(ctx, c.row)
	}
	if err != nil {
		return err
	}
	start := len(c.keys)
	c.keys = append(c.keys, key...)
	c.entries = append(c.entries, entry{key: c.keys[start:], position: position, ordinal: ordinal})
	return nil
}

func (c *cursor) sealRaw(ctx context.Context) error {
	if err := c.raw.Seal(ctx); err != nil {
		return err
	}
	slices.SortFunc(c.entries, func(a, b entry) int {
		if n := bytes.Compare(a.key, b.key); n != 0 {
			return n
		}
		if a.ordinal < b.ordinal {
			return -1
		}
		if a.ordinal > b.ordinal {
			return 1
		}
		return 0
	})
	return c.check(ctx)
}

func (c *cursor) flush(ctx context.Context) (err error) {
	if c.raw == nil {
		return nil
	}
	if err := c.sealRaw(ctx); err != nil {
		return err
	}
	r, err := c.raw.Reader(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	if c.tape == nil {
		c.tape, err = c.newSpool(ctx, false)
		if err != nil {
			return err
		}
	}
	if err := c.appendHeader(ctx, c.tape, uint64(len(c.entries)), c.raw.Bytes()); err != nil {
		return err
	}
	for _, e := range c.entries {
		row, err := r.Read(ctx, e.position)
		if err != nil {
			return err
		}
		if _, err := c.tape.AppendRow(ctx, row); err != nil {
			return err
		}
	}
	if err := r.Close(); err != nil {
		return err
	}
	if err := c.raw.Close(); err != nil {
		return err
	}
	c.raw = nil
	clear(c.entries)
	c.entries = c.entries[:0]
	c.keys = c.keys[:0]
	c.runCount++
	return nil
}

func (c *cursor) nextRow(ctx context.Context) ([]model.Value, error) {
	if c.memoryReader != nil {
		if c.memoryIndex == len(c.entries) {
			return nil, io.EOF
		}
		row, err := c.memoryReader.Read(ctx, c.entries[c.memoryIndex].position)
		if err == nil {
			c.memoryIndex++
		}
		return row, err
	}
	if c.merge != nil {
		return c.merge.next(ctx)
	}
	return nil, io.EOF
}

func (c *cursor) finishNext(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	if errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, model.ErrValueBudget) || errors.Is(err, storage.ErrDecodeBudget) || errors.Is(err, io.ErrShortWrite) {
		err = resource("sort row or batch exceeds its resource limit", err)
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
		c.row, c.keyValues, c.keys = nil, nil, nil
		work := c.metadata - 4096
		if c.memoryReader == nil {
			c.entries = nil
		} else {
			for i := range c.entries {
				c.entries[i].key = nil
			}
			work += c.options.RunBytes
		}
		if err := c.claim.Resize(work); err != nil {
			return err
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
			row, err := c.nextRow(callCtx)
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			c.pending = row
		}
		err := c.builder.Append(callCtx, c.pending[2:])
		if errors.Is(err, columnar.ErrBatchBudget) && c.builder.Rows() > 0 {
			break
		}
		if err != nil {
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
	if c.input != nil {
		c.closeErr = errors.Join(c.closeErr, c.input.Close())
		c.input = nil
	}
	if c.memoryReader != nil {
		c.closeErr = errors.Join(c.closeErr, c.memoryReader.Close())
		c.memoryReader = nil
	}
	if c.merge != nil {
		c.closeErr = errors.Join(c.closeErr, c.merge.close())
		c.merge = nil
	}
	if c.raw != nil {
		c.closeErr = errors.Join(c.closeErr, c.raw.Close())
		c.raw = nil
	}
	if c.tape != nil {
		c.closeErr = errors.Join(c.closeErr, c.tape.Close())
		c.tape = nil
	}
	c.runs = nil
	c.header = nil
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
	}
	c.pending, c.row, c.keyValues, c.entries, c.keys, c.layouts, c.keyLayout = nil, nil, nil, nil, nil, nil, nil
	c.options.Layouts, c.options.Keys = nil, nil
	if c.claim != nil {
		c.claim.Release()
	}
	return c.closeErr
}
