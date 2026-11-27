package neosepia

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestPhase4RunsCheckpointEncryptedBaseManifest(t *testing.T) {
	f := newF5Fixture(t)
	// Use a string primary key so SST LowerKey/UpperKey bounds stored in
	// base-*.runs would directly contain plaintext key strings if unencrypted.
	strKeyType := model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}
	strLayouts := []storage.ObjectLayout{{
		Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Columns: []storage.ColumnLayout{
			{ID: 1, Type: strKeyType},
			{ID: 2, Type: f.intType},
		},
	}}
	encodeStrKey := func(k string) []byte {
		t.Helper()
		enc, err := f.encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: strKeyType}}, []model.Value{model.StringVal(k)}, 256)
		if err != nil {
			t.Fatal(err)
		}
		return enc
	}
	makeStrBatch := func(txByte byte, commitTS model.Timestamp, k string, val int64) storage.MutationBatch {
		t.Helper()
		batch := storage.MutationBatch{
			TxID:            model.TxnID{txByte},
			CommitTS:        commitTS,
			Epoch:           f.epoch,
			EncodingVersion: storage.MutationEncodingVersion,
			Layouts:         strLayouts,
			Mutations: []storage.Mutation{{
				Op:        storage.MutationUpsert,
				Target:    strLayouts[0].Target,
				Key:       encodeStrKey(k),
				Ordinal:   1,
				ColumnIDs: []model.ColumnID{1, 2},
				Values:    []model.Value{model.StringVal(k), model.Int64Val(val)},
			}},
		}
		var err error
		batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return batch
	}
	readStrRow := func(backend *Backend, snapTS model.Timestamp, pos uint64, k string) int64 {
		t.Helper()
		fence, err := f.authority.Issue(t.Context(), snapTS, pos, f.epoch)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: snapTS, Fence: fence})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := reader.Close(); err != nil {
				t.Error(err)
			}
		}()
		row, err := reader.Get(t.Context(), storage.ReadRequest{
			Target:    strLayouts[0].Target,
			Key:       encodeStrKey(k),
			ColumnIDs: []model.ColumnID{2},
			Layout:    strLayouts[0].Columns,
		})
		if err != nil {
			t.Fatalf("read %q: %v", k, err)
		}
		return row.Values[0].IntVal
	}

	key := randomAES256Key(t)
	wrongKey := randomAES256Key(t)
	keyAlpha := "SECRET_BASE_KEY_ALPHA_001"
	keyOmega := "SECRET_BASE_KEY_OMEGA_999"

	cfg := Config{
		Directory:     f.dir,
		Scale:         f.scale,
		KeyEncoder:    f.encoder,
		FenceVerifier: f.authority,
		EncryptionKey: key,
	}
	db, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open encrypted db: %v", err)
	}
	if _, err := db.ApplyMutations(t.Context(), makeStrBatch(1, 100, keyAlpha, 111)); err != nil {
		t.Fatalf("ApplyMutations 1: %v", err)
	}
	if _, err := db.ApplyMutations(t.Context(), makeStrBatch(2, 200, keyOmega, 222)); err != nil {
		t.Fatalf("ApplyMutations 2: %v", err)
	}
	if err := db.Collect(t.Context(), storage.GCRequest{Boundary: 200}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	baseGen := db.state.BaseGeneration
	if baseGen == 0 {
		t.Fatal("expected Collect to publish a base manifest generation")
	}

	// Verify checkpoint creation and verification work with an encrypted base-*.runs.
	fence, err := f.authority.Issue(t.Context(), 200, 2, f.epoch)
	if err != nil {
		t.Fatal(err)
	}
	cpHandle, err := db.Checkpoint(t.Context(), storage.CheckpointRequest{
		Snapshot: storage.SnapshotSpec{Timestamp: 200, Fence: fence},
	})
	if err != nil {
		t.Fatalf("Checkpoint after compaction: %v", err)
	}
	cpManifest := checkpointManifest(t, cpHandle)
	cpDir := t.TempDir()
	copyCheckpointFiles(t, cpHandle, cpDir)
	if err := cpHandle.Close(); err != nil {
		t.Fatalf("Checkpoint Close: %v", err)
	}
	cpRoot, err := os.OpenRoot(cpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cpRoot.Close(); err != nil {
			t.Error(err)
		}
	}()
	verifier := CheckpointVerifier{
		FenceVerifier: f.authority,
		Limits:        DefaultLimits(),
		ExtraFiles:    []string{encryptionMetaName},
		EncryptionKey: key,
	}
	if err := verifier.VerifyCheckpoint(t.Context(), cpRoot, cpManifest); err != nil {
		t.Fatalf("VerifyCheckpoint with valid key: %v", err)
	}
	wrongVerifier := verifier
	wrongVerifier.EncryptionKey = wrongKey
	if err := wrongVerifier.VerifyCheckpoint(t.Context(), cpRoot, cpManifest); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("VerifyCheckpoint with wrong key: got %v, want ErrInvalidEncryptionKey", err)
	}
	nilVerifier := verifier
	nilVerifier.EncryptionKey = nil
	if err := nilVerifier.VerifyCheckpoint(t.Context(), cpRoot, cpManifest); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("VerifyCheckpoint with missing key: got %v, want ErrInvalidEncryptionKey", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close encrypted db: %v", err)
	}

	// Inspect runs/base-*.runs on disk: must start with NSE1 and leak zero plaintext keys.
	runsDir := filepath.Join(f.dir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	baseFilesChecked := 0
	alphaEnc := encodeStrKey(keyAlpha)
	omegaEnc := encodeStrKey(keyOmega)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "base-") || !strings.HasSuffix(name, ".runs") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(runsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(raw, []byte(encryptionMagic)) {
			t.Fatalf("%s missing NSE1 magic prefix: got %q", name, raw[:min(8, len(raw))])
		}
		for _, forbidden := range [][]byte{
			[]byte(baseManifestMagic),
			[]byte(keyAlpha),
			[]byte(keyOmega),
			alphaEnc,
			omegaEnc,
		} {
			if bytes.Contains(raw, forbidden) {
				t.Fatalf("%s leaked plaintext bytes %q on disk", name, forbidden)
			}
		}
		baseFilesChecked++
	}
	if baseFilesChecked == 0 {
		t.Fatal("expected at least one encrypted base-*.runs file on disk")
	}

	// Verify loadRunBaseWithCipher directly against encrypted and plaintext base manifests.
	runsRoot, err := os.OpenRoot(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := runsRoot.Close(); err != nil {
			t.Error(err)
		}
	}()
	validCipher, err := newStorageCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	wrongCipher, err := newStorageCipher(wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	lim := DefaultLimits()
	loadedBase, err := loadRunBaseWithCipher(t.Context(), runsRoot, baseGen, lim.MetadataBytes, lim.IdentityBytes, lim.MaxFiles, validCipher)
	if err != nil {
		t.Fatalf("loadRunBaseWithCipher(valid): %v", err)
	}
	if _, err := loadRunBaseWithCipher(t.Context(), runsRoot, baseGen, lim.MetadataBytes, lim.IdentityBytes, lim.MaxFiles, wrongCipher); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("loadRunBaseWithCipher(wrong key): got %v, want ErrInvalidEncryptionKey", err)
	}
	if _, err := loadRunBaseWithCipher(t.Context(), runsRoot, baseGen, lim.MetadataBytes, lim.IdentityBytes, lim.MaxFiles, nil); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("loadRunBaseWithCipher(missing key): got %v, want ErrInvalidEncryptionKey", err)
	}
	// Plaintext base manifest must be rejected when a cipher is configured.
	plainRoot := commitTestRoot(t)
	if err := writeRunBase(t.Context(), plainRoot, loadedBase, lim.MetadataBytes, lim.IdentityBytes, lim.MaxFiles); err != nil {
		t.Fatalf("writeRunBase plaintext: %v", err)
	}
	if _, err := loadRunBaseWithCipher(t.Context(), plainRoot, baseGen, lim.MetadataBytes, lim.IdentityBytes, lim.MaxFiles, validCipher); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("loadRunBaseWithCipher(plaintext with cipher): got %v, want ErrInvalidEncryptionKey", err)
	}

	// Reopen DB with wrong key -> ErrInvalidEncryptionKey.
	wrongCfg := cfg
	wrongCfg.EncryptionKey = wrongKey
	if _, err := Open(t.Context(), wrongCfg); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("Open with wrong key: got %v, want ErrInvalidEncryptionKey", err)
	}

	// Reopen DB with missing key -> ErrInvalidEncryptionKey.
	nilCfg := cfg
	nilCfg.EncryptionKey = nil
	if _, err := Open(t.Context(), nilCfg); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("Open with missing key: got %v, want ErrInvalidEncryptionKey", err)
	}

	// Reopen DB with valid key -> succeeds and returns original values.
	reopened, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open with valid key: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	if got := readStrRow(reopened, 200, 2, keyAlpha); got != 111 {
		t.Fatalf("reopened %q: got %d, want 111", keyAlpha, got)
	}
	if got := readStrRow(reopened, 200, 2, keyOmega); got != 222 {
		t.Fatalf("reopened %q: got %d, want 222", keyOmega, got)
	}
}

