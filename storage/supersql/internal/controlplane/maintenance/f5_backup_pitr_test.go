package maintenance_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

type f5BackupEnv struct {
	backend   *neosepia.Backend
	config    neosepia.Config
	authority *txn.FenceAuthority
	epoch     clock.Epoch
	encoder   storage.KeyEncoder
	intType   model.Type
	layouts   []storage.ObjectLayout
}

func newF5BackupEnv(t *testing.T, archiveDir string, dbKey, archiveKey []byte) *f5BackupEnv {
	t.Helper()
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	scale := clock.ScaleDescriptor{
		Name:                 clock.ScaleName,
		LeapHistoryID:        sha256.Sum256(history),
		CanonicalLeapHistory: history,
	}
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := txn.NewFenceAuthority([32]byte{11})
	if err != nil {
		t.Fatal(err)
	}
	intType := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	layouts := []storage.ObjectLayout{{
		Target:  storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Columns: []storage.ColumnLayout{{ID: 1, Type: intType}, {ID: 2, Type: intType}},
	}}
	cfg := neosepia.Config{
		Directory:     filepath.Join(t.TempDir(), "source"),
		Scale:         scale,
		KeyEncoder:    encoder,
		FenceVerifier: authority,
		EncryptionKey: dbKey,
		WALArchiveDir: archiveDir,
		WALArchiveKey: archiveKey,
	}
	backend, err := neosepia.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if backend != nil {
			if err := backend.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	return &f5BackupEnv{
		backend:   backend,
		config:    cfg,
		authority: authority,
		epoch:     clock.Epoch{Assurance: [16]byte{5}, Config: [32]byte{6}, LeapHistory: scale.LeapHistoryID},
		encoder:   encoder,
		intType:   intType,
		layouts:   layouts,
	}
}

func (e *f5BackupEnv) key(t *testing.T, id int64) []byte {
	t.Helper()
	k, err := e.encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: e.intType}}, []model.Value{model.Int64Val(id)}, 128)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (e *f5BackupEnv) applyRow(t *testing.T, txID byte, ts model.Timestamp, id, val int64) storage.ApplyResult {
	t.Helper()
	batch := storage.MutationBatch{
		TxID:            model.TxnID{txID},
		CommitTS:        ts,
		Epoch:           e.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         e.layouts,
		Mutations: []storage.Mutation{{
			Op:        storage.MutationUpsert,
			Target:    e.layouts[0].Target,
			Key:       e.key(t, id),
			Ordinal:   1,
			ColumnIDs: []model.ColumnID{1, 2},
			Values:    []model.Value{model.Int64Val(id), model.Int64Val(val)},
		}},
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.backend.ApplyMutations(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *f5BackupEnv) checkpointReq(t *testing.T, ts model.Timestamp, pos uint64) storage.CheckpointRequest {
	t.Helper()
	fence, err := e.authority.Issue(t.Context(), ts, pos, e.epoch)
	if err != nil {
		t.Fatal(err)
	}
	return storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: ts, Fence: fence}}
}

func (e *f5BackupEnv) getRow(t *testing.T, backend *neosepia.Backend, ts model.Timestamp, pos uint64, id int64) (int64, bool) {
	t.Helper()
	fence, err := e.authority.Issue(t.Context(), ts, pos, e.epoch)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: ts, Fence: fence})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	row, err := reader.Get(t.Context(), storage.ReadRequest{
		Target:    e.layouts[0].Target,
		Key:       e.key(t, id),
		ColumnIDs: []model.ColumnID{2},
		Layout:    e.layouts[0].Columns,
	})
	if errors.Is(err, storage.ErrRowNotFound) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row.Values[0].IntVal, true
}

