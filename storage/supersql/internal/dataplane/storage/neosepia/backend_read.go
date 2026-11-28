package neosepia

import (
	"context"
	"errors"
	"os"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func (backend *Backend) OpenSnapshot(ctx context.Context, spec storage.SnapshotSpec) (storage.SnapshotReader, error) {
	backend.mu.Lock()
	needsCommitLock := !backend.closed && (spec.Fence.ClosedTS > backend.closedTimestamp() || backend.metadataUncertain || backend.state.Closed.ClosedTS == 0 || backend.stateFault != nil)
	if needsCommitLock {
		backend.mu.Unlock()
		backend.commitMu.Lock()
		defer backend.commitMu.Unlock()
		backend.mu.Lock()
	}
	defer backend.mu.Unlock()
	if err := backend.prepareRead(ctx, spec); err != nil {
		return nil, err
	}
	pin, err := backend.runs.pin(ctx)
	if err != nil {
		return nil, err
	}
	return newSnapshotReader(ctx, pin, spec, backend.encoder, backend.limits.reads())
}

// The caller holds backend.mu through pin acquisition.
func (backend *Backend) prepareRead(ctx context.Context, spec storage.SnapshotSpec) error {
	if backend.closed {
		return storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := backend.verifier.VerifyReadFence(ctx, spec.Fence); err != nil {
		return err
	}
	if backend.metadataUncertain && spec.Fence.ClosedTS > backend.state.Closed.ClosedTS {
		if err := backend.resolveMetadata(ctx); err != nil {
			return errors.Join(storage.ErrCommitIndeterminate, err)
		}
	}
	if spec.Timestamp < max(backend.state.HistoryFloor, backend.collectionBoundary) {
		return storage.ErrSnapshotTooOld
	}
	if !backend.knownEpoch(spec.Fence.Epoch) {
		return storage.ErrSnapshotNotClosed
	}
	if err := backend.runs.validateFence(ctx, spec.Fence, backend.transactions); err != nil {
		return err
	}
	if backend.pending != nil && spec.Fence.ClosedTS > backend.closedTimestamp() {
		return storage.ErrCommitIndeterminate
	}
	if spec.Fence.ClosedTS > backend.state.Closed.ClosedTS {
		active := backend.state.Scales[len(backend.state.Scales)-1].Scale.LeapHistoryID
		if spec.Fence.ClosedTS > backend.closedTimestamp() && spec.Fence.Epoch.LeapHistory != active {
			return clock.ErrClockEpochChanged
		}
		if backend.state.Generation == ^uint64(0) {
			return storage.ErrCorruptStorage
		}
		if backend.state.Closed.ClosedTS == 0 || backend.stateFault != nil {
			next := backend.state
			next.Generation++
			next.Closed = spec.Fence
			uncertain, err := writeDatabaseState(ctx, backend.directory.root, next, backend.limits.MetadataBytes, backend.stateFault)
			if err != nil {
				backend.metadataUncertain = uncertain
				if uncertain {
					return errors.Join(storage.ErrCommitIndeterminate, err)
				}
				return err
			}
			backend.state = next
			backend.durableClosed = next.Closed
			backend.stateDirty = false
		} else {
			backend.state.Closed = spec.Fence
			backend.stateDirty = true
		}
	}
	return nil
}

func (backend *Backend) knownEpoch(epoch clock.Epoch) bool {
	if epoch.Assurance == ([16]byte{}) || epoch.Config == ([32]byte{}) {
		return false
	}
	for _, version := range backend.state.Scales {
		if version.Scale.LeapHistoryID == epoch.LeapHistory {
			return true
		}
	}
	return false
}

func (backend *Backend) resolveMetadata(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := syncCommitDirectory(backend.directory.root); err != nil {
		return err
	}
	state, err := readDatabaseState(ctx, backend.directory.root, backend.limits.MetadataBytes)
	if err != nil {
		return err
	}
	previous := backend.state
	if state.Generation < previous.Generation || state.BaseGeneration < previous.BaseGeneration || state.Closed.ClosedTS < backend.durableClosed.ClosedTS || state.HistoryFloor < previous.HistoryFloor || len(state.Scales) < len(previous.Scales) {
		return storage.ErrCorruptStorage
	}
	for i, old := range previous.Scales {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := state.Scales[i]
		if next.Position != old.Position || next.Timestamp != old.Timestamp || next.Scale.LeapHistoryID != old.Scale.LeapHistoryID {
			return storage.ErrCorruptStorage
		}
	}
	for _, added := range state.Scales[len(previous.Scales):] {
		if added.Position != backend.frontier.LogPosition || added.Timestamp < backend.closedTimestamp() {
			return storage.ErrCorruptStorage
		}
	}
	if state.Closed.ClosedTS != 0 {
		if err := backend.verifier.VerifyReadFence(ctx, state.Closed); err != nil {
			return err
		}
	}
	if err := backend.validateState(ctx, state); err != nil {
		return err
	}
	var recoveredBase runBase
	if state.BaseGeneration != previous.BaseGeneration {
		base, err := loadRunBase(ctx, backend.directory.runs, state.BaseGeneration, backend.limits.MetadataBytes, backend.limits.IdentityBytes, backend.limits.MaxFiles)
		if err != nil {
			return err
		}
		if base.Floor != state.HistoryFloor || base.Position != backend.frontier.LogPosition || base.CommitTS != backend.frontier.CommitTS ||
			base.Epoch != backend.publishedEpoch || !sameTransactionRecords(base.Transactions, backend.transactions) {
			return storage.ErrCorruptStorage
		}
		files, err := openBaseFiles(ctx, backend.directory.runs, base.Files, backend.limits.preparation().File, backend.limits.ResidentBytes)
		if err != nil {
			return err
		}
		if err := backend.runs.installRecoveredBase(context.WithoutCancel(ctx), base, files); err != nil {
			return err
		}
		recoveredBase = base
	}
	if err := backend.directory.root.Remove(databaseStatePending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncCommitDirectory(backend.directory.root); err != nil {
		return err
	}
	backend.durableClosed = state.Closed
	if state.Closed.ClosedTS < previous.Closed.ClosedTS {
		state.Closed = previous.Closed
		backend.stateDirty = true
	} else {
		backend.stateDirty = false
	}
	backend.state = state
	backend.metadataUncertain = false
	if len(backend.maintenanceGate) == 0 {
		backend.collectionBoundary = 0
	}
	if recoveredBase.Generation != 0 {
		return backend.cleanupSupersededRunMetadata(ctx, recoveredBase, previous.BaseGeneration)
	}
	return backend.retireRecoveredWAL(ctx)
}
