package txn

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errTransactionStatement = &model.Error{Code: "22023", Message: "transaction statement requires a callback"}
	errTransactionPanic     = &model.Error{Code: "25P02", Message: "transaction statement panicked"}
)

// RequireStatement checks that ctx belongs to this transaction's synchronous
// atomic callback. Statement-scoped collaborators must propagate their failures.
func (transaction *Transaction) RequireStatement(ctx context.Context) error {
	token, ok := ctx.Value(transactionTokenKey{}).(*transactionToken)
	if !ok || token.transaction != transaction || !token.active.Load() {
		return errTransactionReentry
	}
	if transaction.statementDepth == 0 {
		return errTransactionReentry
	}
	return transaction.active(ctx)
}

func (transaction *Transaction) WithMutationResolver(ctx context.Context, extension MutationResolverExtension, run func(context.Context, MutationApplier) error) error {
	if extension == nil || run == nil {
		return errTransactionStatement
	}
	if err := transaction.RequireStatement(ctx); err != nil {
		return err
	}
	base := transaction.resolver
	resolver := extension.WithBaseResolver(base)
	if resolver == nil {
		return errTransactionOptions
	}
	transaction.resolver = resolver
	defer func() { transaction.resolver = base }()
	return run(ctx, transaction)
}

// RunSubtransaction creates a rollback boundary inside the active managed
// statement. It cannot be called from a lifecycle or concurrent context.
func (transaction *Transaction) RunSubtransaction(ctx context.Context, run func(context.Context) error) error {
	if run == nil {
		return errTransactionStatement
	}
	if err := transaction.RequireStatement(ctx); err != nil {
		return err
	}
	return transaction.statement(ctx, run)
}

// Statement runs one atomic, uncommitted operation. The callback must use its
// supplied context for synchronous transaction calls and must not share that
// context with concurrent work. Lifecycle changes and nested Statements are
// rejected. A panic aborts the entire transaction before propagating.
func (transaction *Transaction) Statement(ctx context.Context, run func(context.Context) error) error {
	if run == nil {
		return errTransactionStatement
	}
	operation, finish, nested, err := transaction.enter(ctx)
	if err != nil {
		return err
	}
	if nested {
		finish()
		return errTransactionReentry
	}
	completed := false
	defer func() {
		value := recover()
		if !completed {
			// Also covers runtime.Goexit: no callback prefix may remain committable.
			transaction.cancel(errTransactionPanic)
			_ = transaction.abortLocked(false) // Cleanup errors remain in terminalError.
		}
		terminal := transaction.State() == Aborted || transaction.State() == Committed
		finish()
		if terminal {
			<-transaction.cleanupDone
		}
		if value != nil {
			panic(value)
		}
	}()
	err = transaction.statement(operation, run)
	completed = true
	return err
}

func (transaction *Transaction) statement(ctx context.Context, run func(context.Context) error) error {
	if err := transaction.active(ctx); err != nil {
		return err
	}
	if transaction.statementDepth >= 64 {
		return errTransactionBudget
	}
	position, err := transaction.checkpoint()
	if err != nil {
		return transactionError(err)
	}
	transaction.statementDepth++
	defer func() { transaction.statementDepth-- }()
	err = errors.Join(run(ctx), transaction.active(ctx))
	if err == nil {
		return nil
	}
	return transaction.rollbackStatement(ctx, position, err)
}

func (transaction *Transaction) rollbackStatement(ctx context.Context, position transactionCheckpoint, err error) error {
	if cause := context.Cause(transaction.life); cause != nil {
		err = errors.Join(cause, err)
	}
	if transaction.State() != Active {
		if transaction.State() == Aborting {
			err = errors.Join(err, transaction.abortLocked(false))
		}
		return transactionError(err)
	}
	restoreErr := transaction.restore(context.WithoutCancel(ctx), position)
	if restoreErr != nil {
		if !errors.Is(restoreErr, errLockWounded) {
			transaction.terminalError = errors.Join(transaction.terminalError, restoreErr)
			transaction.manager.mu.Lock()
			transaction.manager.transactionCloseError = errors.Join(transaction.manager.transactionCloseError, restoreErr)
			transaction.manager.mu.Unlock()
		}
		transaction.cancel(restoreErr)
		restoreErr = errors.Join(restoreErr, transaction.abortLocked(false))
	}
	return transactionError(errors.Join(err, restoreErr))
}