func TestF5EncryptionChunkedFraming(t *testing.T) {
	key := randomKey(t)
	wrongKey := randomKey(t)

	// Invalid key length rejected.
	if _, err := maintenance.KeyFingerprint([]byte("short")); !errors.Is(err, maintenance.ErrInvalidEncryptionKey) {
		t.Fatalf("short key fingerprint err = %v, want ErrInvalidEncryptionKey", err)
	}

	sizes := []int{0, 1, 17, 65535, 65536, 65537, 131072, 200000}
	for _, sz := range sizes {
		plain := make([]byte, sz)
		if _, err := rand.Read(plain); err != nil {
			t.Fatal(err)
		}
		ct, err := maintenance.EncryptBytes(key, plain)
		if err != nil {
			t.Fatalf("EncryptBytes(%d): %v", sz, err)
		}
		if !bytes.HasPrefix(ct, []byte("NSE1")) {
			t.Fatalf("size %d missing NSE1 magic", sz)
		}
		got, err := maintenance.DecryptBytes(key, ct)
		if err != nil {
			t.Fatalf("DecryptBytes(%d): %v", sz, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size %d round-trip mismatch", sz)
		}
		// Wrong key fails.
		if _, err := maintenance.DecryptBytes(wrongKey, ct); err == nil {
			t.Fatalf("size %d wrong key accepted", sz)
		}
		// Flipped byte in ciphertext fails.
		tampered := bytes.Clone(ct)
		tampered[len(tampered)-1] ^= 0x01
		if _, err := maintenance.DecryptBytes(key, tampered); err == nil {
			t.Fatalf("size %d tampered ciphertext accepted", sz)
		}
		// Truncation fails.
		if _, err := maintenance.DecryptBytes(key, ct[:len(ct)-1]); err == nil {
			t.Fatalf("size %d truncated ciphertext accepted", sz)
		}
		// Appended trailing byte fails.
		if _, err := maintenance.DecryptBytes(key, append(bytes.Clone(ct), 0x00)); err == nil {
			t.Fatalf("size %d trailing byte accepted", sz)
		}
	}
}

func TestF5EncryptedBackupAndRestore(t *testing.T) {
	env := newF5BackupEnv(t, "", nil, nil)
	env.applyRow(t, 1, 10, 1, 100)
	env.applyRow(t, 2, 20, 2, 200)

	key := randomKey(t)
	wrongKey := randomKey(t)
	backupDir := filepath.Join(t.TempDir(), "enc-backup")

	manifest, err := maintenance.CreateBackup(t.Context(), env.backend, env.checkpointReq(t, 20, 2), maintenance.BackupOptions{
		Destination:   backupDir,
		Limits:        backupLimits(),
		EncryptionKey: key,
	})
	if err != nil {
		t.Fatalf("CreateBackup encrypted: %v", err)
	}

	// Verify envelope records encrypted:true and the exact key fingerprint.
	envRaw, err := os.ReadFile(filepath.Join(backupDir, maintenance.BackupManifestName))
	if err != nil {
		t.Fatal(err)
	}
	fp, err := maintenance.KeyFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envRaw), `"encrypted":true`) || !strings.Contains(string(envRaw), hex.EncodeToString(fp[:])) {
		t.Fatalf("backup envelope missing encrypted flag or fingerprint: %s", string(envRaw))
	}

	// Verify backed-up files on disk are NSE1-encrypted.
	for _, f := range manifest.Files {
		raw, err := os.ReadFile(filepath.Join(backupDir, f.Name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(raw, []byte("NSE1")) {
			t.Fatalf("backed up file %s not NSE1-encrypted", f.Name)
		}
	}

	verifier := neosepia.CheckpointVerifier{FenceVerifier: env.authority, Limits: neosepia.DefaultLimits()}

	// Missing key fails fast with ErrInvalidEncryptionKey.
	missingDest := filepath.Join(t.TempDir(), "missing-key")
	if _, err := maintenance.RestoreCheckpoint(t.Context(), backupDir, maintenance.RestoreOptions{
		Destination: missingDest, Limits: backupLimits(), Verifier: verifier,
	}); !errors.Is(err, maintenance.ErrInvalidEncryptionKey) {
		t.Fatalf("missing key error = %v, want ErrInvalidEncryptionKey", err)
	}
	if _, err := os.Lstat(missingDest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore published destination")
	}

	// Wrong key fails fast with ErrInvalidEncryptionKey.
	wrongDest := filepath.Join(t.TempDir(), "wrong-key")
	if _, err := maintenance.RestoreCheckpoint(t.Context(), backupDir, maintenance.RestoreOptions{
		Destination: wrongDest, Limits: backupLimits(), Verifier: verifier, EncryptionKey: wrongKey,
	}); !errors.Is(err, maintenance.ErrInvalidEncryptionKey) {
		t.Fatalf("wrong key error = %v, want ErrInvalidEncryptionKey", err)
	}

	// Right key succeeds and recovers exact data!
	okDest := filepath.Join(t.TempDir(), "restored")
	if _, err := maintenance.RestoreCheckpoint(t.Context(), backupDir, maintenance.RestoreOptions{
		Destination: okDest, Limits: backupLimits(), Verifier: verifier, EncryptionKey: key,
	}); err != nil {
		t.Fatalf("RestoreCheckpoint right key: %v", err)
	}
	cfg := env.config
	cfg.Directory = okDest
	restored, err := neosepia.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	if v, ok := env.getRow(t, restored, 20, 2, 1); !ok || v != 100 {
		t.Fatalf("row 1 = (%d, %v), want (100, true)", v, ok)
	}
	if v, ok := env.getRow(t, restored, 20, 2, 2); !ok || v != 200 {
		t.Fatalf("row 2 = (%d, %v), want (200, true)", v, ok)
	}

	// Tampered ciphertext fails cleanly without publishing.
	targetPath := filepath.Join(backupDir, manifest.Files[0].Name)
	ct, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	ct[len(ct)-1] ^= 0xff
	if err := os.WriteFile(targetPath, ct, 0600); err != nil {
		t.Fatal(err)
	}
	tamperDest := filepath.Join(t.TempDir(), "tampered")
	if _, err := maintenance.RestoreCheckpoint(t.Context(), backupDir, maintenance.RestoreOptions{
		Destination: tamperDest, Limits: backupLimits(), Verifier: verifier, EncryptionKey: key,
	}); err == nil {
		t.Fatal("tampered encrypted backup restored")
	}
	if _, err := os.Lstat(tamperDest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tampered restore published destination")
	}
}

func TestF5IncrementalBackupTwoHopChain(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		var key []byte
		if encrypted {
			name = "encrypted"
			key = randomKey(t)
		}
		t.Run(name, func(t *testing.T) {
			env := newF5BackupEnv(t, "", nil, nil)
			env.applyRow(t, 1, 10, 1, 100)
			if err := env.backend.Collect(t.Context(), storage.GCRequest{Boundary: 10}); err != nil {
				t.Fatal(err)
			}

			baseDir := filepath.Join(t.TempDir(), "base")
			baseManifest, err := maintenance.CreateBackup(t.Context(), env.backend, env.checkpointReq(t, 10, 1), maintenance.BackupOptions{
				Destination: baseDir, Limits: backupLimits(), EncryptionKey: key,
			})
			if err != nil {
				t.Fatalf("CreateBackup base: %v", err)
			}

			// Hop 1: insert row 2 at T=20.
			env.applyRow(t, 2, 20, 2, 200)
			inc1Dir := filepath.Join(t.TempDir(), "inc1")
			inc1, err := maintenance.CreateIncrementalBackup(t.Context(), env.backend, env.checkpointReq(t, 20, 2), baseDir, maintenance.BackupOptions{
				Destination: inc1Dir, Limits: backupLimits(), EncryptionKey: key,
			})
			if err != nil {
				t.Fatalf("CreateIncrementalBackup hop1: %v", err)
			}
			if len(inc1.InheritedFiles) == 0 {
				t.Fatalf("expected unchanged SST/base files to be inherited in hop1")
			}
			// Verify inherited files were NOT duplicated on disk in inc1Dir!
			for _, inh := range inc1.InheritedFiles {
				if _, statErr := os.Stat(filepath.Join(inc1Dir, inh.Name)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("inherited file %q was duplicated in inc1Dir", inh.Name)
				}
			}

			// Hop 2: insert row 3 at T=30.
			env.applyRow(t, 3, 30, 3, 300)
			inc2Dir := filepath.Join(t.TempDir(), "inc2")
			inc2, err := maintenance.CreateIncrementalBackup(t.Context(), env.backend, env.checkpointReq(t, 30, 3), inc1Dir, maintenance.BackupOptions{
				Destination: inc2Dir, Limits: backupLimits(), EncryptionKey: key,
			})
			if err != nil {
				t.Fatalf("CreateIncrementalBackup hop2: %v", err)
			}
			if inc2.ParentBackupID != inc1.BackupID || inc2.BaseSnapshotTS != 20 || inc2.SnapshotTS != 30 {
				t.Fatalf("unexpected hop2 lineage: %+v", inc2)
			}
			if len(inc2.InheritedFiles) <= len(baseManifest.Files)-1 {
				t.Fatalf("expected hop2 to inherit base + hop1 unchanged files; inherited=%d", len(inc2.InheritedFiles))
			}
			for _, inh := range inc2.InheritedFiles {
				if _, statErr := os.Stat(filepath.Join(inc2Dir, inh.Name)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("inherited file %q was duplicated in inc2Dir", inh.Name)
				}
			}

			verifier := neosepia.CheckpointVerifier{FenceVerifier: env.authority, Limits: neosepia.DefaultLimits()}

			// Broken chain (skipping hop1) must fail lineage check!
			if _, err := maintenance.RestoreIncremental(t.Context(), []string{baseDir, inc2Dir}, maintenance.RestoreOptions{
				Destination: filepath.Join(t.TempDir(), "bad-chain"), Limits: backupLimits(), Verifier: verifier, EncryptionKey: key,
			}); err == nil {
				t.Fatal("RestoreIncremental accepted chain with missing middle hop")
			}

			// Full 2-hop restore reconstructs exact database!
			restoredDir := filepath.Join(t.TempDir(), "restored")
			if _, err := maintenance.RestoreIncremental(t.Context(), []string{baseDir, inc1Dir, inc2Dir}, maintenance.RestoreOptions{
				Destination: restoredDir, Limits: backupLimits(), Verifier: verifier, EncryptionKey: key,
			}); err != nil {
				t.Fatalf("RestoreIncremental: %v", err)
			}

			cfg := env.config
			cfg.Directory = restoredDir
			restored, err := neosepia.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = restored.Close() }()

			for id, wantVal := range map[int64]int64{1: 100, 2: 200, 3: 300} {
				if v, ok := env.getRow(t, restored, 30, 3, id); !ok || v != wantVal {
					t.Fatalf("row %d = (%d, %v), want (%d, true)", id, v, ok, wantVal)
				}
			}
		})
	}
}

