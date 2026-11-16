package txn

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type TableLockMode uint8

const (
	AccessShareTableLock TableLockMode = iota + 1
	RowShareTableLock
	RowExclusiveTableLock
	ShareUpdateExclusiveTableLock
	ShareTableLock
	ShareRowExclusiveTableLock
	ExclusiveTableLock
	AccessExclusiveTableLock
)

type TableLockRequest struct {
	Target storage.ObjectRef
	Mode   TableLockMode
}

// LockTables acquires the complete relation set atomically in stable identity
// order. The locks are retained by the transaction and follow its savepoints.
func (transaction *Transaction) LockTables(ctx context.Context, requests []TableLockRequest, nowait bool) error {
	operation, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if err := transaction.active(operation); err != nil {
		return err
	}
	if transaction.mode != ReadWrite || transaction.owner == nil {
		return ErrReadOnlyTransaction
	}
	if len(requests) == 0 {
		return errTransactionOptions
	}
	owned := slices.Clone(requests)
	for _, request := range owned {
		if err := request.Target.Validate(); err != nil {
			return err
		}
		if request.Mode < AccessShareTableLock || request.Mode > AccessExclusiveTableLock {
			return errTransactionOptions
		}
	}
	slices.SortFunc(owned, func(a, b TableLockRequest) int {
		if order := cmp.Compare(a.Target.ObjectID, b.Target.ObjectID); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Target.Generation, b.Target.Generation); order != 0 {
			return order
		}
		return cmp.Compare(a.Mode, b.Mode)
	})
	for index := 1; index < len(owned); index++ {
		if owned[index-1].Target.ObjectID == owned[index].Target.ObjectID {
			return errTransactionOptions
		}
	}
	position, err := transaction.manager.locks.savepoint(transaction.owner)
	if err != nil {
		return transactionError(err)
	}
	policy := lockWait
	if nowait {
		policy = lockNoWait
	}
	for _, request := range owned {
		mode := tableLockMode(request.Mode)
		all := lockRequest{ObjectID: request.Target.ObjectID, Generation: request.Target.Generation, Lower: lockBound{Unbounded: true}, Upper: lockBound{Unbounded: true}}
		if _, err := transaction.manager.locks.acquire(operation, transaction.owner, all, mode, policy); err != nil {
			return transactionError(errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position)))
		}
	}
	if err := transaction.active(operation); err != nil {
		return transactionError(errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position)))
	}
	return nil
}

func tableLockMode(mode TableLockMode) lockMode {
	switch mode {
	case AccessShareTableLock:
		return lockIntent
	case RowShareTableLock:
		return lockTableRowShare
	case RowExclusiveTableLock:
		return lockTableRowExclusive
	case ShareUpdateExclusiveTableLock:
		return lockTableShareUpdateExclusive
	case ShareTableLock:
		return lockTableShare
	case ShareRowExclusiveTableLock:
		return lockTableShareRowExclusive
	case ExclusiveTableLock:
		return lockTableExclusive
	default:
		return lockSchema
	}
}

func (transaction *Transaction) lockTableIntentWithPolicy(ctx context.Context, target storage.ObjectRef, mode lockMode, policy lockWaitPolicy) (bool, error) {
	if err := target.Validate(); err != nil {
		return false, err
	}
	all := lockRequest{ObjectID: target.ObjectID, Generation: target.Generation, Lower: lockBound{Unbounded: true}, Upper: lockBound{Unbounded: true}}
	acquired, err := transaction.manager.locks.acquire(ctx, transaction.owner, all, mode, policy)
	return acquired, transactionError(err)
}
