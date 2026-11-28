package neosepia

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func retirementProof(t *testing.T, target storage.ObjectRef, retiredAt model.Timestamp) storage.ObjectRetirement {
	t.Helper()
	retirement, err := storage.NewObjectRetirement(storage.ObjectGeneration{ObjectID: target.ObjectID, Generation: target.Generation}, retiredAt)
	if err != nil {
		t.Fatal(err)
	}
	return retirement
}

func requireTargetAbsent(t *testing.T, fixture backendFixture, timestamp model.Timestamp, target storage.ObjectRef) {
	t.Helper()
	reader := fixture.snapshot(t, timestamp)
	_, err := reader.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(1), ColumnIDs: []model.ColumnID{1}, Layout: scanFixtureSpec().Layout})
	if !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("retired target remains readable: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCollectRetiresGenerationOnlyAtSafeFloorAndAfterPins(t *testing.T) {
	fixture := openBackendFixture(t)
	for _, batch := range []storage.MutationBatch{fixture.batch(t, 1, 10, 100, 100), fixture.batch(t, 2, 20, 90, 110)} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	target := encTestTarget()
	retirement := retirementProof(t, target, 20)
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 19, Retired: []storage.ObjectRetirement{retirement}}); !errors.Is(err, storage.ErrInvalidMutation) {
		t.Fatalf("retirement before catalog history aged out: %v", err)
	}
	historical := fixture.snapshot(t, 10)
	// A captured checkpoint records SnapshotTS == Fence.ClosedTS, and both
	// checkpoint verification and partition snapshots enforce it. History at
	// timestamp 10 is exactly the state closed at position 1.
	fence, err := fixture.authority.Issue(t.Context(), 10, 1, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 10, Fence: fence}})
	if err != nil {
		t.Fatal(err)
	}
	request := storage.GCRequest{Boundary: 20, Retired: []storage.ObjectRetirement{retirement}}
	if err := fixture.backend.Collect(t.Context(), request); !errors.Is(err, storage.ErrSnapshotPinned) {
		t.Fatalf("retirement crossed historical and checkpoint pins: %v", err)
	}
	if err := historical.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), request); !errors.Is(err, storage.ErrSnapshotPinned) {
		t.Fatalf("retirement crossed checkpoint pin: %v", err)
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	requireTargetAbsent(t, fixture, 20, target)
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = reopened
	requireTargetAbsent(t, fixture, 20, target)
}

func TestCanceledCollectionDoesNotPublishRetirement(t *testing.T) {
	fixture := openBackendFixture(t)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 100, 100)); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	fixture.backend.maintenanceFault = func(ctx context.Context, _ maintenancePoint) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	target := encTestTarget()
	retirement := retirementProof(t, target, 10)
	go func() {
		done <- fixture.backend.Collect(ctx, storage.GCRequest{Boundary: 10, Retired: []storage.ObjectRetirement{retirement}})
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled collection: %v", err)
	}
	fixture.backend.maintenanceFault = nil
	reader := fixture.snapshot(t, 10)
	if got := backendBalance(t, reader, 0); got != 100 {
		t.Fatalf("canceled retirement changed target: %d", got)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}
