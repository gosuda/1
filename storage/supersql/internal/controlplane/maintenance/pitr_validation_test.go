package maintenance_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
)

func TestPITRValidatesWALBeforeTargetCutoff(t *testing.T) {
	for _, test := range []struct {
		name   string
		damage func(*testing.T, string)
	}{
		{
			name: "sidecar timestamp hides required commit",
			damage: func(t *testing.T, archiveDir string) {
				path := filepath.Join(archiveDir, "00000000000000000002.meta")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var meta neosepia.WALArchiveMetadata
				if err := json.Unmarshal(raw, &meta); err != nil {
					t.Fatal(err)
				}
				meta.CommitTS = 400
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
			name: "gap precedes first post-target commit",
			damage: func(t *testing.T, archiveDir string) {
				if err := os.Remove(filepath.Join(archiveDir, "00000000000000000002.wal")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "post-target boundary WAL is corrupt",
			damage: func(t *testing.T, archiveDir string) {
				path := filepath.Join(archiveDir, "00000000000000000003.wal")
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
	} {
		t.Run(test.name, func(t *testing.T) {
			archiveDir := filepath.Join(t.TempDir(), "archive")
			env := newF5BackupEnv(t, archiveDir, nil, nil)
			env.applyRow(t, 1, 100, 1, 1000)
			baseDir := filepath.Join(t.TempDir(), "base")
			if _, err := maintenance.CreateBackup(t.Context(), env.backend, env.checkpointReq(t, 100, 1), maintenance.BackupOptions{
				Destination: baseDir,
				Limits:      backupLimits(),
			}); err != nil {
				t.Fatal(err)
			}
			env.applyRow(t, 2, 200, 2, 2000)
			env.applyRow(t, 3, 300, 3, 3000)
			test.damage(t, archiveDir)

			destination := filepath.Join(t.TempDir(), "restored")
			_, err := maintenance.RestorePITR(t.Context(), baseDir, archiveDir, 250, maintenance.RestoreOptions{
				Destination: destination,
				Limits:      backupLimits(),
				MaxWALBytes: neosepia.DefaultLimits().WALBytes,
				Verifier: neosepia.CheckpointVerifier{
					FenceVerifier: env.authority,
					Limits:        neosepia.DefaultLimits(),
				},
			})
			if !errors.Is(err, maintenance.ErrCorrupt) {
				t.Fatalf("RestorePITR error = %v, want ErrCorrupt", err)
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed restore published destination: %v", err)
			}
		})
	}
}

func TestPITRRejectsWALOverConfiguredLimit(t *testing.T) {
	archiveDir := filepath.Join(t.TempDir(), "archive")
	env := newF5BackupEnv(t, archiveDir, nil, nil)
	env.applyRow(t, 1, 100, 1, 1000)
	baseDir := filepath.Join(t.TempDir(), "base")
	if _, err := maintenance.CreateBackup(t.Context(), env.backend, env.checkpointReq(t, 100, 1), maintenance.BackupOptions{
		Destination: baseDir,
		Limits:      backupLimits(),
	}); err != nil {
		t.Fatal(err)
	}
	env.applyRow(t, 2, 200, 2, 2000)
	if err := os.Remove(filepath.Join(archiveDir, "00000000000000000002.meta")); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(archiveDir, "00000000000000000002.wal"), 8<<20); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	_, err := maintenance.RestorePITR(t.Context(), baseDir, archiveDir, 250, maintenance.RestoreOptions{
		Destination: destination,
		Limits:      backupLimits(),
		MaxWALBytes: 1024,
		Verifier: neosepia.CheckpointVerifier{
			FenceVerifier: env.authority,
			Limits:        neosepia.DefaultLimits(),
		},
	})
	if !errors.Is(err, maintenance.ErrLimit) {
		t.Fatalf("RestorePITR error = %v, want ErrLimit", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore published destination: %v", err)
	}
}
