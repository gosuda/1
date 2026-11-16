package neosepia

import (
	"context"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

var _ storage.SnapshotObserver = (*snapshotReader)(nil)

// ObserveSnapshot measures physical work that can be attributed exactly to the
// reader's pinned snapshot. An overlay makes version-history counts unavailable:
// its logical rows remain visible to ANALYZE, but it has no durable file history.
func (reader *snapshotReader) ObserveSnapshot(ctx context.Context, target storage.ObjectRef) (observation storage.SnapshotObservation, err error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return observation, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	if err := target.Validate(); err != nil {
		return observation, err
	}
	observation.Target = target
	observation.Snapshot = reader.spec.Timestamp
	observation.Blocks.Known = true
	observation.Files.Known = true
	observation.DeltaBytes.Known = true
	observation.VersionRows.Known = reader.spec.Overlay == nil
	err = reader.pin.visit(ctx, target, func(file *sstFile) (visitErr error) {
		fileObserved := false
		for index := range file.count {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry := file.entry(index)
			if entry.minCommitTS > reader.spec.Timestamp {
				continue
			}
			block, err := file.ReadBlock(ctx, index, reader.limits.BlockBytes, nil)
			if err != nil {
				return err
			}
			blockObserved := false
			for row := range block.Rows() {
				_, _, timestamp, _, kind, rowErr := block.Version(row)
				if rowErr != nil {
					visitErr = errors.Join(visitErr, rowErr)
					break
				}
				if timestamp > reader.spec.Timestamp {
					continue
				}
				blockObserved = true
				if observation.VersionRows.Known && kind != storage.MutationDeleteRange {
					if observation.VersionRows.Value == math.MaxUint64 {
						visitErr = errors.Join(visitErr, storage.ErrCorruptStorage)
						break
					}
					observation.VersionRows.Value++
				}
			}
			visitErr = errors.Join(visitErr, block.Close())
			if visitErr != nil {
				return visitErr
			}
			if !blockObserved {
				continue
			}
			fileObserved = true
			if observation.Blocks.Value == math.MaxUint64 || entry.length > math.MaxUint64-observation.DeltaBytes.Value && file.tier == activeTier {
				return storage.ErrCorruptStorage
			}
			observation.Blocks.Value++
			if file.tier == activeTier {
				observation.DeltaBytes.Value += entry.length
			}
		}
		if fileObserved {
			if observation.Files.Value == math.MaxUint64 {
				return storage.ErrCorruptStorage
			}
			observation.Files.Value++
		}
		return nil
	})
	if err != nil {
		return storage.SnapshotObservation{}, err
	}
	return observation, ctx.Err()
}
