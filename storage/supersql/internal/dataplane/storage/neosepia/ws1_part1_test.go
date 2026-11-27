package neosepia

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func openBackendFixtureWithLimits(t *testing.T, limits Limits) backendFixture {
	t.Helper()
	state := databaseStateFixture()
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		Directory:     t.TempDir(),
		Scale:         state.Scales[0].Scale,
		KeyEncoder:    backendIntEncoder{},
		FenceVerifier: authority,
		Limits:        limits,
	}
	backend, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	return backendFixture{backend: backend, config: config, authority: authority, epoch: state.Closed.Epoch}
}

func makeIntRowsBatch(t *testing.T, fixture backendFixture, txSeq uint64, ts model.Timestamp, target storage.ObjectRef, keys []int64, values []int64) storage.MutationBatch {
	t.Helper()
	batch := storage.MutationBatch{
		CommitTS:        ts,
		Epoch:           fixture.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         []storage.ObjectLayout{{Target: target, Columns: scanFixtureSpec().Layout}},
		Mutations:       make([]storage.Mutation, len(keys)),
	}
	binary.LittleEndian.PutUint64(batch.TxID[:], txSeq)
	for i := range keys {
		batch.Mutations[i] = storage.Mutation{
			Op:        storage.MutationUpsert,
			Target:    target,
			Key:       backendKey(keys[i]),
			Ordinal:   uint64(i + 1),
			ColumnIDs: []model.ColumnID{1},
			Values:    []model.Value{model.Int64Val(values[i])},
		}
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func readIntKey(t *testing.T, reader storage.SnapshotReader, target storage.ObjectRef, key int64) int64 {
	t.Helper()
	row, err := reader.Get(t.Context(), storage.ReadRequest{
		Target:    target,
		Key:       backendKey(key),
		ColumnIDs: []model.ColumnID{1},
		Layout:    scanFixtureSpec().Layout,
	})
	if err != nil {
		t.Fatalf("get key %d: %v", key, err)
	}
	if len(row.Values) != 1 || row.Values[0].IsNull {
		t.Fatalf("key %d unexpected row %+v", key, row.Values)
	}
	return row.Values[0].IntVal
}

func copyDirectoryTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == ".lock" {
			continue
		}
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			copyDirectoryTree(t, srcPath, dstPath)
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dstPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNSBASE04PrunesTransactionsBeyondIdentityWall verifies that NSBASE04 prunes
// covered transaction identities so lifetime commits far exceed IdentityBytes/112,
// while preserving O(1) fence validation, reopen, retry idempotency, and checkpoints.
func TestNSBASE04PrunesTransactionsBeyondIdentityWall(t *testing.T) {
	limits := DefaultLimits()
	limits.IdentityBytes = 4096 // fits ~36 ordered.Tree nodes total
	fixture := openBackendFixtureWithLimits(t, limits)
	target := encTestTarget()

	const totalCommits = 120 // > 3x IdentityBytes / 112
	var lastBatch storage.MutationBatch
	for i := uint64(1); i <= totalCommits; i++ {
		ts := model.Timestamp(i * 10)
		batch := makeIntRowsBatch(t, fixture, i, ts, target, []int64{int64(i)}, []int64{int64(i * 100)})
		res, err := fixture.backend.ApplyMutations(t.Context(), batch)
		if err != nil {
			t.Fatalf("commit %d failed: %v", i, err)
		}
		if res.LogPosition != i {
			t.Fatalf("commit %d position = %d, want %d", i, res.LogPosition, i)
		}
		lastBatch = batch
		if i%10 == 0 {
			// Alternate Boundary=0 (window cap pruning) and advancing floor pruning.
			var boundary model.Timestamp
			if i >= 60 {
				boundary = model.Timestamp((i - 10) * 10)
			}
			if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: boundary}); err != nil {
				t.Fatalf("collect at commit %d failed: %v", i, err)
			}
		}
	}

	if fixture.backend.prunedPosition == 0 || fixture.backend.prunedCommitTS == 0 {
		t.Fatalf("expected pruned base metadata, got prunedPosition=%d prunedCommitTS=%d", fixture.backend.prunedPosition, fixture.backend.prunedCommitTS)
	}
	maxRetained := maxRetainedBaseTransactions(limits.IdentityBytes)
	if got := fixture.backend.transactions.Len(); got > maxRetained {
		t.Fatalf("retained transactions = %d, exceeds bounded cap %d", got, maxRetained)
	}

	// Verify O(1) fence validation rejects a fence inside the pruned prefix whose
	// ClosedTS omits commits up to prunedCommitTS.
	badFence, err := fixture.authority.Issue(t.Context(), fixture.backend.prunedCommitTS, fixture.backend.prunedPosition-1, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: fixture.backend.state.HistoryFloor, Fence: badFence}); !errors.Is(err, storage.ErrSnapshotNotClosed) {
		if r != nil {
			_ = r.Close()
		}
		t.Fatalf("expected ErrSnapshotNotClosed for stale fence omitting pruned commit, got %v", err)
	}

	// Take and verify a checkpoint over the pruned NSBASE04 base.
	st, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), st.ClosedTS, st.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{
		Snapshot: storage.SnapshotSpec{Timestamp: st.ClosedTS, Fence: fence},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	cpDir := t.TempDir()
	copyCheckpointFiles(t, checkpoint, cpDir)
	cpRoot, err := os.OpenRoot(cpDir)
	if err != nil {
		t.Fatal(err)
	}
	verifier := CheckpointVerifier{FenceVerifier: fixture.authority, Limits: limits}
	verifyErr := verifier.VerifyCheckpoint(t.Context(), cpRoot, checkpointManifest(t, checkpoint))
	if err := errors.Join(verifyErr, cpRoot.Close()); err != nil {
		t.Fatalf("VerifyCheckpoint on pruned NSBASE04 base failed: %v", err)
	}

	// Reopen and verify all 120 keys + frontier retry idempotency.
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatalf("reopen pruned NSBASE04 backend failed: %v", err)
	}
	fixture.backend = reopened
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	reader := fixture.snapshot(t, model.Timestamp(totalCommits*10))
	for i := int64(1); i <= totalCommits; i++ {
		if got := readIntKey(t, reader, target, i); got != i*100 {
			t.Fatalf("reopened key %d = %d, want %d", i, got, i*100)
		}
	}
	if retry, err := reopened.ApplyMutations(t.Context(), lastBatch); err != nil || retry.LogPosition != totalCommits {
		t.Fatalf("frontier retry after pruned reopen: %+v, %v", retry, err)
	}
}

