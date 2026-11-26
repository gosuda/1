package maintenance_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type cancelingCheckpointSource struct {
	source maintenance.Checkpointer
	cancel context.CancelFunc
}

func (s cancelingCheckpointSource) Checkpoint(ctx context.Context, request storage.CheckpointRequest) (storage.CheckpointHandle, error) {
	handle, err := s.source.Checkpoint(ctx, request)
	if err != nil {
		return handle, err
	}
	return cancelingCheckpoint{CheckpointHandle: handle, cancel: s.cancel}, nil
}

type cancelingCheckpoint struct {
	storage.CheckpointHandle
	cancel context.CancelFunc
}

func (c cancelingCheckpoint) OpenFile(ctx context.Context, name string) (io.ReadCloser, error) {
	reader, err := c.CheckpointHandle.OpenFile(ctx, name)
	if err != nil {
		return reader, err
	}
	return cancelingBackupReader{ReadCloser: reader, cancel: c.cancel}, nil
}

type cancelingBackupReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r cancelingBackupReader) Read(data []byte) (int, error) {
	n, err := r.ReadCloser.Read(data)
	r.cancel()
	return n, err
}

func TestCanceledBackupReleasesCheckpointProtection(t *testing.T) {
	fixture := newBackupFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	destination := filepath.Join(t.TempDir(), "backup")
	_, err := maintenance.CreateBackup(ctx, cancelingCheckpointSource{source: fixture.backend, cancel: cancel}, fixture.request, maintenance.BackupOptions{Destination: destination, Limits: backupLimits()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled copy: %v", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled backup published destination: %v", err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 3, 30, 120, 220)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 30}); err != nil {
		t.Fatalf("failed backup retained a checkpoint pin: %v", err)
	}
}
