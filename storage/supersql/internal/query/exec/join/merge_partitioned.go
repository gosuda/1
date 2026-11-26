package join

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type partitionedMergeCursor struct{ mergeCursor }

func (c *partitionedMergeCursor) Partition() uint32 { return c.partitioned.published }

func partitionedMergeWork(ctx context.Context, partitioning Partitioning, options Options, ordering []MergeOrder) (mergeWorkspace, bool, error) {
	work, err := checkMerge(ctx, options, ordering)
	if err != nil {
		return work, false, err
	}
	if partitioning.Count < 2 || uint64(partitioning.Count) > uint64(options.MaxPartitions) || len(partitioning.KeyIndices) == 0 || len(partitioning.KeyIndices) > len(options.Keys) || uint64(len(options.Keys)) > math.MaxUint32 {
		return work, false, runtime.ErrInvalidConfig
	}
	identity := len(partitioning.KeyIndices) == len(options.Keys)
	for i, index := range partitioning.KeyIndices {
		if err := context.Cause(ctx); err != nil {
			return work, false, err
		}
		if index < 0 || index >= len(options.Keys) {
			return work, false, runtime.ErrInvalidConfig
		}
		identity = identity && index == i && !ordering[index].Descending && ordering[index].NullsFirst
	}
	spool := work.metadata + options.MemoryBytes + 2*(options.MaxRecordBytes*9+8192)
	remaining := options.WorkBytes - work.fixed - spool - work.set
	admit := func(count, size uint64) bool {
		if size != 0 && count > remaining/size {
			return false
		}
		size *= count
		work.fixed += size
		remaining -= size
		return true
	}
	if !admit(1, uint64(unsafe.Sizeof(partitionedState{}))) {
		return work, false, resource("admit partitioned merge metadata", nil)
	}
	if !identity {
		unit := uint64(unsafe.Sizeof(int(0))) + uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(model.Value{}))
		if !admit(uint64(len(partitioning.KeyIndices)), unit) || !admit(uint64(len(options.Keys)), uint64(unsafe.Sizeof(bool(false)))) {
			return work, false, resource("admit partitioned merge key mapping", nil)
		}
		// Ordered and canonical distribution encodings coexist only when their
		// key sequences or ordering differ. No workspace scales with Count.
		if !admit(1, options.MaxRecordBytes) {
			return work, false, resource("admit partitioned merge key workspace", nil)
		}
		for _, index := range partitioning.KeyIndices {
			if err := context.Cause(ctx); err != nil {
				return work, false, err
			}
			if options.LeftLayouts[options.Keys[index].Left].Type.Storage == model.TypeNumeric && !admit(1, 1<<20) {
				return work, false, resource("admit partitioned merge numeric workspace", nil)
			}
		}
	}
	return work, identity, nil
}

// CheckPartitionedMergeSupport checks the canonical partition contract and the
// streaming workspace bound without consuming inputs. Its metadata claim is
// released before return. Inputs must be globally ordered by all merge keys.
func CheckPartitionedMergeSupport(ctx context.Context, partitioning Partitioning, options Options, ordering []MergeOrder) (err error) {
	work, identity, err := partitionedMergeWork(ctx, partitioning, options, ordering)
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
	return c.configurePartitioning(ctx, partitioning, identity)
}

// NewPartitionedMerge owns both inputs on every outcome; other collaborators are
// borrowed as in New. Inputs must be globally ordered by all merge keys, not
// merely ordered within each partition. It retains one right equality group
// with the same spool and ordinal-membership bounds as NewMerge. Output batches
// retain their originating partition, including unmatched rows of full joins.
func NewPartitionedMerge(ctx context.Context, left, right PartitionedInput, partitioning Partitioning, options Options, ordering []MergeOrder) (PartitionedCursor, error) {
	c := &partitionedMergeCursor{mergeCursor: mergeCursor{cursor: cursor{left: left, right: right, options: options}}}
	if left == nil || right == nil {
		return nil, errors.Join(runtime.ErrInvalidConfig, c.Close())
	}
	work, identity, err := partitionedMergeWork(ctx, partitioning, options, ordering)
	if err != nil {
		return nil, errors.Join(resourceCause(err), c.Close())
	}
	if err = c.configureMerge(ctx, work, ordering); err == nil {
		err = c.configurePartitioning(ctx, partitioning, identity)
	}
	if err != nil {
		return nil, errors.Join(resourceCause(err), c.Close())
	}
	return c, nil
}