// TestNSBASE03LegacyDecodeStillWorks verifies backward-compatible decoding of NSBASE03 manifests.
func TestNSBASE03LegacyDecodeStillWorks(t *testing.T) {
	fixture := openBackendFixture(t)
	for _, batch := range []storage.MutationBatch{
		fixture.batch(t, 1, 10, 100, 100),
		fixture.batch(t, 2, 20, 90, 110),
	} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{}); err != nil {
		t.Fatal(err)
	}
	gen := fixture.backend.state.BaseGeneration
	basePath := filepath.Join(fixture.config.Directory, "runs", baseManifestName(gen))
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[:8]) != "NSBASE04" {
		t.Fatalf("expected NSBASE04 magic, got %q", string(raw[:8]))
	}
	// Downgrade header magic to NSBASE03 (PrunedPosition/PrunedCommitTS are 0).
	copy(raw[:8], "NSBASE03")
	digest := sha256.Sum256(raw[:len(raw)-sha256.Size])
	copy(raw[len(raw)-sha256.Size:], digest[:])
	if err := os.WriteFile(basePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatalf("open legacy NSBASE03 failed: %v", err)
	}
	fixture.backend = reopened
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	reader := fixture.snapshot(t, 20)
	if got := backendBalance(t, reader, 0); got != 90 {
		t.Fatalf("legacy NSBASE03 value = %d, want 90", got)
	}
}

