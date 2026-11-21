package neosepia

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// coveredFenceIndex caches the contiguous commit timestamps covered by the
// active durable base in log-position order so validateCoveredFence runs in
// O(1) without scanning the transaction tree under set.mu.
type coveredFenceIndex struct {
	prunedPosition uint64
	prunedCommitTS model.Timestamp
	// timestamps[i] is the CommitTS for LogPosition == prunedPosition + 1 + i.
	timestamps []model.Timestamp
}

func (set *runSet) validateFence(ctx context.Context, fence storage.ReadFence, covered coveredFenceIndex) error {
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
		if generation.baseGeneration != 0 {
			if generation.position == fence.LogPosition {
				if generation.timestamp > fence.ClosedTS {
					return storage.ErrSnapshotNotClosed
				}
				return nil
			}
			if generation.timestamp <= fence.ClosedTS {
				return storage.ErrSnapshotNotClosed
			}
			return validateCoveredFence(fence, covered)
		}
		firstPos := generation.firstPosition
		if firstPos == 0 {
			firstPos = generation.position
		}
		if fence.LogPosition >= firstPos {
			var atTS model.Timestamp
			if len(generation.commitTSs) == int(generation.position-firstPos+1) {
				atTS = generation.commitTSs[fence.LogPosition-firstPos]
			} else {
				atTS = generation.timestamp
			}
			if atTS > fence.ClosedTS {
				return storage.ErrSnapshotNotClosed
			}
			if fence.LogPosition < generation.position {
				nextTS := generation.commitTSs[fence.LogPosition-firstPos+1]
				if nextTS <= fence.ClosedTS {
					return storage.ErrSnapshotNotClosed
				}
			}
			return nil
		}
		firstTS := generation.timestamp
		if len(generation.commitTSs) > 0 {
			firstTS = generation.commitTSs[0]
		}
		if firstTS <= fence.ClosedTS {
			return storage.ErrSnapshotNotClosed
		}
		expected = firstPos - 1
	}
	if fence.LogPosition != 0 || expected != 0 {
		return storage.ErrSnapshotNotClosed
	}
	return nil
}

// Current fences take the generation fast path. An older fence inside a compacted
// base checks its adjacent durable decisions in O(1) via coveredFenceIndex; for
// positions older than prunedPosition, it enforces that ClosedTS strictly precedes
// the retired prefix's high-water commit timestamp.
func validateCoveredFence(fence storage.ReadFence, covered coveredFenceIndex) error {
	if len(covered.timestamps) == 0 {
		return storage.ErrSnapshotNotClosed
	}
	basePos := covered.prunedPosition + uint64(len(covered.timestamps))
	if fence.LogPosition >= basePos {
		return storage.ErrSnapshotNotClosed
	}
	if fence.LogPosition >= covered.prunedPosition {
		var prevTS model.Timestamp
		if fence.LogPosition == covered.prunedPosition {
			prevTS = covered.prunedCommitTS
		} else {
			prevTS = covered.timestamps[fence.LogPosition-covered.prunedPosition-1]
		}
		if prevTS > fence.ClosedTS {
			return storage.ErrSnapshotNotClosed
		}
		nextTS := covered.timestamps[fence.LogPosition-covered.prunedPosition]
		if nextTS <= fence.ClosedTS {
			return storage.ErrSnapshotNotClosed
		}
		return nil
	}
	// fence.LogPosition < covered.prunedPosition => LogPosition+1 <= prunedPosition,
	// so commitTS(LogPosition+1) <= prunedCommitTS. If ClosedTS >= prunedCommitTS,
	// the fence omitted at least one commit at or before ClosedTS.
	if fence.ClosedTS >= covered.prunedCommitTS {
		return storage.ErrSnapshotNotClosed
	}
	return nil
}
