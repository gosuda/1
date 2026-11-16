package neosepia

import (
	"context"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (backend *Backend) ApplyMutations(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()
	backend.mu.Lock()
	if backend.closed {
		backend.mu.Unlock()
		return storage.ApplyResult{}, storage.ErrClosed
	}
	validated, err := validateMutationBatch(ctx, batch)
	if err != nil {
		backend.mu.Unlock()
		return storage.ApplyResult{}, err
	}
	id := recoveryTxnID(batch.TxID)
	if previous, found := backend.transactions.Get(id); found {
		backend.mu.Unlock()
		if previous.Identity != validated.identity {
			return storage.ApplyResult{}, storage.ErrTxnIDReused
		}
		return previous.Result, nil
	}
	if backend.pending != nil {
		same := backend.pending.decision.TxID == batch.TxID && backend.pending.decision.Identity != validated.identity
		backend.mu.Unlock()
		if same {
			return storage.ApplyResult{}, storage.ErrTxnIDReused
		}
		return storage.ApplyResult{}, storage.ErrCommitIndeterminate
	}
	if backend.metadataUncertain {
		backend.mu.Unlock()
		return storage.ApplyResult{}, storage.ErrCommitIndeterminate
	}
	if batch.CommitTS <= backend.closedTimestamp() || backend.frontier.LogPosition == math.MaxUint64 {
		backend.mu.Unlock()
		return storage.ApplyResult{}, storage.ErrInvalidMutation
	}
	active := backend.state.Scales[len(backend.state.Scales)-1]
	if batch.Epoch.LeapHistory != active.Scale.LeapHistoryID || batch.Epoch.Assurance == ([16]byte{}) || batch.Epoch.Config == ([32]byte{}) {
		backend.mu.Unlock()
		return storage.ApplyResult{}, clock.ErrClockEpochChanged
	}
	position := backend.frontier.LogPosition + 1
	result := storage.ApplyResult{AppliedCount: batch.Count(), CommitTS: batch.CommitTS, ChecksumCRC: batch.ChecksumCRC, LogPosition: position}
	transactions, _, err := backend.transactions.Set(id, transactionRecord{Identity: validated.identity, Result: result}, backend.limits.IdentityBytes-backend.transactions.NodeBytes())
	if err != nil {
		backend.mu.Unlock()
		return storage.ApplyResult{}, err
	}
	directory := backend.directory
	runs := backend.runs
	preparation := backend.limits.preparation()
	walBytes := backend.limits.WALBytes
	publicationBytes := backend.limits.PublicationBytes
	commitFault := backend.commitFault
	backend.mu.Unlock()
	submitted, err := commitMutationRuns(ctx, directory, runs, position, validated, preparation, walBytes, publicationBytes, commitFault)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if err != nil && !errors.Is(err, storage.ErrCommitIndeterminate) {
		return storage.ApplyResult{}, err
	}
	backend.pending = &pendingCommit{decision: submitted.Decision, epoch: batch.Epoch, transactions: transactions, admission: submitted.Admission}
	if err != nil {
		return storage.ApplyResult{}, err
	}
	if err := backend.publishCommit(ctx, submitted.Result); err != nil {
		return storage.ApplyResult{}, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	return submitted.Result, nil
}

func (backend *Backend) publishCommit(ctx context.Context, result storage.ApplyResult) error {
	pending := backend.pending
	expected, found := pending.transactions.Get(recoveryTxnID(pending.decision.TxID))
	if !found || result != expected.Result {
		return storage.ErrCorruptStorage
	}
	if backend.commitFault != nil {
		if err := backend.commitFault(commitBeforeReaderPublication); err != nil {
			return err
		}
	}
	if err := pending.admission.publish(ctx); err != nil {
		return err
	}
	backend.frontier = recoveredPosition{LogPosition: result.LogPosition, CommitTS: result.CommitTS}
	backend.publishedEpoch = pending.epoch
	backend.transactions = pending.transactions
	backend.pending = nil
	// Admission is consumed; finish decision bookkeeping before exposing a fault.
	if backend.commitFault != nil {
		return backend.commitFault(commitReaderPublished)
	}
	return nil
}

func (backend *Backend) ResolveCommit(ctx context.Context, id model.TxnID) (storage.CommitResolution, error) {
	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.closed {
		return storage.CommitResolution{}, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return storage.CommitResolution{}, err
	}
	if id == (model.TxnID{}) {
		return storage.CommitResolution{}, storage.ErrInvalidMutation
	}
	if previous, found := backend.transactions.Get(recoveryTxnID(id)); found {
		return storage.CommitResolution{State: storage.CommitCommitted, Result: previous.Result}, nil
	}
	if backend.metadataUncertain {
		if err := backend.resolveMetadata(ctx); err != nil {
			return storage.CommitResolution{}, err
		}
	}
	if backend.pending == nil || backend.pending.decision.TxID != id {
		return storage.CommitResolution{State: storage.CommitNotCommitted}, nil
	}
	resolution, _, err := resolveMutationRuns(ctx, backend.directory, backend.pending.decision, backend.limits.preparation(), backend.limits.recovery().Batch)
	if err != nil {
		return resolution, err
	}
	switch resolution.State {
	case storage.CommitNotCommitted:
		if err := backend.pending.admission.Close(); err != nil {
			return resolution, err
		}
		backend.pending = nil
	case storage.CommitCommitted:
		if err := backend.publishCommit(ctx, resolution.Result); err != nil {
			return resolution, err
		}
	}
	return resolution, nil
}