// TestStreamingVerifyBaseExceedsVersionTreeWall verifies VerifyCheckpoint streams
// base SSTs in bounded memory and handles far more row versions than IdentityBytes/48.
func TestStreamingVerifyBaseExceedsVersionTreeWall(t *testing.T) {
	limits := DefaultLimits()
	limits.IdentityBytes = 8192 // old versions ordered.Tree ceiling: 8192 / 48 = 170 row versions
	fixture := openBackendFixtureWithLimits(t, limits)
	target := encTestTarget()

	const commits = 6
	const rowsPerCommit = 100 // 600 row versions (> 3.5x old 170-version ceiling)
	for c := uint64(1); c <= commits; c++ {
		keys := make([]int64, rowsPerCommit)
		vals := make([]int64, rowsPerCommit)
		for i := range rowsPerCommit {
			k := int64((c-1)*rowsPerCommit) + int64(i) + 1
			keys[i] = k
			vals[i] = k * 7
		}
		batch := makeIntRowsBatch(t, fixture, c, model.Timestamp(c*10), target, keys, vals)
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatalf("commit %d: %v", c, err)
		}
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}
	st, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), st.ClosedTS, st.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{
		Snapshot: storage.SnapshotSpec{Timestamp: st.ClosedTS, Fence: fence},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	dest := t.TempDir()
	copyCheckpointFiles(t, checkpoint, dest)
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	verifier := CheckpointVerifier{FenceVerifier: fixture.authority, Limits: limits}
	verifyErr := verifier.VerifyCheckpoint(t.Context(), root, checkpointManifest(t, checkpoint))
	if err := errors.Join(verifyErr, root.Close()); err != nil {
		t.Fatalf("streaming VerifyCheckpoint failed with %d row versions and IdentityBytes=%d: %v", commits*rowsPerCommit, limits.IdentityBytes, err)
	}

	restoreCfg := fixture.config
	restoreCfg.Directory = dest
	restored, err := Open(t.Context(), restoreCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = restored
	reader := fixture.snapshot(t, st.ClosedTS)
	for k := int64(1); k <= commits*rowsPerCommit; k++ {
		if got := readIntKey(t, reader, target, k); got != k*7 {
			t.Fatalf("restored key %d = %d, want %d", k, got, k*7)
		}
	}
}

// TestInactiveTierMultiWindowCompactionProducesOneSSTPerWindowAndConverges verifies
// that compacting keys spanning 4 hourly windows produces 1 inactive SST per window
// (not O(Keys x Windows)) and a second maintenance pass performs zero rewrites.
func TestInactiveTierMultiWindowCompactionProducesOneSSTPerWindowAndConverges(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()

	const numKeys = 20
	const numWindows = 4 // windows 1, 2, 3 become inactive; window 4 stays active
	keys := make([]int64, numKeys)
	for i := range numKeys {
		keys[i] = int64(i + 1)
	}
	for w := uint64(1); w <= numWindows; w++ {
		vals := make([]int64, numKeys)
		for i := range numKeys {
			vals[i] = int64(w*1000) + int64(i+1)
		}
		ts := model.Timestamp(w*inactiveWindowWidth + 100)
		batch := makeIntRowsBatch(t, fixture, w, ts, target, keys, vals)
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatalf("window %d commit: %v", w, err)
		}
	}

	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}

	pin, err := fixture.backend.runs.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var activeCount, inactiveCount int
	var firstPassNames []string
	if err := pin.visit(t.Context(), target, func(file *sstFile) error {
		firstPassNames = append(firstPassNames, file.descriptor.Name)
		if file.tier == activeTier {
			activeCount++
		} else {
			inactiveCount++
		}
		return nil
	}); err != nil {
		_ = pin.Close()
		t.Fatal(err)
	}
	work, err := pin.selectTargetCompaction(t.Context(), target, make(map[string]struct{}), fixture.backend.limits)
	if err := errors.Join(err, pin.Close()); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active SST count = %d, want 1", activeCount)
	}
	if inactiveCount != numWindows-1 {
		t.Fatalf("inactive SST count = %d, want %d (1 per historical window)", inactiveCount, numWindows-1)
	}
	if len(work.files) != 0 {
		t.Fatalf("second pass selected %d files to compact; expected no-op", len(work.files))
	}

	// Verify second Collect does not rewrite any SST file.
	slices.Sort(firstPassNames)
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}
	secondPassNames := runSSTNames(t, fixture.config.Directory)
	if !slices.Equal(firstPassNames, secondPassNames) {
		t.Fatalf("second maintenance pass rewrote SSTs: before=%v after=%v", firstPassNames, secondPassNames)
	}

	// Verify all 4 historical snapshots return exact values for all 20 keys.
	for w := uint64(1); w <= numWindows; w++ {
		ts := model.Timestamp(w*inactiveWindowWidth + 100)
		reader := fixture.snapshot(t, ts)
		for i := range numKeys {
			want := int64(w*1000) + int64(i+1)
			if got := readIntKey(t, reader, target, int64(i+1)); got != want {
				t.Fatalf("window %d key %d = %d, want %d", w, i+1, got, want)
			}
		}
	}
}

