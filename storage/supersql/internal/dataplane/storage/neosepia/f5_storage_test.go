package neosepia

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type f5Fixture struct {
	dir       string
	scale     clock.ScaleDescriptor
	encoder   storage.KeyEncoder
	authority *txn.FenceAuthority
	epoch     clock.Epoch
	layouts   []storage.ObjectLayout
	intType   model.Type
	strType   model.Type
}

func newF5Fixture(t *testing.T) f5Fixture {
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
	authority, err := txn.NewFenceAuthority([32]byte{19})
	if err != nil {
		t.Fatal(err)
	}
	intType := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	strType := model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}
	layouts := []storage.ObjectLayout{{
		Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Columns: []storage.ColumnLayout{
			{ID: 1, Type: intType},
			{ID: 2, Type: intType},
			{ID: 3, Type: strType},
		},
	}}
	return f5Fixture{
		dir:       filepath.Join(t.TempDir(), "db"),
		scale:     scale,
		encoder:   encoder,
		authority: authority,
		epoch:     clock.Epoch{Assurance: [16]byte{3}, Config: [32]byte{4}, LeapHistory: scale.LeapHistoryID},
		layouts:   layouts,
		intType:   intType,
		strType:   strType,
	}
}

func (f f5Fixture) encodeKey(t *testing.T, id int64) []byte {
	t.Helper()
	k, err := f.encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: f.intType}}, []model.Value{model.Int64Val(id)}, 128)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (f f5Fixture) makeBatch(t *testing.T, txByte byte, commitTS model.Timestamp, rows []struct {
	id  int64
	val int64
	txt string
}) storage.MutationBatch {
	t.Helper()
	batch := storage.MutationBatch{
		TxID:            model.TxnID{txByte},
		CommitTS:        commitTS,
		Epoch:           f.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         f.layouts,
	}
	for i, r := range rows {
		batch.Mutations = append(batch.Mutations, storage.Mutation{
			Op:        storage.MutationUpsert,
			Target:    f.layouts[0].Target,
			Key:       f.encodeKey(t, r.id),
			Ordinal:   uint64(i + 1),
			ColumnIDs: []model.ColumnID{1, 2, 3},
			Values:    []model.Value{model.Int64Val(r.id), model.Int64Val(r.val), model.StringVal(r.txt)},
		})
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func (f f5Fixture) readRow(t *testing.T, backend *Backend, snapTS model.Timestamp, pos uint64, id int64) (int64, string, bool) {
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
		Target:    f.layouts[0].Target,
		Key:       f.encodeKey(t, id),
		ColumnIDs: []model.ColumnID{2, 3},
		Layout:    f.layouts[0].Columns,
	})
	if errors.Is(err, storage.ErrRowNotFound) {
		return 0, "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row.Values[0].IntVal, row.Values[1].StrVal, true
}

func randomAES256Key(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestF5EncryptedLiveNeoSepiaDB(t *testing.T) {
	f := newF5Fixture(t)
	key := randomAES256Key(t)
	wrongKey := randomAES256Key(t)

	// Invalid key length rejected immediately.
	if _, err := Open(t.Context(), Config{
		Directory: f.dir, Scale: f.scale, KeyEncoder: f.encoder, FenceVerifier: f.authority,
		EncryptionKey: []byte("too-short"),
	}); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("short key error = %v, want ErrInvalidEncryptionKey", err)
	}

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

	// Write enough data (including >64KiB multi-chunk payload) to span multiple
	// NSE1 chunks inside both WAL and SST files.
	largeText := strings.Repeat("SECRET_PLAINTEXT_PAYLOAD_", 4000) // ~100 KiB
	if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 1, 100, []struct {
		id  int64
		val int64
		txt string
	}{
		{id: 1, val: 111, txt: "alpha-secret"},
		{id: 2, val: 222, txt: largeText},
	})); err != nil {
		t.Fatalf("ApplyMutations 1: %v", err)
	}
	// Trigger compaction so encrypted SST runs are compacted and rewritten encrypted.
	if err := db.Collect(t.Context(), storage.GCRequest{Boundary: 100}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 2, 200, []struct {
		id  int64
		val int64
		txt string
	}{
		{id: 3, val: 333, txt: "beta-secret"},
	})); err != nil {
		t.Fatalf("ApplyMutations 2: %v", err)
	}

	// Verify live checkpoint + CheckpointVerifier work on an encrypted DB.
	fence, err := f.authority.Issue(t.Context(), 200, 2, f.epoch)
	if err != nil {
		t.Fatal(err)
	}
	cpHandle, err := db.Checkpoint(t.Context(), storage.CheckpointRequest{
		Snapshot: storage.SnapshotSpec{Timestamp: 200, Fence: fence},
	})
	if err != nil {
		t.Fatalf("Checkpoint encrypted db: %v", err)
	}
	cpManifest, err := cpHandle.Manifest(t.Context(), storage.CheckpointManifestLimits{MaxBytes: 1 << 20, MaxFiles: 1024})
	if err != nil {
		t.Fatalf("Checkpoint Manifest: %v", err)
	}
	hasEncMetaInManifest := false
	for _, cf := range cpManifest.Files {
		if cf.Name == encryptionMetaName {
			hasEncMetaInManifest = true
		}
	}
	if !hasEncMetaInManifest {
		t.Fatalf("checkpoint manifest missing %s: %+v", encryptionMetaName, cpManifest.Files)
	}
	if err := cpHandle.Close(); err != nil {
		t.Fatalf("Checkpoint Close: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close encrypted db: %v", err)
	}

	// Verify on-disk SST and WAL files start with NSE1 and do not leak plaintext!
	runsEntries, err := os.ReadDir(filepath.Join(f.dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	sstChecked := 0
	for _, e := range runsEntries {
		if !strings.HasSuffix(e.Name(), ".sst") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(f.dir, "runs", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(raw, []byte(encryptionMagic)) {
			t.Fatalf("SST %s missing NSE1 magic prefix: %q", e.Name(), raw[:min(8, len(raw))])
		}
		if bytes.Contains(raw, []byte("SECRET_PLAINTEXT_PAYLOAD_")) || bytes.Contains(raw, []byte("alpha-secret")) {
			t.Fatalf("SST %s leaked plaintext on disk", e.Name())
		}
		sstChecked++
	}
	if sstChecked == 0 {
		t.Fatal("expected at least one encrypted SST file on disk")
	}

	commitsEntries, err := os.ReadDir(filepath.Join(f.dir, "commits"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range commitsEntries {
		if !strings.HasSuffix(e.Name(), ".wal") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(f.dir, "commits", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(raw, []byte(encryptionMagic)) {
			t.Fatalf("WAL %s missing NSE1 magic prefix", e.Name())
		}
		if bytes.Contains(raw, []byte("beta-secret")) {
			t.Fatalf("WAL %s leaked plaintext on disk", e.Name())
		}
	}

	// Reopening with wrong key fails fast with ErrInvalidEncryptionKey.
	wrongCfg := cfg
	wrongCfg.EncryptionKey = wrongKey
	if _, err := Open(t.Context(), wrongCfg); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("reopen wrong key error = %v, want ErrInvalidEncryptionKey", err)
	}

	// Reopening with missing key fails fast with ErrInvalidEncryptionKey.
	noKeyCfg := cfg
	noKeyCfg.EncryptionKey = nil
	if _, err := Open(t.Context(), noKeyCfg); !errors.Is(err, ErrInvalidEncryptionKey) {
		t.Fatalf("reopen missing key error = %v, want ErrInvalidEncryptionKey", err)
	}

	// Reopening with the right key recovers 100% cleanly and reads all rows!
	reopened, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("reopen right key: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()

	v1, s1, ok1 := f.readRow(t, reopened, 200, 2, 1)
	if !ok1 || v1 != 111 || s1 != "alpha-secret" {
		t.Fatalf("row 1 = (%d, %q, %v), want (111, alpha-secret, true)", v1, s1, ok1)
	}
	v2, s2, ok2 := f.readRow(t, reopened, 200, 2, 2)
	if !ok2 || v2 != 222 || s2 != largeText {
		t.Fatalf("row 2 mismatch: ok=%v v2=%d len=%d", ok2, v2, len(s2))
	}
	v3, s3, ok3 := f.readRow(t, reopened, 200, 2, 3)
	if !ok3 || v3 != 333 || s3 != "beta-secret" {
		t.Fatalf("row 3 = (%d, %q, %v), want (333, beta-secret, true)", v3, s3, ok3)
	}
}

func TestF5WALArchivingAndRecoverToTimestamp(t *testing.T) {
	f := newF5Fixture(t)
	archiveDir := filepath.Join(t.TempDir(), "wal-archive")
	archiveKey := randomAES256Key(t)

	var mu sync.Mutex
	var hookEntries []WALArchiveEntry

	cfg := Config{
		Directory:     f.dir,
		Scale:         f.scale,
		KeyEncoder:    f.encoder,
		FenceVerifier: f.authority,
		WALArchiveDir: archiveDir,
		WALArchiveKey: archiveKey,
		WALArchiveHook: func(ctx context.Context, entry WALArchiveEntry) error {
			mu.Lock()
			hookEntries = append(hookEntries, entry)
			mu.Unlock()
			return nil
		},
	}
	db, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 1, 100, []struct {
		id  int64
		val int64
		txt string
	}{{id: 1, val: 10, txt: "A"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 2, 200, []struct {
		id  int64
		val int64
		txt string
	}{{id: 2, val: 20, txt: "B"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 3, 300, []struct {
		id  int64
		val int64
		txt string
	}{{id: 3, val: 30, txt: "C"}})); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	if len(hookEntries) != 3 {
		t.Fatalf("hookEntries = %d, want 3", len(hookEntries))
	}
	for i, e := range hookEntries {
		if e.LogPosition != uint64(i+1) || e.CommitTS != model.Timestamp((i+1)*100) || !e.Encrypted {
			t.Fatalf("hook entry %d = %+v", i, e)
		}
	}
	mu.Unlock()

	// Verify archived WAL files are encrypted with archiveKey and inspectable.
	for pos := uint64(1); pos <= 3; pos++ {
		walPath := filepath.Join(archiveDir, commitName(pos))
		metaPath := filepath.Join(archiveDir, walMetaName(pos))
		raw, err := os.ReadFile(walPath)
		if err != nil {
			t.Fatalf("read archived WAL %d: %v", pos, err)
		}
		if !bytes.HasPrefix(raw, []byte(encryptionMagic)) {
			t.Fatalf("archived WAL %d not encrypted", pos)
		}
		if _, err := os.Stat(metaPath); err != nil {
			t.Fatalf("missing archived meta %d: %v", pos, err)
		}
		gotPos, gotTS, err := InspectCommitHeader(t.Context(), raw, archiveKey)
		if err != nil {
			t.Fatalf("InspectCommitHeader %d: %v", pos, err)
		}
		if gotPos != pos || gotTS != model.Timestamp(pos*100) {
			t.Fatalf("header %d = (%d, %d)", pos, gotPos, gotTS)
		}
	}

	// RecoverToTimestamp(200) must recover commits 1 and 2 and exclude commit 3!
	recovered, err := RecoverToTimestamp(t.Context(), Config{
		Directory: f.dir, Scale: f.scale, KeyEncoder: f.encoder, FenceVerifier: f.authority,
	}, 200)
	if err != nil {
		t.Fatalf("RecoverToTimestamp: %v", err)
	}
	defer func() {
		if err := recovered.Close(); err != nil {
			t.Error(err)
		}
	}()
	st, err := recovered.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.LogPosition != 2 || st.CommitTS != 200 {
		t.Fatalf("recovered state = %+v, want pos=2 commitTS=200", st)
	}
	if v, s, ok := f.readRow(t, recovered, 200, 2, 1); !ok || v != 10 || s != "A" {
		t.Fatalf("row A missing: %v %d %q", ok, v, s)
	}
	if v, s, ok := f.readRow(t, recovered, 200, 2, 2); !ok || v != 20 || s != "B" {
		t.Fatalf("row B missing: %v %d %q", ok, v, s)
	}
	if _, _, ok := f.readRow(t, recovered, 200, 2, 3); ok {
		t.Fatal("row C from T3 must NOT exist after RecoverToTimestamp(T2)")
	}
}
