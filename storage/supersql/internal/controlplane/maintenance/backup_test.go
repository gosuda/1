package maintenance_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type backupFixture struct {
	backend   *neosepia.Backend
	config    neosepia.Config
	authority *txn.FenceAuthority
	epoch     clock.Epoch
	request   storage.CheckpointRequest
	key       []byte
	layouts   []storage.ObjectLayout
}

func backupLimits() maintenance.Limits {
	return maintenance.Limits{MaxManifestBytes: 8 << 20, MaxMetadataBytes: 16 << 20, MaxFiles: 4096, MaxNameBytes: 256, MaxNamesBytes: 1 << 20, MaxFileBytes: 64 << 20, MaxTotalBytes: 256 << 20, CopyBufferBytes: 4096}
}

func newBackupFixture(t *testing.T, compacted bool) backupFixture {
	t.Helper()
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	scale := clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: sha256.Sum256(history), CanonicalLeapHistory: history}
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := txn.NewFenceAuthority([32]byte{7})
	if err != nil {
		t.Fatal(err)
	}
	config := neosepia.Config{Directory: filepath.Join(t.TempDir(), "source"), Scale: scale, KeyEncoder: encoder, FenceVerifier: authority}
	backend, err := neosepia.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	integer := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	key, err := encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: integer}}, []model.Value{model.Int64Val(1)}, 128)
	if err != nil {
		t.Fatal(err)
	}
	fixture := backupFixture{backend: backend, config: config, authority: authority, epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{2}, LeapHistory: scale.LeapHistoryID}, key: key}
	for _, id := range []model.ObjectID{1, 2} {
		fixture.layouts = append(fixture.layouts, storage.ObjectLayout{Target: storage.ObjectRef{ObjectID: id, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Columns: []storage.ColumnLayout{{ID: 1, Type: integer}, {ID: 2, Type: integer}}})
	}
	if _, err := backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 100, 200)); err != nil {
		t.Fatal(err)
	}
	if compacted {
		if err := backend.Collect(t.Context(), storage.GCRequest{Boundary: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backend.ApplyMutations(t.Context(), fixture.batch(t, 2, 20, 110, 210)); err != nil {
		t.Fatal(err)
	}
	fence, err := authority.Issue(t.Context(), 20, 2, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	fixture.request = storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 20, Fence: fence}}
	return fixture
}

func (f backupFixture) batch(t *testing.T, id byte, timestamp model.Timestamp, first, second int64) storage.MutationBatch {
	t.Helper()
	batch := storage.MutationBatch{TxID: model.TxnID{id}, CommitTS: timestamp, Epoch: f.epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: f.layouts}
	for i, value := range []int64{first, second} {
		batch.Mutations = append(batch.Mutations, storage.Mutation{Op: storage.MutationUpsert, Target: f.layouts[i].Target, Key: f.key, Ordinal: uint64(i + 1), ColumnIDs: []model.ColumnID{1, 2}, Values: []model.Value{model.Int64Val(1), model.Int64Val(value)}})
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

type changingCheckpointSource struct {
	*neosepia.Backend
	after storage.MutationBatch
}

func (s changingCheckpointSource) Checkpoint(ctx context.Context, request storage.CheckpointRequest) (storage.CheckpointHandle, error) {
	handle, err := s.Backend.Checkpoint(ctx, request)
	if err != nil {
		return handle, err
	}
	_, err = s.Backend.ApplyMutations(ctx, s.after)
	return handle, err
}

func TestBackupRestoresOneDatabaseCheckpoint(t *testing.T) {
	for _, compacted := range []bool{false, true} {
		name := "wal"
		if compacted {
			name = "base_and_wal"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newBackupFixture(t, compacted)
			backup := filepath.Join(t.TempDir(), "backup")
			source := changingCheckpointSource{Backend: fixture.backend, after: fixture.batch(t, 3, 30, 900, 901)}
			manifest, err := maintenance.CreateBackup(t.Context(), source, fixture.request, maintenance.BackupOptions{Destination: backup, Limits: backupLimits()})
			if err != nil {
				t.Fatal(err)
			}
			if manifest.SnapshotTS != 20 || manifest.Fence.LogPosition != 2 {
				t.Fatalf("captured frontier %+v", manifest.Fence)
			}
			if err := fixture.backend.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(fixture.config.Directory); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "restored")
			if _, err := maintenance.RestoreCheckpoint(t.Context(), backup, maintenance.RestoreOptions{Destination: destination, Limits: backupLimits(), Verifier: neosepia.CheckpointVerifier{FenceVerifier: fixture.authority, Limits: neosepia.DefaultLimits()}}); err != nil {
				t.Fatal(err)
			}
			config := fixture.config
			config.Directory = destination
			restored, err := neosepia.Open(t.Context(), config)
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
			if state.LogPosition != 2 || state.CommitTS != 20 {
				t.Fatalf("restored later commit: %+v", state)
			}
			reader, err := restored.OpenSnapshot(t.Context(), fixture.request.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			})
			for i, expected := range []int64{110, 210} {
				row, err := reader.Get(t.Context(), storage.ReadRequest{Target: fixture.layouts[i].Target, Key: fixture.key, ColumnIDs: []model.ColumnID{2}, Layout: fixture.layouts[i].Columns})
				if err != nil {
					t.Fatal(err)
				}
				if len(row.Values) != 1 || row.Values[0].IntVal != expected {
					t.Fatalf("table %d returned %+v, want %d", i+1, row.Values, expected)
				}
			}
		})
	}
}

func TestRestoreRejectsCorruptionWithoutPublishing(t *testing.T) {
	fixture := newBackupFixture(t, false)
	backup := filepath.Join(t.TempDir(), "backup")
	manifest, err := maintenance.CreateBackup(t.Context(), fixture.backend, fixture.request, maintenance.BackupOptions{Destination: backup, Limits: backupLimits()})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(backup, manifest.Files[0].Name), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var value [1]byte
	_, readErr := file.ReadAt(value[:], 0)
	value[0] ^= 0xff
	_, writeErr := file.WriteAt(value[:], 0)
	if err := errors.Join(readErr, writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	_, err = maintenance.RestoreCheckpoint(t.Context(), backup, maintenance.RestoreOptions{Destination: destination, Limits: backupLimits(), Verifier: neosepia.CheckpointVerifier{FenceVerifier: fixture.authority, Limits: neosepia.DefaultLimits()}})
	if err == nil {
		t.Fatal("corrupt backup restored")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore published a destination: %v", err)
	}
}

func TestBackupPreservesOccupiedDestination(t *testing.T) {
	fixture := newBackupFixture(t, false)
	destination := t.TempDir()
	sentinel := filepath.Join(destination, "keep")
	if err := os.WriteFile(sentinel, []byte("caller data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.CreateBackup(t.Context(), fixture.backend, fixture.request, maintenance.BackupOptions{Destination: destination, Limits: backupLimits()}); err == nil {
		t.Fatal("occupied destination accepted")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "caller data" {
		t.Fatalf("destination changed: %q, %v", data, err)
	}
}
