package txn

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestReadOnlyTransitionRestoresSavepointModeAndPreservesSnapshot(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	mark, err := transaction.Savepoint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Statement(t.Context(), func(ctx context.Context) error {
		return transaction.SetReadOnly(ctx, true)
	}); err != nil {
		t.Fatal(err)
	}
	if transaction.mode != ReadOnly {
		t.Fatalf("transaction mode = %v, want ReadOnly", transaction.mode)
	}
	contractValues(t, transaction, fixture.request, 100)
	if transaction.base == nil || transaction.timestamp == 0 || !transaction.HasDataAccess() {
		t.Fatalf("read-only snapshot was not fixed: base=%v timestamp=%d accessed=%v", transaction.base, transaction.timestamp, transaction.HasDataAccess())
	}
	fixedTimestamp := transaction.timestamp
	fixture.manager.locks.mu.Lock()
	lockCount := fixture.manager.locks.lockCount
	fixture.manager.locks.mu.Unlock()
	if lockCount != 0 {
		t.Fatalf("read-only access acquired %d PCC locks", lockCount)
	}
	if err := transaction.SetReadOnly(t.Context(), true); err != nil {
		t.Fatalf("idempotent read-only transition after access: %v", err)
	}
	err = transaction.SetReadOnly(t.Context(), false)
	if !errors.Is(err, ErrTransactionModeAfterAccess) {
		t.Fatalf("fixed read-only snapshot upgraded: %v", err)
	}
	requireLockCode(t, err, "25001")
	if err := transaction.ApplyMutations(t.Context(), []storage.Mutation{statementBalance(fixture, 90)}); !errors.Is(err, ErrReadOnlyTransaction) {
		t.Fatalf("read-only transaction accepted write: %v", err)
	}
	if err := transaction.RollbackTo(t.Context(), mark); err != nil {
		t.Fatal(err)
	}
	if transaction.mode != ReadWrite {
		t.Fatalf("savepoint mode = %v, want ReadWrite", transaction.mode)
	}
	if transaction.base == nil || transaction.timestamp != fixedTimestamp || !transaction.HasDataAccess() {
		t.Fatalf("rollback lost fixed snapshot: base=%v timestamp=%d accessed=%v", transaction.base, transaction.timestamp, transaction.HasDataAccess())
	}
	contractApply(t, transaction, statementBalance(fixture, 90))
	contractCommit(t, transaction)
	current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	contractValues(t, current, fixture.request, 90)
}

func TestInitiallyReadOnlyTransactionCannotGainWriteCapability(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	err := transaction.SetReadOnly(t.Context(), false)
	if !errors.Is(err, ErrReadOnlyTransaction) {
		t.Fatalf("initially read-only transaction upgraded: %v", err)
	}
	requireLockCode(t, err, "25006")
}

func TestTransactionLockTimeoutAndCancellationReleaseRequestLocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		options := contractBalanceOptions(t, fixture)
		holder := contractBegin(t, fixture.manager, options)
		contender := contractBegin(t, fixture.manager, options)
		if acquired, err := holder.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, WaitForRowLock); !acquired || err != nil {
			t.Fatalf("holder lock: acquired=%v err=%v", acquired, err)
		}

		waitContext := WithLockTimeout(t.Context(), time.Hour)
		beforeWait, cancelBeforeWait := context.WithTimeout(t.Context(), 2*time.Hour)
		<-beforeWait.Done()
		cancelBeforeWait()
		timedOut := make(chan lockResult, 1)
		go func() {
			acquired, err := contender.LockRow(waitContext, fixture.request.Target, fixture.request.Key, UpdateLock, WaitForRowLock)
			timedOut <- lockResult{acquired: acquired, err: err}
		}()
		requireLockBlocked(t, timedOut)
		got := <-timedOut
		if got.acquired || !errors.Is(got.err, ErrLockTimeout) || !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("transaction lock timeout: %+v", got)
		}
		requireLockCode(t, got.err, "55P03")
		if contender.State() != Active {
			t.Fatalf("lock timeout changed contender state: %v", contender.State())
		}
		fixture.manager.locks.mu.Lock()
		lockCount := fixture.manager.locks.lockCount
		fixture.manager.locks.mu.Unlock()
		if lockCount != 2 {
			t.Fatalf("timed-out request retained locks: count=%d, want holder's 2", lockCount)
		}

		requestContext, cancelRequest := context.WithCancel(t.Context())
		canceled := make(chan lockResult, 1)
		go func() {
			acquired, err := contender.LockRow(requestContext, fixture.request.Target, fixture.request.Key, UpdateLock, WaitForRowLock)
			canceled <- lockResult{acquired: acquired, err: err}
		}()
		requireLockBlocked(t, canceled)
		cancelRequest()
		got = <-canceled
		if got.acquired || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("transaction lock cancellation: %+v", got)
		}
		requireLockCode(t, got.err, "57014")
		fixture.manager.locks.mu.Lock()
		lockCount = fixture.manager.locks.lockCount
		fixture.manager.locks.mu.Unlock()
		if lockCount != 2 {
			t.Fatalf("canceled request retained locks: count=%d, want holder's 2", lockCount)
		}

		if err := holder.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if acquired, err := contender.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, NoWaitForRowLock); !acquired || err != nil {
			t.Fatalf("lock after holder cleanup: acquired=%v err=%v", acquired, err)
		}
		if err := contender.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
	})
}
