package neosepia

import (
	"context"
	"errors"
	"io"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Source ownership transfers on every return. The writer remains caller-owned;
// only nonempty successful output may be finished and durably published.
func appendRetainedRun(ctx context.Context, writer *sstWriter, source *mvccCursor, limits blockLimits) (uint64, error) {
	if writer == nil {
		if source != nil {
			return 0, errors.Join(storage.ErrInvalidScanPlan, source.Close())
		}
		return 0, storage.ErrInvalidScanPlan
	}
	return appendRetainedBlocksWithBlobs(ctx, source, limits, func(ctx context.Context, target storage.ObjectRef, data []byte, blobs []toastBlob, _ uint64) error {
		if target != writer.target {
			return storage.ErrInvalidTarget
		}
		return writer.AppendWithBlobs(ctx, data, blobs)
	})
}

func appendRetainedBlocks(ctx context.Context, source *mvccCursor, limits blockLimits, appendBlock func(context.Context, storage.ObjectRef, []byte, uint64) error) (rows uint64, err error) {
	if appendBlock == nil {
		if source != nil {
			return 0, errors.Join(storage.ErrInvalidScanPlan, source.Close())
		}
		return 0, storage.ErrInvalidScanPlan
	}
	return appendRetainedBlocksWithBlobs(ctx, source, limits, func(ctx context.Context, target storage.ObjectRef, data []byte, _ []toastBlob, count uint64) error {
		return appendBlock(ctx, target, data, count)
	})
}

func appendRetainedBlocksWithBlobs(ctx context.Context, source *mvccCursor, limits blockLimits, appendBlock func(context.Context, storage.ObjectRef, []byte, []toastBlob, uint64) error) (rows uint64, err error) {
	var coalescer *paxBlockCoalescer
	defer func() {
		if coalescer != nil {
			err = errors.Join(err, coalescer.Close())
		}
		if source != nil {
			err = errors.Join(err, source.Close())
		}
	}()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if source == nil || !source.retainFuture || appendBlock == nil {
		return 0, storage.ErrInvalidScanPlan
	}
	if source.closed {
		return 0, storage.ErrClosed
	}
	if limits.MaxRows <= 0 || limits.MaxDecodedBytes > uint64(math.MaxInt) {
		return 0, columnar.ErrBatchBudget
	}
	if uint64(limits.MaxRows) >= limits.MaxDecodedBytes/4 {
		return 0, columnar.ErrBatchBudget
	}
	coalescer = newPAXBlockCoalescer(limits, limits.MaxDecodedBytes/2)
	emit := func(target storage.ObjectRef, data []byte, blobs []toastBlob, count uint64, _ model.Timestamp) error {
		if err := appendBlock(ctx, target, data, blobs, count); err != nil {
			return err
		}
		rows += count
		return nil
	}
	for {
		row, nextErr := source.Next(ctx)
		if nextErr == io.EOF {
			if err := coalescer.flush(ctx, emit); err != nil {
				return 0, err
			}
			return rows, nil
		}
		if nextErr != nil {
			return 0, nextErr
		}
		if err := coalescer.appendRow(ctx, row.lease.block, row.row, row.key, row.end, row.timestamp, row.ordinal, row.kind, 0, emit); err != nil {
			return 0, err
		}
	}
}