// TestPAXBlockCoalescingMergesSingleRowCommits verifies that compacting 50
// single-row commits coalesces them into 1 PAX block instead of 50 1-row blocks.
func TestPAXBlockCoalescingMergesSingleRowCommits(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()

	const commits = 50
	for i := uint64(1); i <= commits; i++ {
		batch := makeIntRowsBatch(t, fixture, i, model.Timestamp(i*10), target, []int64{int64(i)}, []int64{int64(i * 11)})
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}

	pin, err := fixture.backend.runs.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var sstCount, totalBlocks int
	var totalRows uint64
	if err := pin.visit(t.Context(), target, func(file *sstFile) error {
		sstCount++
		totalBlocks += file.count
		totalRows += file.rows
		return nil
	}); err != nil {
		_ = pin.Close()
		t.Fatal(err)
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	if sstCount != 1 || totalBlocks > 2 || totalRows != commits {
		t.Fatalf("PAX coalescing result: ssts=%d blocks=%d rows=%d, want ssts=1 blocks<=2 rows=%d", sstCount, totalBlocks, totalRows, commits)
	}

	reader := fixture.snapshot(t, model.Timestamp(commits*10))
	for i := int64(1); i <= commits; i++ {
		if got := readIntKey(t, reader, target, i); got != i*11 {
			t.Fatalf("coalesced key %d = %d, want %d", i, got, i*11)
		}
	}
}

// TestActiveL1NonOverlappingMultiFileRunsNotRewritten verifies that non-overlapping
// properly-sized L1 runs split at RunBytes/compactionFileBytes do not trigger
// an endless rewrite loop on subsequent maintenance passes.
func TestActiveL1NonOverlappingMultiFileRunsNotRewritten(t *testing.T) {
	limits := DefaultLimits()
	// Small RunBytes and BlockBytes so a modest table splits into multiple L1 files.
	limits.RunBytes = 32 << 10
	limits.BlockBytes = 12 << 10
	limits.BlockRows = 4
	fixture := openBackendFixtureWithLimits(t, limits)
	target := encTestTarget()
	layout := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}},
		{ID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}},
	}

	payload := make([]byte, 2048)
	for i := range payload {
		payload[i] = byte((i*31 + 7) & 0xff)
	}
	const numRows = 30
	batch := storage.MutationBatch{
		CommitTS:        10,
		Epoch:           fixture.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         []storage.ObjectLayout{{Target: target, Columns: layout}},
		Mutations:       make([]storage.Mutation, numRows),
	}
	binary.LittleEndian.PutUint64(batch.TxID[:], 1)
	for i := range numRows {
		p := append([]byte(nil), payload...)
		p[0] = byte(i)
		batch.Mutations[i] = storage.NewUpsertMutation(target, backendKey(int64(i+1)), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(int64(i + 1)), model.BytesVal(p)})
		batch.Mutations[i].Ordinal = uint64(i + 1)
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}

	pin, err := fixture.backend.runs.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var l1Names []string
	if err := pin.visit(t.Context(), target, func(file *sstFile) error {
		if file.tier == activeTier && file.level == 1 {
			l1Names = append(l1Names, file.descriptor.Name)
		}
		return nil
	}); err != nil {
		_ = pin.Close()
		t.Fatal(err)
	}
	work, err := pin.selectTargetCompaction(t.Context(), target, make(map[string]struct{}), fixture.backend.limits)
	if err := errors.Join(err, pin.Close()); err != nil {
		t.Fatal(err)
	}
	if len(l1Names) < 2 {
		t.Fatalf("expected >= 2 non-overlapping L1 files, got %d", len(l1Names))
	}
	if len(work.files) != 0 {
		t.Fatalf("selectTargetCompaction re-selected %d non-overlapping multi-file L1 runs", len(work.files))
	}

	slices.Sort(l1Names)
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}
	afterNames := runSSTNames(t, fixture.config.Directory)
	if !slices.Equal(l1Names, afterNames) {
		t.Fatalf("second maintenance pass rewrote non-overlapping L1 files: before=%v after=%v", l1Names, afterNames)
	}
}

