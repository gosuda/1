package neosepia

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func checkpointManifest(t testing.TB, checkpoint storage.CheckpointHandle) storage.CheckpointManifest {
	t.Helper()
	manifest, err := checkpoint.Manifest(t.Context(), storage.CheckpointManifestLimits{MaxBytes: 16 << 20, MaxFiles: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func copyCheckpointFiles(t *testing.T, checkpoint storage.CheckpointHandle, directory string) {
	t.Helper()
	// Match restore staging: empty native directories are not manifest entries.
	for _, name := range []string{"commits", "runs"} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range checkpointManifest(t, checkpoint).Files {
		source, err := checkpoint.OpenFile(t.Context(), file.Name)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, file.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			source.Close()
			t.Fatal(err)
		}
		destination, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			source.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(destination, source)
		syncErr := destination.Sync()
		if err := errors.Join(copyErr, syncErr, source.Close(), destination.Close()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckpointRestoresItsPrefixAfterSourceFilesAreUnlinked(t *testing.T) {
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
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 2, 30, 90, 110)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range checkpointManifest(t, checkpoint).Files {
		if file.Name != databaseStateName {
			if err := os.Remove(filepath.Join(fixture.config.Directory, file.Name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	destination := t.TempDir()
	copyCheckpointFiles(t, checkpoint, destination)
	config := fixture.config
	config.Directory = destination
	config.Scale = clock.ScaleDescriptor{}
	restored, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = restored
	reader := fixture.snapshot(t, 20)
	if backendBalance(t, reader, 0) != 100 || backendBalance(t, reader, 1) != 100 {
		t.Fatal("checkpoint restored a later transaction or omitted a participant")
	}
	if _, err := checkpoint.OpenFile(t.Context(), "../database.state"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint accepted an arbitrary path: %v", err)
	}
}

func TestCheckpointHandleRequiresStreamsToCloseFirst(t *testing.T) {
	fixture := openBackendFixture(t)
	fence, err := fixture.authority.Issue(t.Context(), 10, 0, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 10, Fence: fence}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	stream, err := checkpoint.OpenFile(t.Context(), databaseStateName)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Close(); !errors.Is(err, storage.ErrSnapshotPinned) {
		stream.Close()
		t.Fatalf("active checkpoint stream was invalidated: %v", err)
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		stream.Close()
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	copyCheckpointFiles(t, checkpoint, destination)
	config := fixture.config
	config.Directory = destination
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
	if state.LogPosition != 0 || state.ClosedTS != 10 {
		t.Fatalf("empty checkpoint invented a commit or lost its barrier: %+v", state)
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRejectsChangedNativeData(t *testing.T) {
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
	for _, member := range checkpointManifest(t, checkpoint).Files {
		if !strings.HasPrefix(member.Name, "runs/") || member.Name == "runs/"+runManifestName(1) {
			continue
		}
		file, err := fixture.backend.directory.root.OpenFile(member.Name, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		var damage [1]byte
		offset := int64(member.Size) - 1
		if _, err := file.ReadAt(damage[:], offset); err != nil {
			file.Close()
			t.Fatal(err)
		}
		damage[0] ^= 255
		if _, err := file.WriteAt(damage[:], offset); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		damaged, captureErr := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 20, Fence: fence}})
		if damaged != nil {
			if err := damaged.Close(); err != nil {
				t.Fatal(err)
			}
			t.Fatal("checkpoint capture accepted damaged native data")
		}
		if !errors.Is(captureErr, storage.ErrCorruptStorage) {
			t.Fatalf("damaged checkpoint capture: %v", captureErr)
		}
		stream, err := checkpoint.OpenFile(t.Context(), member.Name)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(io.Discard, stream)
		closeErr := stream.Close()
		if !errors.Is(copyErr, storage.ErrCorruptStorage) || !errors.Is(closeErr, storage.ErrCorruptStorage) {
			t.Fatalf("changed checkpoint data was accepted: read=%v close=%v", copyErr, closeErr)
		}
		return
	}
	t.Fatal("checkpoint omitted its native data files")
}
