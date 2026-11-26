package neosepia

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func checkpointVerificationFixture(t *testing.T) (storage.CheckpointManifest, *os.Root, CheckpointVerifier) {
	t.Helper()
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
	directory := t.TempDir()
	copyCheckpointFiles(t, checkpoint, directory)
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return checkpointManifest(t, checkpoint), root, CheckpointVerifier{FenceVerifier: fixture.authority, Limits: DefaultLimits()}
}

func TestCheckpointVerifierRejectsOmittedRequiredFiles(t *testing.T) {
	manifest, root, verifier := checkpointVerificationFixture(t)
	if err := verifier.VerifyCheckpoint(t.Context(), root, manifest); err != nil {
		t.Fatal(err)
	}
	position := -1
	for i, file := range manifest.Files {
		if strings.HasSuffix(file.Name, ".sst") {
			position = i
			break
		}
	}
	if position < 0 {
		t.Fatal("fixture has no SST file")
	}
	name := manifest.Files[position].Name
	if err := root.Remove(name); err != nil {
		t.Fatal(err)
	}
	manifest.Files = append(manifest.Files[:position:position], manifest.Files[position+1:]...)
	if err := verifier.VerifyCheckpoint(t.Context(), root, manifest); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("manifest omitted a required SST without invalidating listed checksums: %v", err)
	}
}

func TestCheckpointVerifierDoesNotRepairInput(t *testing.T) {
	manifest, root, verifier := checkpointVerificationFixture(t)
	if err := verifier.VerifyCheckpoint(t.Context(), root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification created a database lock: %v", err)
	}
	pending := filepath.Base(databaseStatePending)
	file, err := root.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("pending data must not be removed")
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if err := verifier.VerifyCheckpoint(t.Context(), root, manifest); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("unlisted pending input accepted: %v", err)
	}
	file, err = root.Open(pending)
	if err != nil {
		t.Fatalf("verification repaired pending input: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
