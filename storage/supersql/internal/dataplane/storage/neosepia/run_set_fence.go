package neosepia

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

func (set *runSet) validateFence(ctx context.Context, fence storage.ReadFence, covered ordered.Tree[recoveryTxnID, transactionRecord]) error {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := (storage.SnapshotSpec{Timestamp: fence.ClosedTS, Fence: fence}).Validate(); err != nil {
		return err
	}
	if set.head == nil {
		if fence.LogPosition != 0 {
			return storage.ErrSnapshotNotClosed
		}
		return nil
	}
	expected := set.head.position
	if fence.LogPosition > expected {
		return storage.ErrSnapshotNotClosed
	}
	for generation := set.head; generation != nil; generation = generation.parent {
		if err := ctx.Err(); err != nil {
			return err
		}
		if generation.position != expected {
			return storage.ErrSnapshotNotClosed
		}
		if generation.position == fence.LogPosition {
			if generation.timestamp > fence.ClosedTS {
				return storage.ErrSnapshotNotClosed
			}
			return nil
		}
		if generation.timestamp <= fence.ClosedTS {
			return storage.ErrSnapshotNotClosed
		}
		if generation.baseGeneration != 0 {
			return validateCoveredFence(ctx, fence, covered)
		}
		expected--
	}
	if fence.LogPosition != 0 || expected != 0 {
		return storage.ErrSnapshotNotClosed
	}
	return nil
}

// Current fences take the generation fast path. An older fence inside a compacted
// base needs the adjacent durable decisions, not the base's newest commit time.
func validateCoveredFence(ctx context.Context, fence storage.ReadFence, covered ordered.Tree[recoveryTxnID, transactionRecord]) error {
	previousFound, nextFound := fence.LogPosition == 0, false
	cursor := covered.First(false)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, record, ok := cursor.Next()
		if !ok {
			return storage.ErrSnapshotNotClosed
		}
		switch record.Result.LogPosition {
		case fence.LogPosition:
			if record.Result.CommitTS > fence.ClosedTS {
				return storage.ErrSnapshotNotClosed
			}
			previousFound = true
		case fence.LogPosition + 1:
			if record.Result.CommitTS <= fence.ClosedTS {
				return storage.ErrSnapshotNotClosed
			}
			nextFound = true
		}
		if previousFound && nextFound {
			return nil
		}
	}
}
