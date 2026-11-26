package aggregate

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/zeebo/xxh3"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// PartitionedInput tags each nonempty batch until the next Next or Close.
// Partitions may recur and arrive in any order.
type PartitionedInput interface {
	storage.BatchCursor
	Partition() uint32
}

// PartitionedCursor publishes homogeneous batches. Partition is meaningful
// after a successful nonempty Next, until the next Next or Close.
type PartitionedCursor interface {
	storage.BatchCursor
	Partition() uint32
}

// Partitioning hashes ascending, NULLs-first KeyEncoder bytes with unseeded
// XXH3-64 modulo Count. KeyIndices selects unique GroupColumns ordinals in
// encoding order. Count is independent of Options.MaxPartitions.
type Partitioning struct {
	Count      uint32
	KeyIndices []int
}

type partitionedCursor struct{ cursor }

func (c *partitionedCursor) Partition() uint32 { return c.partitioned.published }

type partitionedState struct {
	input                     partitionedInput
	claim                     columnar.Allocation
	count, pending, published uint32
	indices                   []int
	layout                    []storage.KeyColumn
	values                    []model.Value
	identity                  bool
}

type partitionedInput struct {
	source        PartitionedInput
	owner         *cursor
	partition     uint32
	reuseGroupKey bool
	closed        bool
	closeErr      error
}

var errInputPartition = &model.Error{Code: "XX000", Message: "aggregate input partition does not match canonical distribution key"}

func partitionedWork(ctx context.Context, partitioning Partitioning, o Options) (uint64, error) {
	missing := o.Query == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRecordBytes < 128
	invalidWork := o.GroupBytes == 0 || o.PartitionMemoryBytes == 0 || o.WorkBytes == 0 || o.MaxPartitions < 2
	invalidKeys := len(o.GroupColumns) == 0 || len(partitioning.KeyIndices) == 0 || len(partitioning.KeyIndices) > len(o.GroupColumns) || uint64(len(o.GroupColumns)) > math.MaxUint32
	if missing || invalidBatch || invalidWork || invalidKeys || partitioning.Count < 2 {
		return 0, runtime.ErrInvalidConfig
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(o.Query.Context())); err != nil {
		return 0, err
	}
	for _, size := range [...]uint64{o.BatchBytes, o.MaxRecordBytes, o.GroupBytes, o.PartitionMemoryBytes, o.WorkBytes, uint64(o.MaxPartitions), uint64(o.BatchRows)} {
		if size > uint64(math.MaxInt)/128 {
			return 0, runtime.ErrInvalidConfig
		}
	}
	if len(o.OutputLayouts) != len(o.GroupColumns)+len(o.Calls) {
		return 0, columnar.ErrValueLayout
	}
	var used uint64
	admit := func(count, size uint64) bool {
		if size != 0 && count > (o.WorkBytes-used)/size {
			return false
		}
		used += count * size
		return true
	}
	unit := uint64(unsafe.Sizeof(int(0))) + uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(model.Value{}))
	if !admit(1, uint64(unsafe.Sizeof(partitionedState{}))+4096) || !admit(uint64(len(partitioning.KeyIndices)), unit) || !admit(uint64(len(o.GroupColumns)), uint64(unsafe.Sizeof(bool(false)))) {
		return 0, resource("admit partitioned aggregate metadata", nil)
	}
	// Validation owns at most one copied distribution tuple and its encoded key.
	// The engine's grouping key may coexist with a separately encoded subset.
	if !admit(10, o.MaxRecordBytes) {
		return 0, resource("admit partitioned aggregate key workspace", nil)
	}
	for i, column := range o.GroupColumns {
		if err := context.Cause(ctx); err != nil {
			return 0, err
		}
		if column < 0 || column >= len(o.InputLayouts) || !model.TypeLayoutsEqual(o.OutputLayouts[i], o.InputLayouts[column]) {
			return 0, columnar.ErrValueLayout
		}
		if o.InputLayouts[column].Type.Storage == model.TypeNumeric && !admit(1, 1<<20) {
			return 0, resource("admit partitioned aggregate numeric workspace", nil)
		}
	}
	identity := len(partitioning.KeyIndices) == len(o.GroupColumns)
	for i, index := range partitioning.KeyIndices {
		if err := context.Cause(ctx); err != nil {
			return 0, err
		}
		if index < 0 || index >= len(o.GroupColumns) {
			return 0, runtime.ErrInvalidConfig
		}
		identity = identity && index == i
	}
	if !identity {
		for _, index := range partitioning.KeyIndices {
			if o.InputLayouts[o.GroupColumns[index]].Type.Storage == model.TypeNumeric && !admit(1, 1<<20) {
				return 0, resource("admit partitioned aggregate numeric workspace", nil)
			}
		}
	}
	if used == o.WorkBytes {
		return 0, resource("partitioned aggregate work limit cannot admit groups", nil)
	}
	return used, nil
}

