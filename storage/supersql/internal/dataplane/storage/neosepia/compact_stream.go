package neosepia

import (
	"context"
	"errors"
	"io"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Source ownership transfers on every return. The writer remains caller-owned;
// only nonempty successful output may be finished and durably published. Grouping
// retains at most one additional source block beyond the merge's current heads.
func appendRetainedRun(ctx context.Context, writer *sstWriter, source *mvccCursor, limits blockLimits) (uint64, error) {
	if writer == nil {
		if source != nil {
			return 0, errors.Join(storage.ErrInvalidScanPlan, source.Close())
		}
		return 0, storage.ErrInvalidScanPlan
	}
	return appendRetainedBlocks(ctx, source, limits, func(ctx context.Context, target storage.ObjectRef, data []byte, _ uint64) error {
		if target != writer.target {
			return storage.ErrInvalidTarget
		}
		return writer.Append(ctx, data)
	})
}

func appendRetainedBlocks(ctx context.Context, source *mvccCursor, limits blockLimits, appendBlock func(context.Context, storage.ObjectRef, []byte, uint64) error) (rows uint64, err error) {
	var lease *blockLease
	defer func() {
		if lease != nil {
			err = errors.Join(err, lease.release())
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
	var order []uint32
	limits.MaxDecodedBytes -= uint64(limits.MaxRows) * 4
	var emit func([]uint32) error
	emit = func(selected []uint32) error {
		encoded, err := rewriteBlock(ctx, lease.block, selected, limits)
		if errors.Is(err, columnar.ErrBatchBudget) && len(selected) > 1 {
			middle := len(selected) / 2
			if err := emit(selected[:middle]); err != nil {
				return err
			}
			return emit(selected[middle:])
		}
		if err != nil {
			return err
		}
		count := uint64(len(selected))
		if err := appendBlock(ctx, lease.block.Target(), encoded, count); err != nil {
			return err
		}
		rows += count
		return nil
	}
	flush := func() error {
		if lease == nil {
			return nil
		}
		if err := emit(order); err != nil {
			return err
		}
		releaseErr := lease.release()
		lease = nil
		order = order[:0]
		return releaseErr
	}
	for {
		row, nextErr := source.Next(ctx)
		if nextErr == io.EOF {
			if err := flush(); err != nil {
				return 0, err
			}
			return rows, nil
		}
		if nextErr != nil {
			return 0, nextErr
		}
		if lease != row.lease || len(order) == cap(order) {
			if err := flush(); err != nil {
				return 0, err
			}
		}
		if lease == nil {
			lease = row.lease.retain()
		}
		if order == nil {
			order = make([]uint32, 0, limits.MaxRows)
		}
		order = append(order, uint32(row.row))
	}
}
