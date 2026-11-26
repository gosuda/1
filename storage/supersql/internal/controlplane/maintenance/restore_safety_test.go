package maintenance_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
)

func TestRestorePreservesOccupiedDestination(t *testing.T) {
	fixture := newBackupFixture(t, false)
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := maintenance.CreateBackup(t.Context(), fixture.backend, fixture.request, maintenance.BackupOptions{Destination: backup, Limits: backupLimits()}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	sentinel := filepath.Join(destination, "keep")
	if err := os.WriteFile(sentinel, []byte("caller data"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := maintenance.RestoreCheckpoint(t.Context(), backup, maintenance.RestoreOptions{Destination: destination, Limits: backupLimits(), Verifier: neosepia.CheckpointVerifier{FenceVerifier: fixture.authority, Limits: neosepia.DefaultLimits()}})
	if err == nil {
		t.Fatal("occupied restore destination accepted")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "caller data" {
		t.Fatalf("restore changed caller data: %q, %v", data, err)
	}
}

func TestRestoreRejectsSymlinkEvenWhenContentsMatch(t *testing.T) {
	fixture := newBackupFixture(t, false)
	backup := filepath.Join(t.TempDir(), "backup")
	manifest, err := maintenance.CreateBackup(t.Context(), fixture.backend, fixture.request, maintenance.BackupOptions{Destination: backup, Limits: backupLimits()})
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(backup, manifest.Files[0].Name)
	outside := filepath.Join(t.TempDir(), "unchanged-bytes")
	if err := os.Rename(original, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, original); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	_, err = maintenance.RestoreCheckpoint(t.Context(), backup, maintenance.RestoreOptions{Destination: destination, Limits: backupLimits(), Verifier: neosepia.CheckpointVerifier{FenceVerifier: fixture.authority, Limits: neosepia.DefaultLimits()}})
	if err == nil {
		t.Fatal("symlink backup file accepted")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink rejection published destination: %v", err)
	}
}
