package txn

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type RowLockStrength uint8

const (
	KeyShareLock RowLockStrength = iota
	ShareLock
	NoKeyUpdateLock
	UpdateLock
)

type RowLockWait uint8

const (
	WaitForRowLock RowLockWait = iota
	NoWaitForRowLock
	SkipLockedRow
)

// RowLockRequest identifies one point lock in an atomic lock set. Key is
// borrowed for the duration of LockRows.
type RowLockRequest struct {
	Target   storage.ObjectRef
	Key      []byte
	Strength RowLockStrength
}

// LockingMutationApplier supplies explicit write intent before conflict probes.
type LockingMutationApplier interface {
	MutationApplier
	LockRow(context.Context, storage.ObjectRef, []byte, RowLockStrength, RowLockWait) (bool, error)
}

// LockRow does not read a row or upgrade a read-only transaction. A false result
// with SkipLockedRow excludes that candidate from the caller's write input.
func (transaction *Transaction) LockRow(ctx context.Context, target storage.ObjectRef, key []byte, strength RowLockStrength, wait RowLockWait) (bool, error) {
	request := [1]RowLockRequest{{Target: target, Key: key, Strength: strength}}
	return transaction.LockRows(ctx, request[:], wait)
}

// LockRows acquires a point-lock set atomically. When acquisition fails or a
// SKIP LOCKED member is unavailable, locks and intents acquired by this call are
// rolled back before returning.
func (transaction *Transaction) LockRows(ctx context.Context, requests []RowLockRequest, wait RowLockWait) (bool, error) {
	operation, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return false, err
	}
	defer finish()
	if err := transaction.active(operation); err != nil {
		return false, err
	}
	if transaction.mode != ReadWrite {
		return false, ErrReadOnlyTransaction
	}
	if len(requests) == 0 || wait > SkipLockedRow {
		return false, errTransactionOptions
	}
	for _, request := range requests {
		if request.Strength > UpdateLock {
			return false, errTransactionOptions
		}
		if err := request.Target.Validate(); err != nil {
			return false, err
		}
	}
	position, err := transaction.manager.locks.savepoint(transaction.owner)
	if err != nil {
		return false, transactionError(err)
	}
	policy := lockWait
	switch wait {
	case NoWaitForRowLock:
		policy = lockNoWait
	case SkipLockedRow:
		policy = lockSkip
	}
	for _, request := range requests {
		acquired, err := transaction.lockTableIntentWithPolicy(operation, request.Target, lockTableRowShare, policy)
		if err != nil || !acquired {
			return false, transactionError(errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position)))
		}
		mode := rowLockMode(request.Strength)
		acquired, err = transaction.manager.locks.acquire(operation, transaction.owner, pointLock(request.Target, request.Key), mode, policy)
		if err != nil || !acquired {
			return false, transactionError(errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position)))
		}
	}
	if err := transaction.active(operation); err != nil {
		return false, transactionError(errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position)))
	}
	return true, nil
}

func rowLockMode(strength RowLockStrength) lockMode {
	switch strength {
	case ShareLock:
		return lockShare
	case NoKeyUpdateLock:
		return lockNoKeyUpdate
	case UpdateLock:
		return lockUpdate
	default:
		return lockKeyShare
	}
}
