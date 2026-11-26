package neosepia

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// The caller serializes publication and validates the fence before selecting
// its prefix. Checkpoint timestamps cannot precede a commit covered by the fence.
// Coalesced runs and compacted bases are immutable multi-commit artifacts whose
// manifests and SST rows cover up to generation.position, which CheckpointVerifier
// checks strictly against spec.Fence.LogPosition and spec.Timestamp. Accordingly,
// pinCheckpoint requires an exact generation.position == spec.Fence.LogPosition
// boundary and returns storage.ErrSnapshotNotClosed when concurrent flushes or
// compactions have folded spec.Fence.LogPosition into a newer generation so the
// caller can re-acquire a fresh CheckpointSpec.
func (set *runSet) pinCheckpoint(ctx context.Context, spec storage.SnapshotSpec) (*runPin, error) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return nil, storage.ErrClosed
	}
	generation := set.head
	for generation != nil && generation.position > spec.Fence.LogPosition {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		generation = generation.parent
	}
	if generation == nil {
		if spec.Fence.LogPosition != 0 {
			return nil, storage.ErrSnapshotNotClosed
		}
	} else if generation.position != spec.Fence.LogPosition || generation.timestamp > spec.Timestamp {
		return nil, storage.ErrSnapshotNotClosed
	}
	return set.pinLocked(ctx, generation, spec.Timestamp)
}
