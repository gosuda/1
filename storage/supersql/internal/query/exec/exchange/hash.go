package exchange

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"unsafe"

	"github.com/zeebo/xxh3"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// HashFunction identifies the canonical-key-to-partition mapping. Changing it
// requires a new identity rather than silently changing co-location semantics.
type HashFunction uint64

// XXH3V1 hashes the complete ascending, NULLs-first encoded key with unseeded
// XXH3-64, then takes its unsigned remainder modulo the partition count.
const XXH3V1 HashFunction = 1

type HashKey struct {
	Column         int
	InputCollation model.CollationID
}

type HashOptions struct {
	Sources     Options
	Keys        []HashKey
	Encoder     storage.KeyEncoder
	Function    HashFunction
	Partitions  uint32
	MaxKeyBytes uint64
	WorkBytes   uint64
}

// PartitionBatch contains rows for exactly one zero-based partition. The batch
// is borrowed until the next Next or Close; MaxRows is caller-supplied demand.
// Partition is meaningful only after a successful Next with nonempty rows.
type PartitionBatch struct {
	Batch     columnar.Batch
	Partition uint32
}

// Repartition multiplexes bounded partition views without flattening away their
// identities. A downstream dispatcher must consume each view before Next. There
// are no independently drainable, unbounded per-partition mailboxes.
type Repartition struct {
	mu                           sync.Mutex
	sources                      *Sources
	options                      HashOptions
	keyLayout                    []storage.KeyColumn
	values                       []model.Value
	batch                        columnar.Batch
	output                       []columnar.Vector
	routes, indices              []uint32
	starts, counts               []int
	input, inputCount, partition int
	position                     int
	claim                        columnar.Allocation
	closed                       bool
	closeErr                     error
}

func hashWork(ctx context.Context, inputCount int, o HashOptions) (uint64, error) {
	if err := CheckSupport(ctx, inputCount, o.Sources); err != nil {
		return 0, err
	}
	invalidKeys := o.Encoder == nil || len(o.Keys) == 0 || uint64(len(o.Keys)) > math.MaxUint32
	invalidPartitioning := o.Function != XXH3V1 || o.Partitions < 2
	invalidWork := o.MaxKeyBytes == 0 || o.WorkBytes == 0 || o.MaxKeyBytes > uint64(math.MaxInt) || o.WorkBytes > uint64(math.MaxInt)
	if invalidKeys || invalidPartitioning || invalidWork {
		return 0, runtime.ErrInvalidConfig
	}
	var used uint64
	for _, allocation := range [...]struct{ count, bytes uint64 }{
		{1, uint64(unsafe.Sizeof(Repartition{}))},
		{uint64(len(o.Keys)), uint64(unsafe.Sizeof(HashKey{})) + uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(model.Value{}))},
		{uint64(len(o.Sources.Layouts)), 2 * uint64(unsafe.Sizeof(columnar.Vector{}))},
		{uint64(o.Sources.BatchRows), 2 * uint64(unsafe.Sizeof(uint32(0)))},
		{uint64(o.Partitions)*2 + 1, uint64(unsafe.Sizeof(int(0)))},
		{2, o.MaxKeyBytes},
	} {
		if !addSourceBytes(&used, allocation.count, allocation.bytes, o.WorkBytes) {
			return 0, exchangeBudget("admit hash exchange workspace", nil)
		}
	}
	for _, key := range o.Keys {
		if err := context.Cause(ctx); err != nil {
			return 0, err
		}
		if key.Column < 0 || key.Column >= len(o.Sources.Layouts) {
			return 0, storage.ErrInvalidScanPlan
		}
		layout := o.Sources.Layouts[key.Column]
		keyBytes, err := storage.KeyLayoutBytes(ctx, []storage.KeyColumn{{Type: layout.Type, Children: layout.Children}}, o.WorkBytes-used)
		if err != nil {
			return 0, err
		}
		used += keyBytes
		if o.Sources.Layouts[key.Column].Type.ID == 1700 && !addSourceBytes(&used, 1, 1<<20, o.WorkBytes) {
			return 0, exchangeBudget("admit hash exchange numeric workspace", nil)
		}
	}
	return used, nil
}

func hashKeyLayout(o HashOptions) []storage.KeyColumn {
	layout := make([]storage.KeyColumn, len(o.Keys))
	for i, key := range o.Keys {
		typ := o.Sources.Layouts[key.Column].Type
		if key.InputCollation != 0 {
			typ.Collation = key.InputCollation
		}
		// Hash equality treats NULLs as one class, independently of join matching.
		layout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: typ, Children: o.Sources.Layouts[key.Column].Children, NullsFirst: true}
	}
	return layout
}

// CheckHashSupport validates the declared mapping and bounded workspace without
// consuming inputs. Encoder metadata scratch is admitted before validation.
func CheckHashSupport(ctx context.Context, inputCount int, options HashOptions) error {
	work, err := hashWork(ctx, inputCount, options)
	if err != nil {
		return err
	}
	claim, err := options.Sources.Query.Allocator(runtime.WorkMemory).Reserve(work)
	if err != nil {
		return err
	}
	defer claim.Release()
	_, err = options.Encoder.EncodePrefix(ctx, hashKeyLayout(options), nil, options.MaxKeyBytes)
	return err
}

