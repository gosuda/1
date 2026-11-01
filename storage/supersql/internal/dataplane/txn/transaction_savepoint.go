package txn

import (
	"context"
	"errors"
	"unsafe"
)

type transactionCheckpoint struct {
	log             mutationLogPosition
	overlay         overlayCheckpoint
	locks           lockPosition
	mode            TransactionMode
	targets, marks  int
	metadata, views uint64
	ordinal         uint64
}

type transactionMark struct {
	transaction *Transaction
	checkpoint  transactionCheckpoint
	valid       bool
}

// Savepoint is an opaque retained-branch mark. Copying it preserves identity.
type Savepoint struct{ mark *transactionMark }

func (transaction *Transaction) checkpoint() (transactionCheckpoint, error) {
	position := transactionCheckpoint{
		mode: transaction.mode, targets: len(transaction.targets), marks: len(transaction.marks),
		metadata: transaction.metadataUsed, views: transaction.viewSerial, ordinal: transaction.ordinal,
	}
	if transaction.owner == nil {
		return position, nil
	}
	var err error
	if transaction.log != nil {
		position.log, err = transaction.log.savepoint()
		if err != nil {
			return transactionCheckpoint{}, err
		}
		position.overlay, err = transaction.overlay.checkpoint()
		if err != nil {
			return transactionCheckpoint{}, err
		}
	}
	position.locks, err = transaction.manager.locks.savepoint(transaction.owner)
	return position, err
}

func (transaction *Transaction) restore(ctx context.Context, position transactionCheckpoint) error {
	if transaction.log != nil {
		// Validate the log's retained branch before publishing an older overlay root.
		if err := transaction.log.rollback(ctx, position.log); err != nil {
			return err
		}
	}
	err := transaction.closeViews(position.views)
	transaction.publications.rollback(position.ordinal)
	if transaction.log != nil {
		err = errors.Join(err, transaction.overlay.restore(position.overlay))
	}
	if err != nil {
		return err
	}
	for _, target := range transaction.targets[position.targets:] {
		transaction.metadataUsed -= target.bytes
	}
	clear(transaction.targets[position.targets:])
	transaction.targets = transaction.targets[:position.targets]
	for _, mark := range transaction.marks[position.marks:] {
		mark.valid = false
		mark.checkpoint = transactionCheckpoint{}
	}
	clear(transaction.marks[position.marks:])
	transaction.marks = transaction.marks[:position.marks]
	// Slice capacity and caller-held invalid savepoint handles remain charged.
	if transaction.owner != nil {
		if err := transaction.manager.locks.rollback(transaction.owner, position.locks); err != nil {
			return err
		}
	}
	transaction.mode = position.mode
	return nil
}

func (transaction *Transaction) Savepoint(ctx context.Context) (Savepoint, error) {
	operation, finish, nested, err := transaction.enter(ctx)
	if err != nil {
		return Savepoint{}, err
	}
	defer finish()
	if nested {
		return Savepoint{}, errTransactionReentry
	}
	if err := transaction.active(operation); err != nil {
		return Savepoint{}, err
	}
	charge := uint64(unsafe.Sizeof(transactionMark{})) + 2*uint64(unsafe.Sizeof((*transactionMark)(nil)))
	if charge > transaction.metadataLimit-transaction.metadataUsed {
		return Savepoint{}, errTransactionBudget
	}
	position, err := transaction.checkpoint()
	if err != nil {
		return Savepoint{}, transactionError(err)
	}
	mark := &transactionMark{transaction: transaction, checkpoint: position, valid: true}
	transaction.marks = append(transaction.marks, mark)
	mark.checkpoint.marks = len(transaction.marks)
	transaction.metadataUsed += charge
	return Savepoint{mark: mark}, nil
}

func (transaction *Transaction) RollbackTo(ctx context.Context, savepoint Savepoint) error {
	operation, finish, nested, err := transaction.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if nested {
		return errTransactionReentry
	}
	if err := transaction.active(operation); err != nil {
		return err
	}
	mark := savepoint.mark
	if mark == nil || mark.transaction != transaction || !mark.valid {
		return errTransactionSavepoint
	}
	if err := transaction.restore(operation, mark.checkpoint); err != nil {
		if errors.Is(err, errMutationLogPosition) {
			return errors.Join(errTransactionSavepoint, err)
		}
		transaction.cancel(err)
		return transactionError(errors.Join(err, transaction.abortLocked(false)))
	}
	return nil
}

// ReleaseSavepoint invalidates this mark and its descendants without changing
// mutations, row visibility, or held locks. Older marks remain usable.
func (transaction *Transaction) ReleaseSavepoint(ctx context.Context, savepoint Savepoint) error {
	operation, finish, nested, err := transaction.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if nested {
		return errTransactionReentry
	}
	if err := transaction.active(operation); err != nil {
		return err
	}
	mark := savepoint.mark
	if mark == nil || mark.transaction != transaction || !mark.valid {
		return errTransactionSavepoint
	}
	index := -1
	for i, candidate := range transaction.marks {
		if candidate == mark {
			index = i
			break
		}
	}
	if index < 0 {
		return errTransactionSavepoint
	}
	for _, released := range transaction.marks[index:] {
		released.valid = false
		released.checkpoint = transactionCheckpoint{}
	}
	clear(transaction.marks[index:])
	transaction.marks = transaction.marks[:index]
	return nil
}
