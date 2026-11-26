package locks

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

func TestAdvisoryContentionDeadlockAndCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		graph, err := NewWaitGraph(DefaultWaitGraphLimits())
		if err != nil {
			t.Fatal(err)
		}
		limits := DefaultAdvisoryLimits()
		limits.WaitGraph = graph
		manager, err := NewAdvisoryLockManagerWithLimits(limits)
		if err != nil {
			t.Fatal(err)
		}
		first := AdvisoryOwner{SessionID: 1, TransactionID: 11}
		second := AdvisoryOwner{SessionID: 2, TransactionID: 22}
		left, right := KeyFromBigInt(1), KeyFromBigInt(2)
		if err := manager.Lock(t.Context(), first, left, AdvisoryExclusive, ScopeTransaction); err != nil {
			t.Fatal(err)
		}
		if err := manager.Lock(t.Context(), second, right, AdvisoryExclusive, ScopeTransaction); err != nil {
			t.Fatal(err)
		}
		acquired := make(chan error, 1)
		go func() { acquired <- manager.Lock(t.Context(), first, right, AdvisoryExclusive, ScopeTransaction) }()
		synctest.Wait()
		if err := manager.Lock(t.Context(), second, left, AdvisoryExclusive, ScopeTransaction); !errors.Is(err, ErrDeadlockDetected) {
			t.Fatalf("deadlock result = %v", err)
		}
		manager.ReleaseTransactionLocks(second)
		if err := <-acquired; err != nil {
			t.Fatal(err)
		}
		manager.ReleaseTransactionLocks(first)
		manager.mu.Lock()
		defer manager.mu.Unlock()
		if len(manager.locks) != 0 || len(manager.owners) != 0 || manager.waiters != 0 || manager.holds != 0 || manager.usedBytes != 0 {
			t.Fatalf("retained state: keys=%d owners=%d waiters=%d holds=%d bytes=%d", len(manager.locks), len(manager.owners), manager.waiters, manager.holds, manager.usedBytes)
		}
	})
}

func TestAdvisoryCancellationAndKeySpaces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, err := NewAdvisoryLockManagerWithLimits(DefaultAdvisoryLimits())
		if err != nil {
			t.Fatal(err)
		}
		first := AdvisoryOwner{SessionID: 1}
		second := AdvisoryOwner{SessionID: 2}
		bigint := KeyFromBigInt(1)
		pair := KeyFromPair(0, 1)
		if err := manager.Lock(t.Context(), first, bigint, AdvisoryExclusive, ScopeSession); err != nil {
			t.Fatal(err)
		}
		if acquired, err := manager.TryLock(second, pair, AdvisoryExclusive, ScopeSession); err != nil || !acquired {
			t.Fatalf("separate key space: acquired=%v err=%v", acquired, err)
		}
		if !manager.Unlock(second, pair, AdvisoryExclusive) {
			t.Fatal("pair unlock failed")
		}
		ctx, cancel := context.WithCancel(t.Context())
		blocked := make(chan error, 1)
		go func() { blocked <- manager.Lock(ctx, second, bigint, AdvisoryExclusive, ScopeSession) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-blocked; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter = %v", err)
		}
		manager.mu.Lock()
		waiters := manager.waiters
		manager.mu.Unlock()
		if waiters != 0 {
			t.Fatalf("retained waiters = %d", waiters)
		}
		manager.CloseSession(first.SessionID)
		if acquired, err := manager.TryLock(second, bigint, AdvisoryExclusive, ScopeSession); err != nil || !acquired {
			t.Fatalf("lock after close cleanup: acquired=%v err=%v", acquired, err)
		}
		manager.CloseSession(second.SessionID)
	})
}