func TestF5PITREndToEnd(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		var key, archiveKey []byte
		if encrypted {
			name = "encrypted"
			key = randomKey(t)
			archiveKey = randomKey(t)
		}
		t.Run(name, func(t *testing.T) {
			archiveDir := filepath.Join(t.TempDir(), "archive")
			env := newF5BackupEnv(t, archiveDir, key, archiveKey)

			// T1: insert row A.
			env.applyRow(t, 1, 100, 1, 1000)

			// Take base backup at T1.
			baseDir := filepath.Join(t.TempDir(), "base")
			if _, err := maintenance.CreateBackup(t.Context(), env.backend, env.checkpointReq(t, 100, 1), maintenance.BackupOptions{
				Destination: baseDir, Limits: backupLimits(), EncryptionKey: key,
			}); err != nil {
				t.Fatalf("CreateBackup at T1: %v", err)
			}

			// T2: insert row B.
			env.applyRow(t, 2, 200, 2, 2000)
			// T3: insert row C (must be EXCLUDED when restoring to T2!).
			env.applyRow(t, 3, 300, 3, 3000)

			verifier := neosepia.CheckpointVerifier{
				FenceVerifier: env.authority,
				Limits:        neosepia.DefaultLimits(),
				EncryptionKey: key,
			}

			// Target before base backup fails cleanly.
			if _, err := maintenance.RestorePITR(t.Context(), baseDir, archiveDir, 50, maintenance.RestoreOptions{
				Destination: filepath.Join(t.TempDir(), "too-early"), Limits: backupLimits(), Verifier: verifier, EncryptionKey: key, WALArchiveKey: archiveKey,
			}); !errors.Is(err, maintenance.ErrInvalid) {
				t.Fatalf("PITR before base err = %v, want ErrInvalid", err)
			}

			// PITR to T2 = 200!
			pitrDest := filepath.Join(t.TempDir(), "pitr-restored")
			pitrRes, err := maintenance.RestorePITR(t.Context(), baseDir, archiveDir, 200, maintenance.RestoreOptions{
				Destination:   pitrDest,
				Limits:        backupLimits(),
				Verifier:      verifier,
				EncryptionKey: key,
				WALArchiveKey: archiveKey,
			})
			if err != nil {
				t.Fatalf("RestorePITR: %v", err)
			}
			if pitrRes.BaseLogPosition != 1 || pitrRes.RecoveredLogPos != 2 || pitrRes.RecoveredCommitTS != 200 || pitrRes.ReplayedSegments != 1 {
				t.Fatalf("unexpected PITRResult: %+v", pitrRes)
			}

			// Open restored DB and verify Row A & Row B exist, Row C does NOT exist!
			openCfg := env.config
			openCfg.Directory = pitrDest
			openCfg.WALArchiveDir = ""
			openCfg.WALArchiveKey = nil
			restored, err := neosepia.Open(t.Context(), openCfg)
			if err != nil {
				t.Fatalf("neosepia.Open PITR restored: %v", err)
			}
			defer func() { _ = restored.Close() }()

			st, err := restored.State(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if st.LogPosition != 2 || st.CommitTS != 200 {
				t.Fatalf("restored state = %+v, want pos=2 ts=200", st)
			}
			if v, ok := env.getRow(t, restored, 200, 2, 1); !ok || v != 1000 {
				t.Fatalf("row A = (%d, %v), want (1000, true)", v, ok)
			}
			if v, ok := env.getRow(t, restored, 200, 2, 2); !ok || v != 2000 {
				t.Fatalf("row B = (%d, %v), want (2000, true)", v, ok)
			}
			if _, ok := env.getRow(t, restored, 200, 2, 3); ok {
				t.Fatal("row C (from T3) MUST NOT exist after PITR to T2")
			}
		})
	}
}
