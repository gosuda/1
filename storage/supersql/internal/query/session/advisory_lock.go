package session

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type sessionAdvisoryRuntime struct {
	manager *locks.AdvisoryLockManager
	owner   locks.AdvisoryOwner
}

func (runtime sessionAdvisoryRuntime) AcquireAdvisory(ctx context.Context, request model.AdvisoryLockRequest) (bool, error) {
	if request.Wait {
		if err := runtime.manager.Lock(ctx, runtime.owner, request.Key, request.Mode, request.Scope); err != nil {
			return false, err
		}
		return true, nil
	}
	return runtime.manager.TryLock(runtime.owner, request.Key, request.Mode, request.Scope)
}

func (runtime sessionAdvisoryRuntime) UnlockAdvisory(ctx context.Context, key model.AdvisoryKey, mode model.AdvisoryLockMode) (bool, error) {
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	return runtime.manager.Unlock(runtime.owner, key, mode), nil
}

func (runtime sessionAdvisoryRuntime) UnlockAllAdvisory(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	runtime.manager.UnlockAllSession(runtime.owner.SessionID)
	return nil
}

func (transactions *Transactions) currentAdvisoryOwner() locks.AdvisoryOwner {
	return locks.AdvisoryOwner{SessionID: transactions.advisory.SessionID, TransactionID: transactions.generation}
}

func (transactions *Transactions) advisoryRuntime() model.AdvisoryLockRuntime {
	if transactions.advisory.Manager == nil || !transactions.active || transactions.transaction == nil {
		return nil
	}
	return sessionAdvisoryRuntime{manager: transactions.advisory.Manager, owner: transactions.currentAdvisoryOwner()}
}

func (transactions *Transactions) advisoryStatementMark() (locks.AdvisorySavepoint, error) {
	if transactions.advisory.Manager == nil {
		return locks.AdvisorySavepoint{}, nil
	}
	return transactions.advisory.Manager.TransactionSavepoint(transactions.currentAdvisoryOwner())
}

func (transactions *Transactions) rollbackAdvisoryStatement(mark locks.AdvisorySavepoint) error {
	if transactions.advisory.Manager == nil {
		return nil
	}
	return transactions.advisory.Manager.RollbackTransactionLocks(transactions.currentAdvisoryOwner(), mark)
}