func TestAdvisoryPerKeyWakeupAvoidsThunderingHerd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, err := NewAdvisoryLockManagerWithLimits(DefaultAdvisoryLimits())
		if err != nil {
			t.Fatal(err)
		}
		holderA := AdvisoryOwner{SessionID: 1}
		holderB := AdvisoryOwner{SessionID: 2}
		waiterA := AdvisoryOwner{SessionID: 3}
		waiterB := AdvisoryOwner{SessionID: 4}
		keyA := KeyFromBigInt(100)
		keyB := KeyFromBigInt(200)

		if err := manager.Lock(t.Context(), holderA, keyA, AdvisoryExclusive, ScopeSession); err != nil {
			t.Fatal(err)
		}
		if err := manager.Lock(t.Context(), holderB, keyB, AdvisoryExclusive, ScopeSession); err != nil {
			t.Fatal(err)
		}

		acquiredA := make(chan error, 1)
		acquiredB := make(chan error, 1)
		go func() { acquiredA <- manager.Lock(t.Context(), waiterA, keyA, AdvisoryExclusive, ScopeSession) }()
		go func() { acquiredB <- manager.Lock(t.Context(), waiterB, keyB, AdvisoryExclusive, ScopeSession) }()
		synctest.Wait()

		manager.mu.Lock()
		itemB := manager.locks[keyB]
		if itemB == nil || itemB.changed == nil {
			manager.mu.Unlock()
			t.Fatal("expected keyB waiter notification channel")
		}
		keyBChanged := itemB.changed
		manager.mu.Unlock()

		if !manager.Unlock(holderA, keyA, AdvisoryExclusive) {
			t.Fatal("unlock keyA failed")
		}
		synctest.Wait()

		if err := <-acquiredA; err != nil {
			t.Fatalf("waiterA lock failed: %v", err)
		}
		select {
		case <-keyBChanged:
			t.Fatal("unlocking keyA spuriously woke waiter on keyB")
		default:
		}
		manager.mu.Lock()
		if manager.locks[keyB] == nil || manager.locks[keyB].changed != keyBChanged {
			manager.mu.Unlock()
			t.Fatal("keyB notification channel was replaced during unrelated keyA unlock")
		}
		manager.mu.Unlock()

		// Closing a waiting session that holds no locks still wakes its waiter cleanly.
		manager.CloseSession(waiterB.SessionID)
		synctest.Wait()
		if err := <-acquiredB; !errors.Is(err, errAdvisorySessionClosed) {
			t.Fatalf("closed waiterB result = %v, want errAdvisorySessionClosed", err)
		}

		// Savepoint rollback wakes waiters only on keys released after the savepoint.
		txOwner := AdvisoryOwner{SessionID: 5, TransactionID: 55}
		waiterC := AdvisoryOwner{SessionID: 6}
		waiterD := AdvisoryOwner{SessionID: 7}
		keyC := KeyFromBigInt(300)
		keyD := KeyFromBigInt(400)
		if err := manager.Lock(t.Context(), txOwner, keyC, AdvisoryExclusive, ScopeTransaction); err != nil {
			t.Fatal(err)
		}
		savepoint, err := manager.TransactionSavepoint(txOwner)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.Lock(t.Context(), txOwner, keyD, AdvisoryExclusive, ScopeTransaction); err != nil {
			t.Fatal(err)
		}
		acquiredC := make(chan error, 1)
		acquiredD := make(chan error, 1)
		go func() { acquiredC <- manager.Lock(t.Context(), waiterC, keyC, AdvisoryExclusive, ScopeSession) }()
		go func() { acquiredD <- manager.Lock(t.Context(), waiterD, keyD, AdvisoryExclusive, ScopeSession) }()
		synctest.Wait()

		manager.mu.Lock()
		keyCChanged := manager.locks[keyC].changed
		manager.mu.Unlock()

		if err := manager.RollbackTransactionLocks(txOwner, savepoint); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := <-acquiredD; err != nil {
			t.Fatalf("waiterD lock after rollback failed: %v", err)
		}
		select {
		case <-keyCChanged:
			t.Fatal("rolling back keyD spuriously woke waiter on keyC")
		default:
		}

		manager.ReleaseTransactionLocks(txOwner)
		synctest.Wait()
		if err := <-acquiredC; err != nil {
			t.Fatalf("waiterC lock after tx release failed: %v", err)
		}

		manager.CloseSession(holderB.SessionID)
		manager.CloseSession(waiterA.SessionID)
		manager.CloseSession(waiterC.SessionID)
		manager.CloseSession(waiterD.SessionID)

		manager.mu.Lock()
		defer manager.mu.Unlock()
		if len(manager.locks) != 0 || len(manager.owners) != 0 || manager.waiters != 0 || manager.holds != 0 || manager.usedBytes != 0 {
			t.Fatalf("retained state: keys=%d owners=%d waiters=%d holds=%d bytes=%d", len(manager.locks), len(manager.owners), manager.waiters, manager.holds, manager.usedBytes)
		}
	})
}