// TestLeaseHorizonClosedTSSurvivesCrashWithoutPerReadFsync verifies that reads
// advancing ClosedTS within the persisted lease horizon perform zero database.state
// writes/fsyncs, while a simulated crash recovers ClosedTS >= ClosedHorizon so
// post-crash commits cannot violate pre-crash read fences.
func TestLeaseHorizonClosedTSSurvivesCrashWithoutPerReadFsync(t *testing.T) {
	fixture := openBackendFixture(t)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 100, 100, 100)); err != nil {
		t.Fatal(err)
	}

	// First read establishes ClosedTS=100 and persists ClosedHorizon=100+5s.
	r1 := fixture.snapshot(t, 100)
	if got := backendBalance(t, r1, 0); got != 100 {
		t.Fatalf("r1 balance = %d, want 100", got)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	onDiskAfterFirst, err := readDatabaseState(t.Context(), fixture.backend.directory.root, fixture.backend.limits.MetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	wantHorizon := closedHorizonFor(100)
	if onDiskAfterFirst.Closed.ClosedTS != 100 || onDiskAfterFirst.ClosedHorizon != wantHorizon {
		t.Fatalf("on-disk state after first read: ClosedTS=%d ClosedHorizon=%d, want 100 / %d",
			onDiskAfterFirst.Closed.ClosedTS, onDiskAfterFirst.ClosedHorizon, wantHorizon)
	}

	// Subsequent reads advancing ClosedTS within the lease window must NOT rewrite database.state.
	var lastInMemoryTS model.Timestamp
	for step := int64(1); step <= 10; step++ {
		lastInMemoryTS = model.Timestamp(100 + step*int64(time.Millisecond))
		fence, err := fixture.authority.Issue(t.Context(), lastInMemoryTS, 1, fixture.epoch)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: lastInMemoryTS, Fence: fence})
		if err != nil {
			t.Fatalf("read step %d: %v", step, err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
	onDiskAfterBurst, err := readDatabaseState(t.Context(), fixture.backend.directory.root, fixture.backend.limits.MetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	if onDiskAfterBurst.Generation != onDiskAfterFirst.Generation {
		t.Fatalf("reads within lease horizon triggered database.state write: gen %d -> %d",
			onDiskAfterFirst.Generation, onDiskAfterBurst.Generation)
	}
	inMemState, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if inMemState.ClosedTS != lastInMemoryTS {
		t.Fatalf("in-memory ClosedTS = %d, want %d", inMemState.ClosedTS, lastInMemoryTS)
	}

	// Simulate an unclean crash by copying the live directory before Close() runs.
	crashedDir := t.TempDir()
	copyDirectoryTree(t, fixture.config.Directory, crashedDir)

	// Also verify on the live backend that advancing past the lease horizon triggers a new persist.
	beyondLease := wantHorizon + 1
	fenceBeyond, err := fixture.authority.Issue(t.Context(), beyondLease, 1, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	rBeyond, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: beyondLease, Fence: fenceBeyond})
	if err != nil {
		t.Fatal(err)
	}
	if err := rBeyond.Close(); err != nil {
		t.Fatal(err)
	}
	onDiskBeyond, err := readDatabaseState(t.Context(), fixture.backend.directory.root, fixture.backend.limits.MetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	if onDiskBeyond.Generation <= onDiskAfterFirst.Generation || onDiskBeyond.ClosedHorizon != closedHorizonFor(beyondLease) {
		t.Fatalf("read past lease horizon did not renew durable horizon: %+v", onDiskBeyond)
	}

	// Recover the simulated crashed directory and verify ClosedTS >= wantHorizon.
	crashCfg := fixture.config
	crashCfg.Directory = crashedDir
	recovered, err := Open(t.Context(), crashCfg)
	if err != nil {
		t.Fatalf("open crashed directory failed: %v", err)
	}
	t.Cleanup(func() {
		if err := recovered.Close(); err != nil {
			t.Error(err)
		}
	})
	recState, err := recovered.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if recState.ClosedTS != wantHorizon {
		t.Fatalf("crash-recovered ClosedTS = %d, want %d", recState.ClosedTS, wantHorizon)
	}
	// Commits at or below the pre-crash in-memory read fence (or lease horizon) must be rejected.
	if _, err := recovered.ApplyMutations(t.Context(), fixture.batch(t, 2, lastInMemoryTS, 90, 110)); !errors.Is(err, storage.ErrInvalidMutation) {
		t.Fatalf("post-crash commit at pre-crash read fence %d should fail ErrInvalidMutation, got %v", lastInMemoryTS, err)
	}
	if _, err := recovered.ApplyMutations(t.Context(), fixture.batch(t, 2, wantHorizon, 90, 110)); !errors.Is(err, storage.ErrInvalidMutation) {
		t.Fatalf("post-crash commit at lease horizon %d should fail ErrInvalidMutation, got %v", wantHorizon, err)
	}
	// A commit strictly above the recovered lease horizon must succeed.
	if res, err := recovered.ApplyMutations(t.Context(), fixture.batch(t, 2, wantHorizon+1, 90, 110)); err != nil || res.LogPosition != 2 {
		t.Fatalf("post-crash commit above lease horizon failed: %+v, %v", res, err)
	}
}
