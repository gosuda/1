package txn

import (
	"context"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var ErrReadOnlyTransaction = &model.Error{Code: "25006", Message: "cannot write in a read-only transaction"}

var errCommitSequenceExhausted = &model.Error{Code: "54000", Message: "database commit sequence exhausted"}

type managerCommit struct {
	id       model.TxnID
	epoch    clock.Epoch
	expected storage.ApplyResult
	// catalog marks a commit whose batch rewrote catalog rows, so publishing it
	// advances the manager's catalog content version.
	catalog bool
}

type managerSubmission struct {
	Result storage.ApplyResult
	State  storage.CommitState
}

func (manager *Manager) commitUnresolved() bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.pending != nil
}

// submit is the transaction owner's only durable write path. The caller retains
// its immutable normalized buffers until return; asynchronous backends must own
// any references still required after an indeterminate return. catalogScope
// reports whether the submitting transaction staged any change to the catalog
// collections; only such a commit can change a snapshot-derived catalog result.
func (manager *Manager) submit(ctx context.Context, batch storage.MutationBatch, catalogScope bool) (managerSubmission, error) {
	outcome := managerSubmission{State: storage.CommitNotCommitted}
	operation, finish, err := manager.operation(ctx)
	if err != nil {
		return outcome, err
	}
	defer finish()
	if manager.historicalOnly {
		return outcome, ErrReadOnlyTransaction
	}
	if batch.Count() == 0 {
		return outcome, storage.ErrBatchEmpty
	}
	reservedInput := batch.CommitTS != 0 || batch.Epoch != (clock.Epoch{}) || batch.ChecksumCRC != 0
	if batch.TxID == (model.TxnID{}) || reservedInput {
		return outcome, storage.ErrInvalidMutation
	}
	if err := manager.sequencer.lock(operation); err != nil {
		return outcome, err
	}
	defer manager.sequencer.unlock()
	if manager.commitUnresolved() {
		return outcome, storage.ErrCommitIndeterminate
	}
	manager.mu.Lock()
	position := manager.frontier.LogPosition
	manager.mu.Unlock()
	if position == math.MaxUint64 {
		return outcome, errCommitSequenceExhausted
	}
	err = manager.sequencer.fenceLocked(operation, 0, func(ctx context.Context, timestamp model.Timestamp, epoch clock.Epoch) error {
		batch.CommitTS, batch.Epoch = timestamp, epoch
		checksum, err := batch.ComputeChecksum(ctx)
		if err != nil {
			return err
		}
		batch.ChecksumCRC = checksum
		if err := manager.sequencer.checkFence(ctx, timestamp, epoch); err != nil {
			return err
		}
		pending := &managerCommit{id: batch.TxID, epoch: epoch, expected: storage.ApplyResult{AppliedCount: batch.Count(), CommitTS: timestamp, ChecksumCRC: checksum, LogPosition: position + 1}, catalog: catalogScope}
		manager.mu.Lock()
		manager.pending = pending
		manager.pendingInFlight = true
		manager.mu.Unlock()
		outcome.State = storage.CommitUnknown
		result, err := manager.backend.ApplyMutations(ctx, batch)
		manager.mu.Lock()
		manager.pendingInFlight = false
		manager.mu.Unlock()
		if err == nil {
			if err := manager.publishResolved(pending, result); err != nil {
				return err
			}
			outcome = managerSubmission{Result: result, State: storage.CommitCommitted}
			return nil
		}
		if errors.Is(err, storage.ErrTxnIDReused) {
			manager.clearPending(pending)
			outcome.State = storage.CommitNotCommitted
			return err
		}
		resolution, resolveErr := manager.resolvePending(ctx, pending)
		if resolveErr != nil || resolution.State == storage.CommitUnknown {
			return errors.Join(storage.ErrCommitIndeterminate, err, resolveErr)
		}
		outcome.State = resolution.State
		if resolution.State == storage.CommitCommitted {
			outcome.Result = resolution.Result
			return nil
		}
		return err
	})
	if err != nil && outcome.State == storage.CommitUnknown {
		err = errors.Join(storage.ErrCommitIndeterminate, err)
	}
	return outcome, err
}

