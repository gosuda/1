package neosepia

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func makePhase4StrBatch(t *testing.T, fixture backendFixture, txNum uint64, ts model.Timestamp, target storage.ObjectRef, cols []storage.ColumnLayout, k int64, values ...model.Value) storage.MutationBatch {
	t.Helper()
	colIDs := make([]model.ColumnID, len(values))
	for i := range values {
		colIDs[i] = cols[i].ID
	}
	mut := storage.NewUpsertMutation(target, backendKey(k), colIDs, values)
	mut.Ordinal = 1
	b := storage.MutationBatch{
		TxID:            model.TxnID{byte(txNum >> 8), byte(txNum)},
		CommitTS:        ts,
		Epoch:           fixture.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         []storage.ObjectLayout{{Target: target, Columns: cols}},
		Mutations:       []storage.Mutation{mut},
	}
	var err error
	b.ChecksumCRC, err = b.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatalf("ComputeChecksum: %v", err)
	}
	return b
}

func makeValidEncryptionMetaBytes(key []byte) []byte {
	c, err := newStorageCipher(key)
	if err != nil {
		panic(err)
	}
	var payload [encryptionMetaSize]byte
	copy(payload[0:4], encryptionMagic)
	binary.BigEndian.PutUint32(payload[4:8], encryptionMetaVersion)
	copy(payload[8:40], c.fingerprint[:])
	sum := sha256.Sum256(payload[:40])
	copy(payload[40:72], sum[:])
	return payload[:]
}

