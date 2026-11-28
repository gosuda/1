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
