package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestGroupCloseJoinsQueuedWorkAndBlockedProducerCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool, err := NewPool(PoolConfig{Parallelism: 1, QueueCapacity: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		group, err := pool.NewGroup(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		cleanupGate := make(chan struct{})
		defer func() {
			select {
			case <-cleanupGate:
			default:
				close(cleanupGate)
			}
		}()
		first, err := group.Submit(t.Context(), Task{Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return context.Cause(ctx)
		}, Cleanup: func() error { <-cleanupGate; return nil }})
		if err != nil {
			t.Fatal(err)
		}
		<-started
		var queuedRuns atomic.Int32
		var cleanups atomic.Int32
		second, err := group.Submit(t.Context(), Task{Run: func(context.Context) error {
			queuedRuns.Add(1)
			return nil
		}, Cleanup: func() error { cleanups.Add(1); return nil }})
		if err != nil {
			t.Fatal(err)
		}
		producerDone := make(chan error, 1)
		go func() {
			_, err := group.Submit(t.Context(), Task{Run: func(context.Context) error {
				queuedRuns.Add(1)
				return nil
			}, Cleanup: func() error { cleanups.Add(1); return nil }})
			producerDone <- err
		}()
		synctest.Wait()
		closed := make(chan error, 1)
		go func() { closed <- group.Close() }()
		synctest.Wait()
		select {
		case err := <-closed:
			t.Fatalf("Close returned before running cleanup: %v", err)
		default:
		}
		if err := <-producerDone; !errors.Is(err, ErrClosed) {
			t.Fatalf("blocked producer cancellation = %v", err)
		}
		close(cleanupGate)
		if err := <-closed; !errors.Is(err, ErrClosed) {
			t.Fatalf("group cancellation = %v", err)
		}
		for _, future := range []*Future{first, second} {
			if err := future.Wait(t.Context()); !errors.Is(err, ErrClosed) {
				t.Fatalf("future cancellation = %v", err)
			}
		}
		if queuedRuns.Load() != 0 || cleanups.Load() != 2 {
			t.Fatalf("canceled queue runs=%d cleanups=%d", queuedRuns.Load(), cleanups.Load())
		}
	})
}

func TestRejectedSubmissionReleasesReservationAndPreservesCleanupCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool, err := NewPool(PoolConfig{Parallelism: 1, QueueCapacity: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		group, err := pool.NewGroup(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer group.Close()
		started := make(chan struct{})
		first, err := group.Submit(t.Context(), Task{Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return context.Cause(ctx)
		}})
		if err != nil {
			t.Fatal(err)
		}
		<-started
		if _, err := group.Submit(t.Context(), Task{Run: func(context.Context) error { return nil }}); err != nil {
			t.Fatal(err)
		}
		budget, err := NewBudget(1, 1)
		if err != nil {
			t.Fatal(err)
		}
		claim, err := budget.TryReserve(1)
		if err != nil {
			t.Fatal(err)
		}
		cleanupCause := errors.New("release pin failed")
		_, err = group.TrySubmit(t.Context(), Task{Run: func(context.Context) error { return nil }, Cleanup: func() error {
			claim.Release()
			return cleanupCause
		}})
		if !errors.Is(err, ErrQueueFull) || !errors.Is(err, cleanupCause) || budget.Usage().Live != 0 {
			t.Fatalf("rejection err=%v live=%d", err, budget.Usage().Live)
		}
		first.Cancel(context.Canceled)
		if err := first.Wait(t.Context()); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestTaskAndCleanupErrorsSurviveConcurrentClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool, err := NewPool(PoolConfig{Parallelism: 1, QueueCapacity: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		group, err := pool.NewGroup(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		runCause := errors.New("execution failed")
		cleanupCause := errors.New("cleanup failed")
		future, err := group.Submit(t.Context(), Task{Run: func(context.Context) error { return runCause }, Cleanup: func() error { return cleanupCause }})
		if err != nil {
			t.Fatal(err)
		}
		if err := future.Wait(t.Context()); !errors.Is(err, runCause) || !errors.Is(err, cleanupCause) {
			t.Fatalf("future causes = %v", err)
		}
		var closes sync.WaitGroup
		results := make(chan error, 8)
		for range 4 {
			closes.Go(func() { results <- group.Close() })
			closes.Go(func() { results <- pool.Close() })
		}
		closes.Wait()
		close(results)
		for err := range results {
			// A pool closed after Group.Close has removed that group returns nil.
			if err != nil && (!errors.Is(err, runCause) || !errors.Is(err, cleanupCause)) {
				t.Fatalf("close lost original causes: %v", err)
			}
		}
	})
}

func TestQueryCloseJoinsCallerWorkBeforeReleasingOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool, err := NewPool(PoolConfig{Parallelism: 1, QueueCapacity: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		budget, err := NewBudget(8, 8)
		if err != nil {
			t.Fatal(err)
		}
		query, err := NewQuery(t.Context(), pool, budget, nil, QueryConfig{SpillMode: SpillDisabled})
		if err != nil {
			t.Fatal(err)
		}
		output, err := query.NewBuffer(OutputMemory, 8)
		if err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		defer func() {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}()
		started := make(chan struct{})
		var run sync.WaitGroup
		run.Go(func() {
			_ = query.Tasks().Run(t.Context(), Task{Run: func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				<-gate
				return nil
			}})
		})
		<-started
		closed := make(chan error, 1)
		go func() { closed <- query.Close() }()
		synctest.Wait()
		if budget.Usage().Live != 8 || len(output.Bytes()) != 8 {
			t.Fatal("query released output before caller task joined")
		}
		select {
		case err := <-closed:
			t.Fatalf("query closed before task returned: %v", err)
		default:
		}
		close(gate)
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		run.Wait()
		if budget.Usage().Live != 0 || output.Bytes() != nil {
			t.Fatal("joined query retained output")
		}
		if err := query.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGroupLimitAndCallerExecution(t *testing.T) {
	pool, err := NewPool(PoolConfig{Parallelism: 1, QueueCapacity: 1, MaxGroups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	group, err := pool.NewGroup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.NewGroup(t.Context()); !errors.Is(err, ErrBudget) {
		t.Fatalf("group bound = %v", err)
	}
	value := 0
	if err := group.Run(t.Context(), Task{Run: func(context.Context) error { value = 7; return nil }}); err != nil || value != 7 {
		t.Fatalf("caller work value=%d err=%v", value, err)
	}
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := group.Submit(t.Context(), Task{Run: func(context.Context) error { return nil }}); !errors.Is(err, ErrClosed) {
		t.Fatalf("submission after wait = %v", err)
	}
}
