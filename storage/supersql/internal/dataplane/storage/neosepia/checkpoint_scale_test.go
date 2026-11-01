package neosepia

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestHistoricalCheckpointRetainsFutureLeapMapping(t *testing.T) {
	fixture := openBackendFixture(t)
	original := fixture.batch(t, 1, 20, 100, 100)
	if _, err := fixture.backend.ApplyMutations(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), 20, 1, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	history := make([]byte, 29)
	copy(history, fixture.config.Scale.CanonicalLeapHistory)
	binary.BigEndian.PutUint32(history[9:13], 1)
	binary.BigEndian.PutUint64(history[13:21], 1)
	history[21] = 1
	fixture.config.Scale = clock.ScaleDescriptor{Name: clock.ScaleName, CanonicalLeapHistory: history, LeapHistoryID: sha256.Sum256(history)}
	upgraded, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := upgraded.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = upgraded
	fixture.epoch.LeapHistory = fixture.config.Scale.LeapHistoryID
	if _, err := upgraded.ApplyMutations(t.Context(), fixture.batch(t, 2, 30, 90, 110)); err != nil {
		t.Fatal(err)
	}
	snapshot := storage.SnapshotSpec{Timestamp: 20, Fence: fence}
	checkpoint, err := upgraded.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	config := fixture.config
	config.Directory = t.TempDir()
	config.Scale = clock.ScaleDescriptor{}
	copyCheckpointFiles(t, checkpoint, config.Directory)
	root, err := os.OpenRoot(config.Directory)
	if err != nil {
		t.Fatal(err)
	}
	verifier := CheckpointVerifier{FenceVerifier: fixture.authority, Limits: DefaultLimits()}
	verifyErr := verifier.VerifyCheckpoint(t.Context(), root, checkpointManifest(t, checkpoint))
	if err := errors.Join(verifyErr, root.Close()); err != nil {
		t.Fatalf("historical checkpoint with retained scale extension: %v", err)
	}
	restored, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	reader, err := restored.OpenSnapshot(t.Context(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	if backendBalance(t, reader, 0) != 100 || backendBalance(t, reader, 1) != 100 {
		t.Fatal("historical checkpoint included the later transfer")
	}
	retry, err := restored.ApplyMutations(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}
	if retry.LogPosition != 1 {
		t.Fatalf("restored transaction retry position: got %d, want 1", retry.LogPosition)
	}
	fixture.backend = restored
	result, err := restored.ApplyMutations(t.Context(), fixture.batch(t, 3, 25, 95, 105))
	if err != nil {
		t.Fatalf("write using retained future mapping: %v", err)
	}
	if result.LogPosition != 2 {
		t.Fatalf("post-restore commit position: got %d, want 2", result.LogPosition)
	}
	current := fixture.snapshot(t, 25)
	if backendBalance(t, current, 0) != 95 || backendBalance(t, current, 1) != 105 {
		t.Fatal("restored scale transition lost the next transfer")
	}
}