func TestPhase4StorageP1EncryptionMetaPendingRecovery(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	validPending := makeValidEncryptionMetaBytes(key)
	state := databaseStateFixture()

	newConfig := func(t *testing.T, dir string, encKey []byte) (Config, *txn.FenceAuthority) {
		t.Helper()
		authority, err := txn.NewFenceAuthority([32]byte{1})
		if err != nil {
			t.Fatal(err)
		}
		return Config{
			Directory:     dir,
			Scale:         state.Scales[0].Scale,
			KeyEncoder:    backendIntEncoder{},
			FenceVerifier: authority,
			EncryptionKey: encKey,
		}, authority
	}

	validCases := []struct {
		name    string
		content []byte
		encKey  []byte
	}{
		{name: "FreshEmptyPending", content: nil, encKey: key},
		{name: "FreshPartialMagic1", content: []byte("N"), encKey: key},
		{name: "FreshPartialMagic2", content: []byte("NS"), encKey: key},
		{name: "FreshPartialMagic3", content: []byte("NSE"), encKey: key},
		{name: "FreshPartialPayload", content: validPending[:16], encKey: key},
		{name: "FreshCompletePending", content: validPending, encKey: key},
		{name: "FreshUnencryptedStalePending", content: []byte("NS"), encKey: nil},
	}
	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pendingPath := filepath.Join(dir, encryptionMetaPending)
			if err := os.WriteFile(pendingPath, tc.content, 0600); err != nil {
				t.Fatalf("write pending file: %v", err)
			}
			cfg, authority := newConfig(t, dir, tc.encKey)
			backend, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatalf("Open with valid stale %s failed: %v", encryptionMetaPending, err)
			}
			if _, statErr := os.Stat(pendingPath); !errors.Is(statErr, os.ErrNotExist) {
				_ = backend.Close()
				t.Fatalf("expected %s to be removed on Open, statErr=%v", encryptionMetaPending, statErr)
			}
			metaPath := filepath.Join(dir, encryptionMetaName)
			_, metaErr := os.Stat(metaPath)
			if tc.encKey != nil && metaErr != nil {
				_ = backend.Close()
				t.Fatalf("expected %s to exist for encrypted DB: %v", encryptionMetaName, metaErr)
			}
			if tc.encKey == nil && !errors.Is(metaErr, os.ErrNotExist) {
				_ = backend.Close()
				t.Fatalf("expected no %s for unencrypted DB, statErr=%v", encryptionMetaName, metaErr)
			}

			fixture := backendFixture{backend: backend, config: cfg, authority: authority, epoch: state.Closed.Epoch}
			if _, err := backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 7, 9)); err != nil {
				_ = backend.Close()
				t.Fatalf("ApplyMutations after pending recovery: %v", err)
			}
			if err := backend.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			reopened, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatalf("reopen after recovery: %v", err)
			}
			defer func() {
				if err := reopened.Close(); err != nil {
					t.Errorf("reopened Close: %v", err)
				}
			}()
			fixture.backend = reopened
			reader := fixture.snapshot(t, 10)
			if got := backendBalance(t, reader, 0); got != 7 {
				t.Fatalf("reopened balance = %d, want 7", got)
			}
		})
	}

	t.Run("InitializedStalePendingRemoved", func(t *testing.T) {
		dir := t.TempDir()
		cfg, authority := newConfig(t, dir, key)
		backend, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatalf("initial Open: %v", err)
		}
		fixture := backendFixture{backend: backend, config: cfg, authority: authority, epoch: state.Closed.Epoch}
		if _, err := backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 42, 99)); err != nil {
			_ = backend.Close()
			t.Fatalf("ApplyMutations: %v", err)
		}
		if err := backend.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		pendingPath := filepath.Join(dir, encryptionMetaPending)
		if err := os.WriteFile(pendingPath, validPending, 0600); err != nil {
			t.Fatalf("write stale pending: %v", err)
		}
		reopened, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatalf("reopen with stale pending in initialized DB: %v", err)
		}
		defer func() {
			if err := reopened.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
		if _, statErr := os.Stat(pendingPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("expected stale %s to be removed on reopen, statErr=%v", encryptionMetaPending, statErr)
		}
		fixture.backend = reopened
		reader := fixture.snapshot(t, 10)
		if got := backendBalance(t, reader, 0); got != 42 {
			t.Fatalf("balance after reopen = %d, want 42", got)
		}
	})

	corruptCases := []struct {
		name    string
		content []byte
	}{
		{name: "BadByte1", content: []byte("X")},
		{name: "BadByte3", content: []byte("NSX")},
		{name: "BadVersionMagic", content: []byte("NSE2rest")},
		{name: "GarbagePayload", content: []byte("not-a-neosepia-file")},
	}
	for _, tc := range corruptCases {
		t.Run("Corrupt_"+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pendingPath := filepath.Join(dir, encryptionMetaPending)
			if err := os.WriteFile(pendingPath, tc.content, 0600); err != nil {
				t.Fatalf("write corrupt pending: %v", err)
			}
			cfg, _ := newConfig(t, dir, key)
			backend, err := Open(t.Context(), cfg)
			if backend != nil {
				_ = backend.Close()
			}
			if !errors.Is(err, ErrIncompatibleFormat) {
				t.Fatalf("Open with corrupt %s (%q) = %v, want ErrIncompatibleFormat", encryptionMetaPending, tc.content, err)
			}
		})
	}

	t.Run("NonRegularDirectoryPending", func(t *testing.T) {
		dir := t.TempDir()
		pendingPath := filepath.Join(dir, encryptionMetaPending)
		if err := os.Mkdir(pendingPath, 0700); err != nil {
			t.Fatalf("mkdir pending: %v", err)
		}
		cfg, _ := newConfig(t, dir, key)
		backend, err := Open(t.Context(), cfg)
		if backend != nil {
			_ = backend.Close()
		}
		if !errors.Is(err, ErrIncompatibleFormat) {
			t.Fatalf("Open with directory %s = %v, want ErrIncompatibleFormat", encryptionMetaPending, err)
		}
	})
}

