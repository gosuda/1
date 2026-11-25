package neosepia

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestFailedWALArchiveUpdatePreservesPreviousDestination(t *testing.T) {
	fixture := openBackendFixture(t)
	archiveDir := t.TempDir()
	if err := fixture.backend.EnableWALArchive(t.Context(), archiveDir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	brokenArchive := t.TempDir()
	if err := os.WriteFile(filepath.Join(brokenArchive, commitName(1)), []byte("broken WAL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.EnableWALArchive(t.Context(), brokenArchive, nil); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("EnableWALArchive error = %v, want corrupt storage", err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 2, 30, 101, 201)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(archiveDir, commitName(2))); err != nil {
		t.Fatalf("failed archive update disabled the previous destination: %v", err)
	}
}

func TestWALRetirementRetriesFailedArchiveHook(t *testing.T) {
	fixture := openBackendFixture(t)
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	archiveDir := filepath.Join(t.TempDir(), "archive")
	archiveFailure := errors.New("archive destination is unavailable")
	var available atomic.Bool
	var calls atomic.Uint32
	fixture.config.WALArchiveDir = archiveDir
	fixture.config.WALArchiveHook = func(context.Context, WALArchiveEntry) error {
		calls.Add(1)
		if !available.Load() {
			return archiveFailure
		}
		return nil
	}
	backend, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = backend
	batch := fixture.batch(t, 1, 20, 100, 200)
	if _, err := backend.ApplyMutations(t.Context(), batch); !errors.Is(err, archiveFailure) || !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("ApplyMutations error = %v, want indeterminate archive failure", err)
	}
	resolution, err := backend.ResolveCommit(t.Context(), batch.TxID)
	if err != nil || resolution.State != storage.CommitCommitted {
		t.Fatalf("ResolveCommit = %+v, %v", resolution, err)
	}
	walPath := filepath.Join(fixture.config.Directory, "commits", "00000000000000000001.wal")
	before := calls.Load()
	if err := backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); !errors.Is(err, archiveFailure) {
		t.Fatalf("Collect error = %v, want archive failure", err)
	}
	if calls.Load() <= before {
		t.Fatal("retirement did not retry the failed archive hook")
	}
	if _, err := os.Stat(walPath); err != nil {
		t.Fatalf("retirement removed unarchived source WAL: %v", err)
	}

	available.Store(true)
	before = calls.Load()
	if err := backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() <= before {
		t.Fatal("retirement did not retry the recovered archive hook")
	}
	if _, err := os.Stat(walPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source WAL remains after successful archive retry: %v", err)
	}
}

func TestWALRetirementRejectsInvalidArchive(t *testing.T) {
	for _, test := range []struct {
		name   string
		damage func(*testing.T, string)
	}{
		{
			name: "corrupt WAL",
			damage: func(t *testing.T, directory string) {
				path := filepath.Join(directory, commitName(1))
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "inconsistent metadata",
			damage: func(t *testing.T, directory string) {
				path := filepath.Join(directory, walMetaName(1))
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var meta WALArchiveMetadata
				if err := json.Unmarshal(raw, &meta); err != nil {
					t.Fatal(err)
				}
				meta.CommitTS++
				raw, err = json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "different commit at same position",
			damage: func(t *testing.T, directory string) {
				other := openBackendFixture(t)
				otherArchive := t.TempDir()
				if err := other.backend.EnableWALArchive(t.Context(), otherArchive, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := other.backend.ApplyMutations(t.Context(), other.batch(t, 1, 20, 999, 999)); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{commitName(1), walMetaName(1)} {
					raw, err := os.ReadFile(filepath.Join(otherArchive, name))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(directory, name), raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := openBackendFixture(t)
			archiveDir := t.TempDir()
			if err := fixture.backend.EnableWALArchive(t.Context(), archiveDir, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 200)); err != nil {
				t.Fatal(err)
			}
			test.damage(t, archiveDir)
			if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); !errors.Is(err, storage.ErrCorruptStorage) {
				t.Fatalf("Collect error = %v, want corrupt storage", err)
			}
			if _, err := os.Stat(filepath.Join(fixture.config.Directory, "commits", commitName(1))); err != nil {
				t.Fatalf("retirement removed source WAL after archive validation failed: %v", err)
			}
		})
	}
}

func TestWALRetirementRepairsMetadataWithoutReplacingWAL(t *testing.T) {
	fixture := openBackendFixture(t)
	archiveDir := t.TempDir()
	if err := fixture.backend.EnableWALArchive(t.Context(), archiveDir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	walPath := filepath.Join(archiveDir, commitName(1))
	before, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(archiveDir, walMetaName(1))); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("retry replaced an already published archive WAL")
	}
	if _, err := os.Stat(filepath.Join(archiveDir, walMetaName(1))); err != nil {
		t.Fatalf("retry did not recover archive metadata: %v", err)
	}
}