func TestPhase4RunsCheckpointAdmitBudgetNoUnderflow(t *testing.T) {
	batch := runPreparationFixture(t)
	root := commitTestRoot(t)
	identity, err := batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareMutationRuns(t.Context(), root, 1, batch, identity, preparationLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	})
	files := prepared.detach()
	if len(files) < 2 {
		t.Fatalf("expected multi-run fixture (>=2 files), got %d", len(files))
	}

	// Measure exact resident costs and index bytes of each run so we can pick
	// budgets right inside the uint64 underflow window.
	lim := runLimits()
	scratch := make([]byte, 4096)
	costs := make([]uint64, len(files))
	indexes := make([]uint64, len(files))
	for i, desc := range files {
		run, err := openPreparedRun(t.Context(), root, desc, lim, lim.MaxIndexBytes, scratch)
		if err != nil {
			t.Fatalf("measure run %d: %v", i, err)
		}
		costs[i] = run.residentCost()
		indexes[i] = uint64(cap(run.core.index))
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}

	pointerBytes := uint64(len(files)) * uint64(unsafe.Sizeof((*sstFile)(nil)))
	nativeBytes := uint64(unsafe.Sizeof(sstFile{}))

	// Tight budget 1: remaining after scratch is >= indexes[0] but < costs[0].
	// Previously openPreparedRun succeeded on run 0 and `used += costs[0]`
	// exceeded maxBytes, causing `maxBytes - used` to underflow on run 1.
	target1 := indexes[0] + nativeBytes/2
	if target1 >= costs[0] {
		target1 = costs[0] - 1
	}
	budget1 := pointerBytes + 2*target1

	// Tight budget 2: run 0 fits completely, but run 1 does not fit.
	target2 := costs[0] + nativeBytes/2
	if target2 >= costs[0]+costs[1] {
		target2 = costs[0] + costs[1] - 1
	}
	budget2 := pointerBytes + 2*target2

	decision := runDecision{
		Position:      1,
		TxID:          batch.TxID,
		CommitTS:      batch.CommitTS,
		Identity:      identity,
		MutationCount: batch.Count(),
	}
	coalesced, err := makeCoalescedDecision([]runDecision{decision})
	if err != nil {
		t.Fatal(err)
	}
	commitTSs := []model.Timestamp{decision.CommitTS}

	for _, tc := range []struct {
		name   string
		budget uint64
	}{
		{name: "first_run_underflow_window", budget: budget1},
		{name: "second_run_exceeds_budget", budget: budget2},
	} {
		t.Run("admitDecision/"+tc.name, func(t *testing.T) {
			set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 16, MaxPins: 4})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := set.Close(); err != nil {
					t.Error(err)
				}
			}()
			if _, err := set.admitDecision(t.Context(), root, decision, files, lim, tc.budget); !errors.Is(err, columnar.ErrBatchBudget) {
				t.Fatalf("admitDecision(budget=%d): got %v, want columnar.ErrBatchBudget", tc.budget, err)
			}
		})
		t.Run("admitCoalescedDecision/"+tc.name, func(t *testing.T) {
			set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 16, MaxPins: 4})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := set.Close(); err != nil {
					t.Error(err)
				}
			}()
			if _, err := set.admitCoalescedDecision(t.Context(), root, coalesced, commitTSs, files, lim, tc.budget); !errors.Is(err, columnar.ErrBatchBudget) {
				t.Fatalf("admitCoalescedDecision(budget=%d): got %v, want columnar.ErrBatchBudget", tc.budget, err)
			}
		})
	}

	// Verify both paths succeed with adequate budget.
	for _, isCoalesced := range []bool{false, true} {
		set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 16, MaxPins: 4})
		if err != nil {
			t.Fatal(err)
		}
		var admission *runAdmission
		if isCoalesced {
			admission, err = set.admitCoalescedDecision(t.Context(), root, coalesced, commitTSs, files, lim, 1<<20)
		} else {
			admission, err = set.admitDecision(t.Context(), root, decision, files, lim, 1<<20)
		}
		if err != nil {
			_ = set.Close()
			t.Fatalf("adequate budget (coalesced=%v): %v", isCoalesced, err)
		}
		if err := admission.Close(); err != nil {
			_ = set.Close()
			t.Fatal(err)
		}
		if err := set.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPhase4RunsCheckpointHandleCloseOrder(t *testing.T) {
	t.Run("removeCheckpointDirectory_closes_lock_and_root_before_remove", func(t *testing.T) {
		parentDir := t.TempDir()
		cpName := checkpointPrefix + strings.Repeat("a", 32)
		cpPath := filepath.Join(parentDir, cpName)
		if err := os.MkdirAll(filepath.Join(cpPath, "runs"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cpPath, "runs", "member.sst"), []byte("payload"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cpPath, ".lock"), []byte(checkpointMagic), 0600); err != nil {
			t.Fatal(err)
		}

		parentRoot, err := os.OpenRoot(parentDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := parentRoot.Close(); err != nil {
				t.Error(err)
			}
		}()
		cpRoot, err := parentRoot.OpenRoot(cpName)
		if err != nil {
			t.Fatal(err)
		}
		lockFile, err := cpRoot.OpenFile(".lock", os.O_RDWR, 0)
		if err != nil {
			_ = cpRoot.Close()
			t.Fatal(err)
		}
		origRoot := cpRoot
		origLock := lockFile

		if err := removeCheckpointDirectory(parentRoot, &cpRoot, &lockFile, cpName); err != nil {
			t.Fatalf("removeCheckpointDirectory: %v", err)
		}
		if lockFile != nil {
			t.Fatal("expected lockFile pointer to be cleared to nil")
		}
		if cpRoot != nil {
			t.Fatal("expected cpRoot pointer to be cleared to nil")
		}
		if _, err := origLock.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("expected original lock handle to be closed, got %v", err)
		}
		if _, err := origRoot.Stat("."); err == nil {
			t.Fatal("expected original root handle to be closed")
		}
		if _, err := os.Stat(cpPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected checkpoint directory removed, stat err = %v", err)
		}
	})

	t.Run("checkpointHandle_Close_live_and_nil_root_branch", func(t *testing.T) {
		fixture := openBackendFixture(t)
		if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 100)); err != nil {
			t.Fatal(err)
		}
		fence, err := fixture.authority.Issue(t.Context(), 20, 1, fixture.epoch)
		if err != nil {
			t.Fatal(err)
		}
		cp, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{
			Snapshot: storage.SnapshotSpec{Timestamp: 20, Fence: fence},
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, ok := cp.(*checkpointHandle)
		if !ok {
			t.Fatalf("unexpected checkpoint handle type %T", cp)
		}
		cpPath := filepath.Join(fixture.config.Directory, handle.directory)
		origLock := handle.lock
		origRoot := handle.root
		if origLock == nil || origRoot == nil {
			t.Fatal("expected live checkpointHandle to hold open lock and root")
		}
		if err := handle.Close(); err != nil {
			t.Fatalf("checkpointHandle.Close: %v", err)
		}
		if handle.lock != nil || handle.root != nil || handle.parent != nil {
			t.Fatal("expected checkpointHandle handles to be nil after Close")
		}
		if _, err := origLock.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("expected checkpoint lock closed, got %v", err)
		}
		if _, err := origRoot.Stat("."); err == nil {
			t.Fatal("expected checkpoint root closed")
		}
		if _, err := os.Stat(cpPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected checkpoint directory removed, stat err = %v", err)
		}

		// Also exercise the handle.root == nil branch in checkpointHandle.Close.
		parentDir := t.TempDir()
		dirName := checkpointPrefix + strings.Repeat("b", 32)
		dirPath := filepath.Join(parentDir, dirName)
		if err := os.Mkdir(dirPath, 0700); err != nil {
			t.Fatal(err)
		}
		parentRoot, err := os.OpenRoot(parentDir)
		if err != nil {
			t.Fatal(err)
		}
		// Open an auxiliary lock file outside dirPath so parent.Remove(dirName)
		// tests closing handle.lock before removing the empty directory.
		lockPath := filepath.Join(parentDir, "aux.lock")
		auxLock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			_ = parentRoot.Close()
			t.Fatal(err)
		}
		nilRootHandle := &checkpointHandle{
			parent:    parentRoot,
			root:      nil,
			lock:      auxLock,
			directory: dirName,
		}
		if err := nilRootHandle.Close(); err != nil {
			t.Fatalf("nil-root checkpointHandle.Close: %v", err)
		}
		if nilRootHandle.lock != nil {
			t.Fatal("expected nilRootHandle.lock to be nil after Close")
		}
		if _, err := auxLock.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("expected auxLock closed before Remove, got %v", err)
		}
		if _, err := os.Stat(dirPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected empty checkpoint directory removed, stat err = %v", err)
		}
	})

	t.Run("cleanupCheckpointDirectory_orphan_with_and_without_lock", func(t *testing.T) {
		parentDir := t.TempDir()
		parentRoot, err := os.OpenRoot(parentDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := parentRoot.Close(); err != nil {
				t.Error(err)
			}
		}()

		// Case A: orphan directory with unlocked .lock marker + member files.
		withLockName := checkpointPrefix + strings.Repeat("c", 32)
		withLockPath := filepath.Join(parentDir, withLockName)
		if err := os.MkdirAll(filepath.Join(withLockPath, "commits"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(withLockPath, "commits", "00000000000000000001.commit"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(withLockPath, ".lock"), []byte(checkpointMagic), 0600); err != nil {
			t.Fatal(err)
		}
		collected, err := cleanupCheckpointDirectory(t.Context(), parentRoot, withLockName)
		if err != nil || !collected {
			t.Fatalf("cleanupCheckpointDirectory(with .lock): collected=%v err=%v", collected, err)
		}
		if _, err := os.Stat(withLockPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan with .lock still exists: %v", err)
		}

		// Case B: empty orphan directory without .lock (root must be closed before parent.Remove).
		noLockName := checkpointPrefix + strings.Repeat("d", 32)
		noLockPath := filepath.Join(parentDir, noLockName)
		if err := os.Mkdir(noLockPath, 0700); err != nil {
			t.Fatal(err)
		}
		collected, err = cleanupCheckpointDirectory(t.Context(), parentRoot, noLockName)
		if err != nil || !collected {
			t.Fatalf("cleanupCheckpointDirectory(without .lock): collected=%v err=%v", collected, err)
		}
		if _, err := os.Stat(noLockPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("empty orphan without .lock still exists: %v", err)
		}
	})
}