func TestPhase4StorageP1PreWALBudgetValidation(t *testing.T) {
	target := encTestTarget()
	strCol := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}},
	}
	twoStrCols := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}},
		{ID: 2, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}},
	}

	assertCleanBackend := func(t *testing.T, fixture backendFixture) {
		t.Helper()
		fixture.backend.mu.Lock()
		uncertain := fixture.backend.metadataUncertain
		hasPending := fixture.backend.pending != nil
		fixture.backend.mu.Unlock()
		if uncertain || hasPending {
			t.Fatalf("backend poisoned after pre-WAL rejection: metadataUncertain=%v pending=%v", uncertain, hasPending)
		}
		st, err := fixture.backend.State(t.Context())
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if st.LogPosition != 0 {
			t.Fatalf("LogPosition = %d, want 0", st.LogPosition)
		}
		entries, err := os.ReadDir(filepath.Join(fixture.config.Directory, "commits"))
		if err != nil {
			t.Fatalf("ReadDir commits: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected 0 WAL files after pre-WAL rejection, found %d", len(entries))
		}
	}

	t.Run("ExceedsMutationBytes", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MutationBytes = 128
		limits.RowBytes = 4096
		fixture := openBackendFixtureWithLimits(t, limits)

		oversized := makePhase4StrBatch(t, fixture, 1, 10, target, strCol, 1, model.StringVal(string(bytes.Repeat([]byte("a"), 256))))
		_, err := fixture.backend.ApplyMutations(t.Context(), oversized)
		if !errors.Is(err, columnar.ErrBatchBudget) {
			t.Fatalf("ApplyMutations(>MutationBytes) err = %v, want ErrBatchBudget", err)
		}
		if errors.Is(err, storage.ErrCommitIndeterminate) {
			t.Fatalf("ApplyMutations(>MutationBytes) must not be ErrCommitIndeterminate: %v", err)
		}
		assertCleanBackend(t, fixture)

		valid := makePhase4StrBatch(t, fixture, 2, 20, target, strCol, 1, model.StringVal("ok"))
		res, err := fixture.backend.ApplyMutations(t.Context(), valid)
		if err != nil || res.LogPosition != 1 {
			t.Fatalf("subsequent valid ApplyMutations = (%+v, %v), want pos=1 nil err", res, err)
		}
	})

	t.Run("ExceedsWALBytes", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MutationBytes = 512
		limits.WALBytes = 256
		limits.RowBytes = 4096
		fixture := openBackendFixtureWithLimits(t, limits)

		m1 := storage.NewUpsertMutation(target, backendKey(1), []model.ColumnID{1}, []model.Value{model.StringVal(string(bytes.Repeat([]byte("a"), 120)))})
		m1.Ordinal = 1
		m2 := storage.NewUpsertMutation(target, backendKey(2), []model.ColumnID{1}, []model.Value{model.StringVal(string(bytes.Repeat([]byte("b"), 120)))})
		m2.Ordinal = 2
		oversized := storage.MutationBatch{
			TxID:            model.TxnID{1},
			CommitTS:        10,
			Epoch:           fixture.epoch,
			EncodingVersion: storage.MutationEncodingVersion,
			Layouts:         []storage.ObjectLayout{{Target: target, Columns: strCol}},
			Mutations:       []storage.Mutation{m1, m2},
		}
		var err error
		oversized.ChecksumCRC, err = oversized.ComputeChecksum(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		_, err = fixture.backend.ApplyMutations(t.Context(), oversized)
		if !errors.Is(err, columnar.ErrBatchBudget) {
			t.Fatalf("ApplyMutations(>WALBytes) err = %v, want ErrBatchBudget", err)
		}
		if errors.Is(err, storage.ErrCommitIndeterminate) {
			t.Fatalf("ApplyMutations(>WALBytes) must not be ErrCommitIndeterminate: %v", err)
		}
		assertCleanBackend(t, fixture)

		// Verify WALBytes allows smaller valid commit cleanly.
		fixture.backend.mu.Lock()
		fixture.backend.limits.WALBytes = 1024
		fixture.backend.mu.Unlock()
		valid := makePhase4StrBatch(t, fixture, 2, 20, target, strCol, 1, model.StringVal("ok"))
		res, err := fixture.backend.ApplyMutations(t.Context(), valid)
		if err != nil || res.LogPosition != 1 {
			t.Fatalf("subsequent valid ApplyMutations = (%+v, %v), want pos=1 nil err", res, err)
		}
	})

	t.Run("ExceedsRowBytesSingleAndMultiColumnAndMissingValue", func(t *testing.T) {
		limits := DefaultLimits()
		limits.RowBytes = 64
		limits.MutationBytes = 4096
		fixture := openBackendFixtureWithLimits(t, limits)

		// Single value > RowBytes.
		bigSingle := makePhase4StrBatch(t, fixture, 1, 10, target, strCol, 1, model.StringVal(string(bytes.Repeat([]byte("x"), 128))))
		_, err := fixture.backend.ApplyMutations(t.Context(), bigSingle)
		if !errors.Is(err, model.ErrValueBudget) {
			t.Fatalf("ApplyMutations(single > RowBytes) err = %v, want ErrValueBudget", err)
		}
		if errors.Is(err, storage.ErrCommitIndeterminate) {
			t.Fatalf("ApplyMutations(single > RowBytes) must not be ErrCommitIndeterminate: %v", err)
		}
		assertCleanBackend(t, fixture)

		// Combined row values > RowBytes (40 + 40 = 80 > 64).
		bigCombined := makePhase4StrBatch(t, fixture, 2, 15, target, twoStrCols, 1,
			model.StringVal(string(bytes.Repeat([]byte("a"), 40))),
			model.StringVal(string(bytes.Repeat([]byte("b"), 40))),
		)
		_, err = fixture.backend.ApplyMutations(t.Context(), bigCombined)
		if !errors.Is(err, model.ErrValueBudget) {
			t.Fatalf("ApplyMutations(combined > RowBytes) err = %v, want ErrValueBudget", err)
		}
		assertCleanBackend(t, fixture)

		// Layout MissingValue > RowBytes.
		bigMissingLayout := []storage.ColumnLayout{
			{
				ID:              1,
				Type:            model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC},
				HasMissingValue: true,
				MissingValue:    model.StringVal(string(bytes.Repeat([]byte("m"), 128))),
			},
		}
		bigMissing := makePhase4StrBatch(t, fixture, 3, 18, target, bigMissingLayout, 1, model.StringVal("tiny"))
		_, err = fixture.backend.ApplyMutations(t.Context(), bigMissing)
		if !errors.Is(err, model.ErrValueBudget) {
			t.Fatalf("ApplyMutations(MissingValue > RowBytes) err = %v, want ErrValueBudget", err)
		}
		assertCleanBackend(t, fixture)

		// Direct check that copyObjectLayout enforces maxRowBytes rather than hardcoded 64<<20.
		if _, copyErr := copyObjectLayout(t.Context(), storage.ObjectLayout{Target: target, Columns: bigMissingLayout}, 64); !errors.Is(copyErr, model.ErrValueBudget) {
			t.Fatalf("copyObjectLayout(>maxRowBytes) = %v, want ErrValueBudget", copyErr)
		}

		// Subsequent small write succeeds cleanly at pos 1.
		valid := makePhase4StrBatch(t, fixture, 4, 20, target, strCol, 1, model.StringVal("small"))
		res, err := fixture.backend.ApplyMutations(t.Context(), valid)
		if err != nil || res.LogPosition != 1 {
			t.Fatalf("subsequent valid ApplyMutations = (%+v, %v), want pos=1 nil err", res, err)
		}
	})
}

