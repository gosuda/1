package join

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/zeebo/xxh3"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// PartitionedInput tags every nonempty batch with its zero-based partition.
// Partition is meaningful after a successful Next until the next Next or Close.
// Batches may arrive in any partition order, including repeated partitions.
type PartitionedInput interface {
	storage.BatchCursor
	Partition() uint32
}

// PartitionedCursor publishes batches containing exactly one logical partition.
// Partition returns the last successfully published nonempty batch's tag.
type PartitionedCursor interface {
	storage.BatchCursor
	Partition() uint32
}

// Partitioning uses unseeded XXH3-64 of the complete ascending, NULLs-first SQL
// key modulo Count. KeyIndices selects unique Options.Keys ordinals in encoding
// order, independently of each key's NULL matching policy.
type Partitioning struct {
	Count      uint32
	KeyIndices []int
}

type partitionedState struct {
	count, input, published uint32
	indices                 []int
	layout                  []storage.KeyColumn
	values                  []model.Value
}

type partitionedCursor struct{ cursor }

func (c *partitionedCursor) Partition() uint32 { return c.partitioned.published }

var errInputPartition = &model.Error{Code: "XX000", Message: "join input partition does not match canonical distribution key"}

type partitionedWorkspace struct {
	base, fixed, metadata, hash uint64
	identity                    bool
}

func partitionedWork(ctx context.Context, partitioning Partitioning, o Options) (partitionedWorkspace, error) {
	var work partitionedWorkspace
	missing := o.Query == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRecordBytes < 128
	invalidWork := o.HashBytes == 0 || o.WorkBytes == 0 || o.PartitionBytes == 0
	invalidCount := o.MaxPartitions <= 0 || partitioning.Count < 2 || uint64(partitioning.Count) > uint64(o.MaxPartitions)
	if missing || invalidBatch || invalidWork || invalidCount || o.MaxDepth <= 0 {
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
	invalidKeys := len(o.Keys) == 0 || uint64(len(o.Keys)) > math.MaxUint32
	invalidPartitionKeys := len(partitioning.KeyIndices) == 0 || len(partitioning.KeyIndices) > len(o.Keys)
	if o.MarkKind != 0 || invalidKeys || invalidPartitionKeys || len(o.Output) != len(o.OutputLayouts) {
		return work, runtime.ErrInvalidConfig
	}
	for _, size := range [...]uint64{o.BatchBytes, o.MaxRecordBytes, o.MemoryBytes, o.HashBytes, o.WorkBytes, o.PartitionBytes, uint64(o.MaxPartitions), uint64(o.BatchRows)} {
		if size > uint64(math.MaxInt)/128 {
			return work, runtime.ErrInvalidConfig
		}
	}
	work.identity = len(partitioning.KeyIndices) == len(o.Keys)
	for i, index := range partitioning.KeyIndices {
		if err := context.Cause(ctx); err != nil {
			return work, err
		}
		if index < 0 || index >= len(o.Keys) {
			return work, runtime.ErrInvalidConfig
		}
		work.identity = work.identity && index == i
	}
	var used uint64
	admit := func(count, size uint64) bool {
		if size != 0 && count > (o.WorkBytes-used)/size {
			return false
		}
		used += count * size
		return true
	}
	if !admit(1, uint64(unsafe.Sizeof(partitionedCursor{}))+uint64(unsafe.Sizeof(partitionedState{}))+8192) {
		return work, resource("admit partitioned join metadata", nil)
	}
	for _, count := range [...]int{len(o.LeftLayouts), len(o.RightLayouts), len(o.OutputLayouts), len(o.ResidualInputs), len(o.Keys), 8} {
		if !admit(uint64(count), 512) {
			return work, resource("admit partitioned join metadata", nil)
		}
	}
	if !admit(uint64(partitioning.Count), 64) {
		return work, resource("admit partitioned join metadata", nil)
	}
	if !work.identity {
		unit := uint64(unsafe.Sizeof(int(0))) + uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(model.Value{}))
		if !admit(uint64(len(partitioning.KeyIndices)), unit) || !admit(uint64(len(o.Keys)), uint64(unsafe.Sizeof(bool(false)))) {
			return work, resource("admit partitioned join key mapping", nil)
		}
	}
	work.base = used
	if !admit(1, work.base) {
		return work, resource("admit partitioned join layout copies", nil)
	}
	if o.Residual != nil && !admit(1, o.Residual.OwnedBytes()) {
		return work, resource("admit partitioned join residual metadata", nil)
	}
	// Full and distribution keys coexist with copied rows. Numeric preparation
	// has a separate bound from the encoded byte count.
	if !admit(20, o.MaxRecordBytes) {
		return work, resource("admit partitioned join row workspace", nil)
	}
	for _, key := range o.Keys {
		if err := context.Cause(ctx); err != nil {
			return work, err
		}
		if key.Left < 0 || key.Left >= len(o.LeftLayouts) || key.Right < 0 || key.Right >= len(o.RightLayouts) {
			return work, runtime.ErrInvalidConfig
		}
		if o.LeftLayouts[key.Left].Type.Storage == model.TypeNumeric && !admit(1, 1<<20) {
			return work, resource("admit partitioned join numeric workspace", nil)
		}
	}
	if !work.identity {
		for _, index := range partitioning.KeyIndices {
			if o.LeftLayouts[o.Keys[index].Left].Type.Storage == model.TypeNumeric && !admit(1, 1<<20) {
				return work, resource("admit partitioned join numeric workspace", nil)
			}
		}
	}
	work.fixed = used
	work.metadata = work.base + 4096
	spool := work.metadata + o.MaxRecordBytes*9 + 8192
	if !admit(2*uint64(partitioning.Count)+3, spool) || !admit(1, o.MemoryBytes) {
		return work, resource("admit partitioned join spool workspace", nil)
	}
	work.hash = min(o.HashBytes, o.WorkBytes-used)
	return work, nil
}