// NewHashRepartition owns all inputs on every outcome. Query and Encoder remain
// borrowed. It retains one source batch and bounded row-index buckets, not a
// relation-sized partition map or per-row objects.
func NewHashRepartition(ctx context.Context, inputs []storage.BatchCursor, options HashOptions) (*Repartition, error) {
	work, err := hashWork(ctx, len(inputs), options)
	if err != nil {
		return nil, errors.Join(err, closeSourceInputs(inputs))
	}
	claim, err := options.Sources.Query.Allocator(runtime.WorkMemory).Reserve(work)
	if err != nil {
		return nil, errors.Join(err, closeSourceInputs(inputs))
	}
	c := &Repartition{options: options, claim: claim, inputCount: len(inputs), partition: int(options.Partitions)}
	c.keyLayout, _, err = storage.CopyKeyLayout(ctx, hashKeyLayout(options), work)
	if err != nil {
		return nil, errors.Join(err, closeSourceInputs(inputs), c.Close())
	}
	if _, err := options.Encoder.EncodePrefix(ctx, c.keyLayout, nil, options.MaxKeyBytes); err != nil {
		return nil, errors.Join(err, closeSourceInputs(inputs), c.Close())
	}
	c.options.Keys = make([]HashKey, len(options.Keys))
	copy(c.options.Keys, options.Keys)
	c.values = make([]model.Value, len(options.Keys))
	c.routes = make([]uint32, options.Sources.BatchRows)
	c.indices = make([]uint32, options.Sources.BatchRows)
	c.starts = make([]int, int(options.Partitions)+1)
	c.counts = make([]int, int(options.Partitions))
	c.batch.Columns = make([]columnar.Vector, 0, len(options.Sources.Layouts))
	c.output = make([]columnar.Vector, len(options.Sources.Layouts))
	c.sources, err = NewSources(ctx, inputs, options.Sources)
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	c.options.Sources.Layouts = nil
	return c, nil
}

func (c *Repartition) route(ctx context.Context) error {
	if uint64(c.batch.Length) > math.MaxUint32 || c.batch.Rows() > len(c.routes) {
		return columnar.ErrInvalidBatch
	}
	clear(c.counts)
	defer clear(c.values)
	for row := range c.batch.Rows() {
		if err := errors.Join(context.Cause(ctx), context.Cause(c.sources.ctx)); err != nil {
			return err
		}
		clear(c.values)
		remaining := c.options.MaxKeyBytes
		for i, key := range c.options.Keys {
			value, bytes, err := columnar.CopyValue(ctx, &c.batch.Columns[key.Column], c.batch.Lane(row), remaining)
			if err != nil {
				return exchangeCopyError(err)
			}
			remaining -= bytes
			c.values[i] = value
		}
		key, err := c.options.Encoder.EncodePrefix(ctx, c.keyLayout, c.values, c.options.MaxKeyBytes)
		if err != nil {
			return err
		}
		if uint64(cap(key)) > c.options.MaxKeyBytes {
			return exchangeBudget("hash exchange key exceeds bound", nil)
		}
		partition := uint32(xxh3.Hash(key) % uint64(c.options.Partitions))
		c.routes[row] = partition
		c.counts[partition]++
	}
	c.starts[0] = 0
	for i, count := range c.counts {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		c.starts[i+1] = c.starts[i] + count
	}
	clear(c.counts)
	for row := range c.batch.Rows() {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		partition := c.routes[row]
		c.indices[c.starts[partition]+c.counts[partition]] = uint32(c.batch.Lane(row))
		c.counts[partition]++
	}
	c.partition, c.position = 0, 0
	return nil
}

func (c *Repartition) next(ctx context.Context, out *PartitionBatch) error {
	if c.closed {
		return errors.Join(runtime.ErrClosed, c.closeErr)
	}
	if out == nil || out.Batch.MaxRows < 0 {
		return runtime.ErrInvalidConfig
	}
	out.Batch.Reset()
	out.Partition = 0
	demand := c.options.Sources.BatchRows
	if out.Batch.MaxRows > 0 {
		demand = min(demand, out.Batch.MaxRows)
	}
	for {
		if err := errors.Join(context.Cause(ctx), context.Cause(c.sources.ctx)); err != nil {
			return err
		}
		for c.partition < int(c.options.Partitions) {
			if c.position < c.starts[c.partition+1] {
				end := min(c.position+demand, c.starts[c.partition+1])
				copy(c.output, c.batch.Columns)
				out.Batch.Columns = c.output
				out.Batch.Length = c.batch.Length
				out.Batch.Selection = c.indices[c.position:end]
				out.Partition = uint32(c.partition)
				c.position = end
				return nil
			}
			c.partition++
		}
		if c.input == c.inputCount {
			return io.EOF
		}
		c.batch.Reset()
		c.batch.MaxRows = demand
		err := c.sources.Next(ctx, c.input, &c.batch)
		if err == io.EOF {
			c.input++
			continue
		}
		if err != nil {
			return err
		}
		if err := c.route(ctx); err != nil {
			return err
		}
	}
}

func (c *Repartition) Next(ctx context.Context, out *PartitionBatch) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.next(ctx, out)
	if err == io.EOF {
		if cleanup := c.closeLocked(); cleanup != nil {
			return cleanup
		}
		return io.EOF
	}
	if err != nil {
		return errors.Join(err, c.closeLocked())
	}
	return nil
}

func (c *Repartition) closeLocked() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	if c.sources != nil {
		c.closeErr = c.sources.Close()
	}
	c.batch = columnar.Batch{}
	c.output = nil
	c.routes, c.indices, c.starts, c.counts = nil, nil, nil, nil
	c.keyLayout, c.values, c.options.Keys, c.options.Sources.Layouts = nil, nil, nil, nil
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}

func (c *Repartition) Close() error {
	if c.sources != nil {
		c.sources.cancel(runtime.ErrClosed)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}