func TestPhase4StorageP1FlushErrBackpressureAndRecovery(t *testing.T) {
	target := encTestTarget()
	strCol := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}},
	}

	modes := []struct {
		name          string
		enableOnePath bool
	}{
		{name: "GroupCommitPath", enableOnePath: false},
		{name: "SingleCommitPath", enableOnePath: true},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.ResidentBytes = 128 << 10
			limits.WriteBytes = 128 << 10
			limits.OverlayBytes = 64 << 10
			limits.PublicationBytes = 128 << 10
			if !memtableEnabled(limits) {
				t.Fatalf("expected memtableEnabled=true for test limits (soft=%d)", memtableMaxBytes(limits))
			}
			hardMax := memtableHardMaxBytes(limits)
			if hardMax != 128<<10 {
				t.Fatalf("memtableHardMaxBytes = %d, want %d", hardMax, 128<<10)
			}

			fixture := openBackendFixtureWithLimits(t, limits)
			if mode.enableOnePath {
				fixture.backend.mu.Lock()
				fixture.backend.commitFault = func(commitPoint) error { return nil }
				fixture.backend.mu.Unlock()
			}

			// Inject a persistent flush error (MaxRuns=0 makes flushToRuns fail with ErrBatchBudget).
			origRuns := fixture.backend.limits.Runs
			fixture.backend.mu.Lock()
			fixture.backend.limits.Runs = 0
			fixture.backend.mu.Unlock()

			payload := string(bytes.Repeat([]byte("p"), 20<<10))
			var txNum uint64
			var lastTS model.Timestamp
			var committedKeys []int64

			// Write until flush has failed AND memtable reaches memtableHardMaxBytes.
			for range 16 {
				txNum++
				lastTS += 10
				k := int64(txNum)
				b := makePhase4StrBatch(t, fixture, txNum, lastTS, target, strCol, k, model.StringVal(payload))
				res, err := fixture.backend.ApplyMutations(t.Context(), b)
				if err != nil {
					t.Fatalf("unexpected error before hitting hard max (tx=%d): %v", txNum, err)
				}
				if res.LogPosition != txNum {
					t.Fatalf("tx %d LogPosition = %d, want %d", txNum, res.LogPosition, txNum)
				}
				committedKeys = append(committedKeys, k)

				fixture.backend.mu.Lock()
				curBytes := fixture.backend.memtable.totalBytes
				curFlushErr := fixture.backend.flushErr
				fixture.backend.mu.Unlock()
				if curFlushErr != nil && curBytes >= hardMax {
					break
				}
			}

			fixture.backend.mu.Lock()
			beforeBytes := fixture.backend.memtable.totalBytes
			recordedFlushErr := fixture.backend.flushErr
			fixture.backend.mu.Unlock()
			if recordedFlushErr == nil {
				t.Fatalf("expected non-nil backend.flushErr after crossing soft threshold")
			}
			if beforeBytes < hardMax {
				t.Fatalf("memtable.totalBytes = %d, want >= hardMax %d", beforeBytes, hardMax)
			}

			// Next write MUST trigger backpressure pre-WAL and surface the flush error cleanly.
			blockedTx := txNum + 1
			blockedTS := lastTS + 10
			blockedBatch := makePhase4StrBatch(t, fixture, blockedTx, blockedTS, target, strCol, int64(blockedTx), model.StringVal(payload))
			_, err := fixture.backend.ApplyMutations(t.Context(), blockedBatch)
			if !errors.Is(err, columnar.ErrBatchBudget) {
				t.Fatalf("backpressure ApplyMutations err = %v, want ErrBatchBudget", err)
			}
			if errors.Is(err, storage.ErrCommitIndeterminate) {
				t.Fatalf("backpressure error must be clean pre-WAL, got ErrCommitIndeterminate: %v", err)
			}

			// Verify memtable did not grow, backend is not uncertain, and no WAL file was created for blockedTx.
			fixture.backend.mu.Lock()
			afterBytes := fixture.backend.memtable.totalBytes
			uncertain := fixture.backend.metadataUncertain
			hasPending := fixture.backend.pending != nil
			fixture.backend.mu.Unlock()
			if afterBytes != beforeBytes {
				t.Fatalf("memtable grew during backpressure: before=%d after=%d", beforeBytes, afterBytes)
			}
			if uncertain || hasPending {
				t.Fatalf("backend wedged on backpressure: metadataUncertain=%v pending=%v", uncertain, hasPending)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.config.Directory, "commits", commitName(blockedTx))); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("expected no WAL segment for pre-WAL rejected pos %d, statErr=%v", blockedTx, statErr)
			}
			st, err := fixture.backend.State(t.Context())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if st.LogPosition != txNum {
				t.Fatalf("LogPosition after rejected write = %d, want %d", st.LogPosition, txNum)
			}

			// Clear the persistent flush fault; retrying blockedBatch must flush, clear flushErr, and commit cleanly.
			fixture.backend.mu.Lock()
			fixture.backend.limits.Runs = origRuns
			fixture.backend.mu.Unlock()

			res, err := fixture.backend.ApplyMutations(t.Context(), blockedBatch)
			if err != nil {
				t.Fatalf("ApplyMutations retry after flush fault cleared: %v", err)
			}
			if res.LogPosition != blockedTx {
				t.Fatalf("recovered LogPosition = %d, want %d", res.LogPosition, blockedTx)
			}
			committedKeys = append(committedKeys, int64(blockedTx))

			fixture.backend.mu.Lock()
			recoveredFlushErr := fixture.backend.flushErr
			recoveredBytes := fixture.backend.memtable.totalBytes
			fixture.backend.mu.Unlock()
			if recoveredFlushErr != nil {
				t.Fatalf("expected backend.flushErr == nil after successful flush, got %v", recoveredFlushErr)
			}
			if recoveredBytes >= beforeBytes {
				t.Fatalf("expected memtable to shrink after flush: before=%d after=%d", beforeBytes, recoveredBytes)
			}

			// Verify all committed keys (flushed SSTs + new memtable entry) are readable.
			reader := fixture.snapshot(t, blockedTS)
			for _, k := range committedKeys {
				row, getErr := reader.Get(t.Context(), storage.ReadRequest{
					Target:    target,
					Key:       backendKey(k),
					ColumnIDs: []model.ColumnID{1},
					Layout:    strCol,
				})
				if getErr != nil {
					t.Fatalf("Get(key=%d) after recovery: %v", k, getErr)
				}
				if row.Values[0].StrVal != payload {
					t.Fatalf("key %d payload mismatch (len=%d, want %d)", k, len(row.Values[0].StrVal), len(payload))
				}
			}
		})
	}
}