func (c *cursor) configurePartitioning(ctx context.Context, partitioning Partitioning, identity bool) error {
	state := &partitionedState{count: partitioning.Count}
	c.partitioned = state
	if identity {
		return nil
	}
	seen := make([]bool, len(c.options.Keys))
	for _, index := range partitioning.KeyIndices {
		if err := c.check(ctx); err != nil {
			return err
		}
		if seen[index] {
			return runtime.ErrInvalidConfig
		}
		seen[index] = true
	}
	state.indices = make([]int, len(partitioning.KeyIndices))
	copy(state.indices, partitioning.KeyIndices)
	state.layout = make([]storage.KeyColumn, len(state.indices))
	state.values = make([]model.Value, len(state.indices))
	for i, index := range state.indices {
		state.layout[i] = c.keyLayout[index]
		state.layout[i].ColumnID = model.ColumnID(i + 1)
		state.layout[i].Descending = false
		state.layout[i].NullsFirst = true
	}
	_, err := c.options.Encoder.EncodePrefix(ctx, state.layout, nil, c.options.MaxRecordBytes)
	return err
}

// CheckPartitionedSupport checks the exact partition contract and work bound
// without consuming inputs. Its temporary metadata claim is released on return.
func CheckPartitionedSupport(ctx context.Context, partitioning Partitioning, options Options) (err error) {
	work, err := partitionedWork(ctx, partitioning, options)
	if err != nil {
		return resourceCause(err)
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(work.fixed)
	if err != nil {
		return err
	}
	c := cursor{options: options, claim: claim, ctx: ctx}
	defer func() { err = errors.Join(resourceCause(err), c.Close()) }()
	if err = c.configureSchema(ctx, work.base); err != nil {
		return err
	}
	if _, err = c.residualLayouts(); err != nil {
		return err
	}
	return c.configurePartitioning(ctx, partitioning, work.identity)
}

func closePartitionedInputs(left, right PartitionedInput) error {
	var err error
	if left != nil {
		err = left.Close()
	}
	if right != nil {
		err = errors.Join(err, right.Close())
	}
	return err
}

// NewPartitioned owns both inputs on every outcome; other collaborators are
// borrowed as in New. Count is never resized. Input tags are checked against
// every selected row before its partition spool accepts it. Output visits
// logical partitions in ascending order, skipping partitions with no result.
func NewPartitioned(ctx context.Context, left, right PartitionedInput, partitioning Partitioning, options Options) (PartitionedCursor, error) {
	if left == nil || right == nil {
		return nil, errors.Join(runtime.ErrInvalidConfig, closePartitionedInputs(left, right))
	}
	work, err := partitionedWork(ctx, partitioning, options)
	if err != nil {
		return nil, errors.Join(resourceCause(err), closePartitionedInputs(left, right))
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(work.fixed)
	if err != nil {
		return nil, errors.Join(err, closePartitionedInputs(left, right))
	}
	c := &partitionedCursor{cursor: cursor{left: left, right: right, options: options, claim: claim, metadata: work.metadata, hashBytes: work.hash}}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(options.Query.Context(), c.cancel)
	if err = c.check(ctx); err == nil {
		err = c.configureRows(ctx, work.base)
	}
	if err == nil {
		err = c.configurePartitioning(ctx, partitioning, work.identity)
	}
	if err != nil {
		return nil, errors.Join(resourceCause(err), c.Close())
	}
	c.partitions = make([]partition, int(partitioning.Count))
	c.row = make([]model.Value, max(len(c.leftSchema), len(c.rightSchema)))
	return c, nil
}

func (s *partitionedState) validate(ctx context.Context, encoder storage.KeyEncoder, values []model.Value, encoded []byte, maxBytes uint64) error {
	if uint64(cap(encoded)) > maxBytes {
		return resource("join equality key exceeds resource limit", nil)
	}
	if len(s.indices) != 0 {
		defer clear(s.values)
		for i, index := range s.indices {
			s.values[i] = values[index]
		}
		var err error
		encoded, err = encoder.EncodePrefix(ctx, s.layout, s.values, maxBytes)
		if err != nil {
			return err
		}
		if uint64(cap(encoded)) > maxBytes {
			return resource("join distribution key exceeds resource limit", nil)
		}
	}
	if uint32(xxh3.Hash(encoded)%uint64(s.count)) != s.input {
		return errInputPartition
	}
	return nil
}
