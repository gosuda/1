package neosepia

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestCheckpointStreamClosePreservesTruncationFailure(t *testing.T) {
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
	t.Cleanup(func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	})
	file, err := checkpoint.(*checkpointHandle).root.OpenFile(databaseStateName, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	truncateErr := file.Truncate(1)
	if err := errors.Join(truncateErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, stream)
	closeErr := stream.Close()
	if !errors.Is(readErr, storage.ErrCorruptStorage) || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated checkpoint read: %v", readErr)
	}
	if !errors.Is(closeErr, storage.ErrCorruptStorage) || !errors.Is(closeErr, io.ErrUnexpectedEOF) {
		t.Fatalf("close lost truncated checkpoint failure: %v", closeErr)
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatalf("failed stream retained its checkpoint pin: %v", err)
	}
}
