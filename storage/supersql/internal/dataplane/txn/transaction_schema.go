package txn

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// LockSchema drains or wounds conflicting writers across all generations of the
// logical object. The barrier lasts through this transaction's terminal cleanup.
// Publish descriptor/policy changes in this same transaction; future resolvers
// must select the current write policy after obtaining their schema intent lock.
func (transaction *Transaction) LockSchema(ctx context.Context, target storage.ObjectRef) error {
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
	if transaction.mode != ReadWrite {
		return ErrReadOnlyTransaction
	}
	if err := target.Validate(); err != nil {
		return err
	}
	request := lockRequest{ObjectID: target.ObjectID, Generation: target.Generation, Lower: lockBound{Unbounded: true}, Upper: lockBound{Unbounded: true}}
	_, err = transaction.manager.locks.acquire(operation, transaction.owner, request, lockSchema, lockWait)
	return transactionError(err)
}
