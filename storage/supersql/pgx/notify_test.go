package pgx_test

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPgxWaitForNotification(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	listener, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire listener: %v", err)
	}
	defer listener.Release()

	sender, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire sender: %v", err)
	}
	defer sender.Release()

	ct, err := listener.Exec(ctx, `LISTEN jobs`)
	if err != nil {
		t.Fatalf("LISTEN jobs: %v", err)
	}
	if ct.String() != "LISTEN" {
		t.Fatalf("LISTEN CommandTag = %q, want LISTEN", ct.String())
	}

	// Timeout / context cancellation when no notification arrives.
	shortCtx, cancelShort := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancelShort()
	if _, err := listener.WaitForNotification(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}

	// Send via transactional NOTIFY and via pg_notify from another connection.
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		tx, err := sender.Begin(ctx)
		if err != nil {
			errCh <- err
			return
		}
		if _, err := tx.Exec(ctx, `NOTIFY jobs, 'job-42'`); err != nil {
			_ = tx.Rollback(ctx)
			errCh <- err
			return
		}
		if err := tx.Commit(ctx); err != nil {
			errCh <- err
			return
		}
		if _, err := sender.Exec(ctx, `SELECT pg_notify('jobs', 'job-43')`); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, 3*time.Second)
	defer cancelWait()

	n1, err := listener.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification 1: %v", err)
	}
	if n1.Channel != "jobs" || n1.Payload != "job-42" || n1.PID == 0 {
		t.Fatalf("n1 = %+v, want jobs/job-42 with non-zero PID", n1)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("sender error: %v", err)
	}

	n2, err := listener.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification 2: %v", err)
	}
	if n2.Channel != "jobs" || n2.Payload != "job-43" {
		t.Fatalf("n2 = %+v, want jobs/job-43", n2)
	}
}
