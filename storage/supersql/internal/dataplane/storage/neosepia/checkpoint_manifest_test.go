package neosepia

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestCheckpointManifestLimitsDoNotInvalidateCheckpoint(t *testing.T) {
	fixture := openBackendFixture(t)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 100)); err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), 20, 1, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 20, Fence: fence}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	full := checkpointManifest(t, checkpoint)
	if len(full.Files) < 2 {
		t.Fatal("fixture must include state and committed data")
	}
	for _, limits := range []storage.CheckpointManifestLimits{
		{}, {MaxBytes: 1, MaxFiles: 100}, {MaxBytes: 16 << 20, MaxFiles: uint64(len(full.Files) - 1)},
	} {
		partial, err := checkpoint.Manifest(t.Context(), limits)
		if !errors.Is(err, columnar.ErrBatchBudget) {
			t.Fatalf("limits %+v: %v", limits, err)
		}
		if len(partial.Files) != 0 || len(partial.Scale.CanonicalLeapHistory) != 0 {
			t.Fatal("failed admission returned partial metadata")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := checkpoint.Manifest(ctx, storage.CheckpointManifestLimits{MaxBytes: 16 << 20, MaxFiles: 100}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled manifest: %v", err)
	}
	name := full.Files[0].Name
	firstHistoryByte := full.Scale.CanonicalLeapHistory[0]
	full.Files[0].Name = "changed"
	full.Scale.CanonicalLeapHistory[0] ^= 0xff
	owned := checkpointManifest(t, checkpoint)
	if owned.Files[0].Name != name || owned.Scale.CanonicalLeapHistory[0] != firstHistoryByte {
		t.Fatal("caller mutated checkpoint metadata")
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Manifest(t.Context(), storage.CheckpointManifestLimits{MaxBytes: 16 << 20, MaxFiles: 100}); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("closed manifest: %v", err)
	}
	if owned.Files[0].Name != name || owned.Scale.CanonicalLeapHistory[0] != firstHistoryByte {
		t.Fatal("close invalidated owned metadata")
	}
}
