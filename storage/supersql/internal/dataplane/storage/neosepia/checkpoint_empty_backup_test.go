package neosepia

import (
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestEmptyCheckpointBackupPreservesClosedFence(t *testing.T) {
	fixture := openBackendFixture(t)
	fence, err := fixture.authority.Issue(t.Context(), 10, 0, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	limits := maintenance.Limits{MaxManifestBytes: 1 << 20, MaxMetadataBytes: 2 << 20, MaxFiles: 16, MaxNameBytes: 256, MaxNamesBytes: 4096, MaxFileBytes: 4 << 20, MaxTotalBytes: 16 << 20, CopyBufferBytes: 4096}
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := maintenance.CreateBackup(t.Context(), fixture.backend, storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 10, Fence: fence}}, maintenance.BackupOptions{Destination: backup, Limits: limits}); err != nil {
		t.Fatal(err)
	}
	config := fixture.config
	config.Directory = filepath.Join(t.TempDir(), "restored")
	if _, err := maintenance.RestoreCheckpoint(t.Context(), backup, maintenance.RestoreOptions{Destination: config.Directory, Limits: limits, Verifier: CheckpointVerifier{FenceVerifier: fixture.authority, Limits: DefaultLimits()}}); err != nil {
		t.Fatal(err)
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
	state, err := restored.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.LogPosition != 0 || state.CommitTS != 0 || state.ClosedTS != 10 {
		t.Fatalf("empty restore invented data or lost the closed fence: %+v", state)
	}
}