// ResolveCommit performs no clock read. Already-durable decisions remain
// resolvable during clock loss; later reservations remain fenced until publication.
func (manager *Manager) ResolveCommit(ctx context.Context, id model.TxnID) (storage.CommitResolution, error) {
	operation, finish, err := manager.operation(ctx)
	if err != nil {
		return storage.CommitResolution{}, err
	}
	defer finish()
	if id == (model.TxnID{}) {
		return storage.CommitResolution{}, storage.ErrInvalidMutation
	}
	if manager.sequencer != nil {
		if err := manager.sequencer.lock(operation); err != nil {
			return storage.CommitResolution{}, err
		}
		defer manager.sequencer.unlock()
	}
	manager.mu.Lock()
	pending := manager.pending
	manager.mu.Unlock()
	if pending == nil || pending.id != id {
		return manager.backend.ResolveCommit(operation, id)
	}
	return manager.resolvePending(operation, pending)
}

// The sequencer gate serializes both commit submission and resolution.
func (manager *Manager) resolvePending(ctx context.Context, pending *managerCommit) (storage.CommitResolution, error) {
	resolution, err := manager.backend.ResolveCommit(ctx, pending.id)
	if err != nil {
		return resolution, err
	}
	switch resolution.State {
	case storage.CommitNotCommitted:
		manager.clearPending(pending)
	case storage.CommitCommitted:
		if err := manager.publishResolved(pending, resolution.Result); err != nil {
			return storage.CommitResolution{State: storage.CommitUnknown}, err
		}
	case storage.CommitUnknown:
	default:
		return storage.CommitResolution{State: storage.CommitUnknown}, storage.ErrCorruptStorage
	}
	return resolution, nil
}

func (manager *Manager) clearPending(pending *managerCommit) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.pending == pending {
		manager.pending = nil
		manager.pendingInFlight = false
	}
}

// publishResolved advances the durable frontier for one resolved commit.
//
// catalogVersion is the content identity every live snapshot-derived catalog
// result is keyed by, so it MUST advance for every commit that changed catalog
// rows and MUST NOT advance for one that did not. The rule is decided from the
// transaction's own staged mutations (see Overlay.CatalogVersion), never from
// the commit's declared layouts: a statement declares read dependencies over
// the catalog collections for ordinary queries, so layouts cannot distinguish a
// read from a write.
//
// Every path that rewrites catalog rows funnels through
// Transaction.ApplyMutations -> applyOne -> Overlay.StageOwned, so the staged
// counter is complete: foreground DDL (CREATE/ALTER/DROP TYPE, table and index
// declarations), REFRESH/populate descriptor updates, statistics and
// index-job descriptor publications, native-catalog bootstrap, sequence and
// identity records, and retention policy rows all stage through it. The
// background maintenance loop publishes base manifests and retires WAL without
// touching catalog rows, so it correctly does not bump. Recovery and replay run
// before any Manager serves a statement, and only one live Manager may own a
// directory (the storage lock), so no second writer can change catalog rows
// behind this counter. An aborted transaction never reaches this call.
//
// When the classification is ever in doubt the counter over-invalidates rather
// than under-invalidates: a savepoint restore that discards catalog changes
// still leaves the overlay reporting that it staged one, so the version
// advances and the next statement rebuilds.
func (manager *Manager) publishResolved(pending *managerCommit, result storage.ApplyResult) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.pending != pending || result != pending.expected {
		return storage.ErrCorruptStorage
	}
	manager.frontier.LogPosition = result.LogPosition
	manager.frontier.CommitTS = result.CommitTS
	manager.frontier.ClosedTS = max(manager.frontier.ClosedTS, result.CommitTS)
	manager.frontier.Epoch = pending.epoch
	if pending.catalog {
		manager.catalogVersion++
	}
	manager.pending = nil
	manager.pendingInFlight = false
	return nil
}
