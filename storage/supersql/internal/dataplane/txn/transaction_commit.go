package txn

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (transaction *Transaction) Commit(ctx context.Context) (storage.ApplyResult, error) {
	operation, finish, nested, err := transaction.enter(ctx)
	if err != nil {
		return storage.ApplyResult{}, err
	}
	if nested {
		finish()
		return storage.ApplyResult{}, errTransactionReentry
	}
	result, err := transaction.commitLocked(operation)
	terminal := transaction.State() == Committed || transaction.State() == Aborted
	finish()
	if terminal {
		<-transaction.cleanupDone
	}
	return result, transactionError(err)
}

func (transaction *Transaction) commitLocked(ctx context.Context) (storage.ApplyResult, error) {
	if transaction.State() == Committed {
		return transaction.result, transaction.terminalError
	}
	if transaction.State() == Indeterminate {
		return storage.ApplyResult{}, errors.Join(storage.ErrCommitIndeterminate, transaction.terminalError)
	}
	if err := transaction.active(ctx); err != nil {
		return storage.ApplyResult{}, err
	}
	if err := transaction.closeViews(0); err != nil {
		return storage.ApplyResult{}, errors.Join(err, transaction.abortLocked(false))
	}
	if !transaction.state.CompareAndSwap(uint32(Active), uint32(Committing)) {
		return storage.ApplyResult{}, transaction.active(ctx)
	}
	transaction.publications.owner.Decide(model.PublicationPending)
	if transaction.mode != ReadWrite || transaction.log.Count() == 0 {
		transaction.setState(Committed)
		return transaction.result, transaction.cleanupLocked()
	}
	if err := transaction.manager.locks.beginCommit(transaction.owner); err != nil {
		return storage.ApplyResult{}, errors.Join(err, transaction.abortLocked(false))
	}
	sequence, err := transaction.log.seal(ctx)
	if err != nil {
		return storage.ApplyResult{}, errors.Join(err, transaction.abortLocked(false))
	}
	layouts := make([]storage.ObjectLayout, len(transaction.targets))
	for i, target := range transaction.targets {
		layouts[i] = target.layout
	}
	slices.SortFunc(layouts, func(a, b storage.ObjectLayout) int {
		return (overlayKey{target: a.Target}).Compare(overlayKey{target: b.Target})
	})
	submission, err := transaction.manager.submit(ctx, storage.MutationBatch{TxID: transaction.id, Layouts: layouts, Sequence: sequence, EncodingVersion: storage.MutationEncodingVersion}, transaction.overlay.CatalogVersion() != 0)
	if err != nil {
		if submission.State == storage.CommitUnknown {
			transaction.setState(Indeterminate)
			transaction.terminalError = errors.Join(storage.ErrCommitIndeterminate, err)
			return storage.ApplyResult{}, transaction.terminalError
		}
		return storage.ApplyResult{}, errors.Join(err, transaction.abortLocked(false))
	}
	transaction.result = submission.Result
	transaction.setState(Committed)
	return submission.Result, transaction.cleanupLocked()
}

func (transaction *Transaction) Resolve(ctx context.Context) (storage.CommitResolution, error) {
	operation, finish, nested, err := transaction.enter(ctx)
	if err != nil {
		return storage.CommitResolution{}, err
	}
	if nested {
		finish()
		return storage.CommitResolution{}, errTransactionReentry
	}
	resolution, err := transaction.resolveLocked(operation)
	terminal := transaction.State() == Committed || transaction.State() == Aborted
	finish()
	if terminal {
		<-transaction.cleanupDone
	}
	return resolution, transactionError(err)
}

func (transaction *Transaction) resolveLocked(ctx context.Context) (storage.CommitResolution, error) {
	switch transaction.State() {
	case Committed:
		return storage.CommitResolution{State: storage.CommitCommitted, Result: transaction.result}, transaction.terminalError
	case Aborted:
		return storage.CommitResolution{State: storage.CommitNotCommitted}, transaction.terminalError
	case Indeterminate:
	default:
		return storage.CommitResolution{}, errTransactionInactive
	}
	resolution, err := transaction.manager.ResolveCommit(ctx, transaction.id)
	if err != nil {
		return resolution, err
	}
	if resolution.State == storage.CommitUnknown {
		return resolution, storage.ErrCommitIndeterminate
	}
	transaction.terminalError = nil
	if resolution.State == storage.CommitCommitted {
		transaction.result = resolution.Result
		transaction.setState(Committed)
		return resolution, transaction.cleanupLocked()
	}
	if resolution.State != storage.CommitNotCommitted {
		return storage.CommitResolution{}, storage.ErrCorruptStorage
	}
	return resolution, transaction.abortLocked(true)
}
