package txn

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// TestCheckpointSpecRejectsQueuedCommitResolution pins the gate ordering of
// CheckpointSpec: the unresolved-commit check happens while the sequencer gate
// is held, not before it. A checkpoint request that queues behind an in-flight
// commit must observe that commit's indeterminate outcome instead of certifying
// a fresh fence over a frontier that was never published.
func TestCheckpointSpecRejectsQueuedCommitResolution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var injected *cancelAfterCommitBackend
		fixture := newCommitFixture(t, func(backend storage.Backend) storage.Backend {
			injected = &cancelAfterCommitBackend{Backend: backend}
			return injected
		})
		// Every clock wait parks so the commit can be held at the gate and the
		// checkpoint request can be queued behind it deterministically.
		fixture.manager.sequencer.anchored = false
		fixture.clock.mu.Lock()
		fixture.clock.automatic = false
		fixture.clock.mu.Unlock()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		injected.cancel = cancel
		submitted := make(chan error, 1)
		go func() {
			_, err := fixture.manager.submit(ctx, fixture.batch(2, 90), false)
			submitted <- err
		}()
		target := <-fixture.clock.entered
		if target <= 0 {
			t.Fatalf("commit reservation: got %d", target)
		}
		synctest.Wait()
		var spec storage.SnapshotSpec
		checkpointed := make(chan error, 1)
		go func() {
			var err error
			spec, err = fixture.manager.CheckpointSpec(t.Context())
			checkpointed <- err
		}()
		synctest.Wait()
		// The commit applies durably, then cancels its own context, so its
		// resolution is indeterminate and stays pending while the gate is released.
		fixture.clock.advance(target+90, target+100)
		synctest.Wait()
		if err := <-submitted; !errors.Is(err, storage.ErrCommitIndeterminate) || !errors.Is(err, context.Canceled) {
			t.Fatalf("indeterminate commit: %v", err)
		}
		// A checkpoint that respects the unresolved commit returns before it needs
		// the clock, so this advance is consumed only by a fence that ignored it.
		fixture.clock.advance(target+190, target+200)
		synctest.Wait()
		if err := <-checkpointed; !errors.Is(err, storage.ErrCommitIndeterminate) {
			t.Fatalf("checkpoint certified over an unresolved commit: %v", err)
		}
		if spec != (storage.SnapshotSpec{}) {
			t.Fatalf("checkpoint published over an unresolved commit: %+v", spec)
		}
		resolution, err := fixture.manager.ResolveCommit(t.Context(), model.TxnID{2})
		if err != nil || resolution.State != storage.CommitCommitted {
			t.Fatalf("clock-free resolution after an indeterminate commit: %+v %v", resolution, err)
		}
		fixture.clock.mu.Lock()
		fixture.clock.automatic = true
		fixture.clock.mu.Unlock()
		spec, err = fixture.manager.CheckpointSpec(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if spec.Timestamp != spec.Fence.ClosedTS {
			t.Fatalf("checkpoint fence: timestamp %d, closed %d", spec.Timestamp, spec.Fence.ClosedTS)
		}
	})
}