func (c *cursor) configurePartitioning(ctx context.Context, partitioning Partitioning) error {
	s := c.partitioned
	seen := make([]bool, len(c.options.GroupColumns))
	s.indices = make([]int, len(partitioning.KeyIndices))
	copy(s.indices, partitioning.KeyIndices)
	s.identity = len(s.indices) == len(c.groupKey)
	for i, index := range s.indices {
		if err := c.check(ctx); err != nil {
			return err
		}
		if seen[index] {
			return runtime.ErrInvalidConfig
		}
		seen[index] = true
		s.identity = s.identity && index == i
	}
	s.values = make([]model.Value, len(s.indices))
	if s.identity {
		s.layout = c.groupKey
	} else {
		s.layout = make([]storage.KeyColumn, len(s.indices))
		for i, index := range s.indices {
			s.layout[i] = c.groupKey[index]
			s.layout[i].ColumnID = model.ColumnID(i + 1)
		}
		if _, err := c.options.Encoder.EncodePrefix(ctx, s.layout, nil, c.options.MaxRecordBytes); err != nil {
			return err
		}
	}
	// Sort preprocessing consumes the original cursor outside inputRow. Validate
	// there before tags disappear; plain aggregation can reuse groupBytes instead.
	s.input.reuseGroupKey = !c.hasDistinct && !c.hasOrder
	return nil
}

func closePartitionedInput(input PartitionedInput) error {
	if input == nil {
		return nil
	}
	return input.Close()
}

func newPartitioned(ctx context.Context, input PartitionedInput, partitioning Partitioning, options Options) (*partitionedCursor, error) {
	work, err := partitionedWork(ctx, partitioning, options)
	if err != nil {
		if options.OwnDirectEvaluators {
			err = errors.Join(err, closeDirectEvaluators(options.Calls))
		}
		return nil, errors.Join(err, closePartitionedInput(input))
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(work)
	if err != nil {
		if options.OwnDirectEvaluators {
			err = errors.Join(err, closeDirectEvaluators(options.Calls))
		}
		return nil, errors.Join(err, closePartitionedInput(input))
	}
	options.WorkBytes -= work
	c := &partitionedCursor{cursor: cursor{options: options, sourceOrdinal: -1, orderTag: -1}}
	c.partitioned = &partitionedState{claim: claim, count: partitioning.Count}
	c.partitioned.input = partitionedInput{source: input, owner: &c.cursor}
	c.input = &c.partitioned.input
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	if err := c.configurePartitioning(ctx, partitioning); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

// CheckPartitionedSupport performs constructor-equivalent admission without
// consuming input. All temporary metadata and execution claims are released.
func CheckPartitionedSupport(ctx context.Context, partitioning Partitioning, options Options) error {
	options.OwnDirectEvaluators = false
	c, err := newPartitioned(ctx, nil, partitioning, options)
	if err != nil {
		return err
	}
	return c.Close()
}

// NewPartitioned computes complete grouped results, not partial/final states or
// zero-key global aggregation. It owns input on every outcome; Query and Encoder
// are borrowed. When OwnDirectEvaluators is set, evaluator ownership likewise
// transfers on every outcome. OrderedGroups still requires globally contiguous
// full groups.
func NewPartitioned(ctx context.Context, input PartitionedInput, partitioning Partitioning, options Options) (PartitionedCursor, error) {
	if input == nil {
		err := runtime.ErrInvalidConfig
		if options.OwnDirectEvaluators {
			err = errors.Join(err, closeDirectEvaluators(options.Calls))
		}
		return nil, err
	}
	c, err := newPartitioned(ctx, input, partitioning, options)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *partitionedState) encodePartition(ctx context.Context, encoder storage.KeyEncoder, maxBytes uint64) (uint32, error) {
	key, err := encoder.EncodePrefix(ctx, s.layout, s.values, maxBytes)
	if err != nil {
		return 0, err
	}
	if uint64(cap(key)) > maxBytes {
		return 0, resource("aggregate distribution key exceeds resource limit", nil)
	}
	return uint32(xxh3.Hash(key) % uint64(s.count)), nil
}

func (s *partitionedState) groupPartition(ctx context.Context, encoder storage.KeyEncoder, values []model.Value, maxBytes uint64) (uint32, error) {
	defer clear(s.values)
	for i, index := range s.indices {
		s.values[i] = values[index]
	}
	return s.encodePartition(ctx, encoder, maxBytes)
}

func (s *partitionedState) validateGroup(ctx context.Context, encoder storage.KeyEncoder, values []model.Value, key []byte, maxBytes uint64) error {
	var partition uint32
	if s.identity {
		partition = uint32(xxh3.Hash(key) % uint64(s.count))
	} else {
		var err error
		partition, err = s.groupPartition(ctx, encoder, values, maxBytes)
		if err != nil {
			return err
		}
	}
	if partition != s.input.partition {
		return errInputPartition
	}
	return nil
}

func (s *partitionedState) finishGroup(ctx context.Context, encoder storage.KeyEncoder, g *group, maxBytes uint64) error {
	if s.identity {
		s.pending = uint32(xxh3.HashString(g.key) % uint64(s.count))
		return nil
	}
	partition, err := s.groupPartition(ctx, encoder, g.values, maxBytes)
	if err != nil {
		return err
	}
	s.pending = partition
	return nil
}

func (p *partitionedInput) Next(ctx context.Context, out *columnar.Batch) error {
	if p.closed || p.source == nil {
		return runtime.ErrClosed
	}
	if out == nil || out.MaxRows < 0 {
		return columnar.ErrInvalidBatch
	}
	c := p.owner
	if err := c.check(ctx); err != nil {
		return err
	}
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	demand := out.MaxRows
	out.Reset()
	out.MaxRows = limit
	err := p.source.Next(ctx, out)
	out.MaxRows = demand
	if err != nil {
		return err
	}
	if err := out.Validate(max(out.Length, limit)); err != nil {
		return err
	}
	if out.Rows() > limit || len(out.Columns) != len(c.options.InputLayouts) {
		return columnar.ErrInvalidBatch
	}
	for i, layout := range c.options.InputLayouts {
		if !out.Columns[i].MatchesLayout(layout) {
			return columnar.ErrValueLayout
		}
	}
	if out.Rows() == 0 {
		return nil
	}
	s := c.partitioned
	p.partition = p.source.Partition()
	if p.partition >= s.count {
		return errInputPartition
	}
	if p.reuseGroupKey {
		return nil
	}
	defer clear(s.values)
	for row := range out.Rows() {
		if err := c.check(ctx); err != nil {
			return err
		}
		clear(s.values)
		remaining := c.options.MaxRecordBytes * 8
		for i, index := range s.indices {
			column := c.options.GroupColumns[index]
			value, used, err := columnar.CopyValue(ctx, &out.Columns[column], out.Lane(row), remaining)
			if err != nil {
				return err
			}
			s.values[i], remaining = value, remaining-used
		}
		partition, err := s.encodePartition(ctx, c.options.Encoder, c.options.MaxRecordBytes)
		if err != nil {
			return err
		}
		if partition != p.partition {
			return errInputPartition
		}
	}
	return nil
}

func (p *partitionedInput) Close() error {
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	if p.source != nil {
		p.closeErr = p.source.Close()
		p.source = nil
	}
	return p.closeErr
}
