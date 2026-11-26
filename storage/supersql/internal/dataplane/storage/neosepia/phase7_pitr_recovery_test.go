package neosepia

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

func phase7WriteUnflushedWALCommits(
	t *testing.T,
	commitsDir string,
	fixture backendFixture,
	target storage.ObjectRef,
	cols []storage.ColumnLayout,
	count int,
	payloadBytes int,
) (txIDs []model.TxnID, expected map[int64]string, totalPayload uint64) {
	t.Helper()
	commitsRoot, err := os.OpenRoot(commitsDir)
	if err != nil {
		t.Fatalf("OpenRoot commits: %v", err)
	}
	defer func() {
		if err := commitsRoot.Close(); err != nil {
			t.Errorf("close commits root: %v", err)
		}
	}()

	txIDs = make([]model.TxnID, 0, count)
	expected = make(map[int64]string, count)
	pad := strings.Repeat("x", payloadBytes)
	for i := 1; i <= count; i++ {
		val := fmt.Sprintf("commit-%04d-%s", i, pad)
		ts := model.Timestamp(i * 10)
		batch := makePhase4StrBatch(t, fixture, uint64(i), ts, target, cols, int64(i), model.StringVal(val))
		identity, err := batch.Identity(t.Context())
		if err != nil {
			t.Fatalf("batch %d identity: %v", i, err)
		}
		if _, err := writeCommit(t.Context(), commitsRoot, uint64(i), batch, identity, fixture.config.Limits.WALBytes, nil); err != nil {
			t.Fatalf("writeCommit %d: %v", i, err)
		}
		txIDs = append(txIDs, batch.TxID)
		expected[int64(i)] = val
		totalPayload += uint64(len(val))
	}
	return txIDs, expected, totalPayload
}

func phase7CountRunArtifacts(t *testing.T, runsDir string) (manifestCount, sstCount int) {
	t.Helper()
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatalf("ReadDir runs: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".runs") && !strings.HasPrefix(name, "base-") {
			manifestCount++
		}
		if strings.HasSuffix(name, ".sst") {
			sstCount++
		}
	}
	return manifestCount, sstCount
}

// TestPhase7PITRRecoveryPeriodicSSTFlush verifies that replaying many unflushed
// WAL commits (20+ flush cycles worth) during Open() and RecoverToTimestamp()
// periodically flushes memtable batches into coalesced .runs / .sst files,
// keeps post-open memtable.totalBytes strictly bounded, and reads every row
// accurately.
func TestPhase7PITRRecoveryPeriodicSSTFlush(t *testing.T) {
	target := encTestTarget()
	strCol := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}},
	}

	limits := DefaultLimits()
	limits.ResidentBytes = 128 << 10 // flush threshold = 32 KiB
	limits.WriteBytes = 128 << 10
	limits.OverlayBytes = 64 << 10
	limits.PublicationBytes = 128 << 10
	flushThreshold := memtableMaxBytes(limits)
	if flushThreshold != 32<<10 {
		t.Fatalf("memtableMaxBytes = %d, want %d", flushThreshold, 32<<10)
	}

	// Initialize empty database directory with valid database.state, then close.
	fixture := openBackendFixtureWithLimits(t, limits)
	if err := fixture.backend.Close(); err != nil {
		t.Fatalf("initial Close: %v", err)
	}

	const totalCommits = 65
	const payloadBytes = 12 << 10 // ~12 KiB/commit => ~780 KiB total (> 24 flush cycles at 32 KiB)
	_, expected, totalPayload := phase7WriteUnflushedWALCommits(
		t,
		filepath.Join(fixture.config.Directory, "commits"),
		fixture,
		target,
		strCol,
		totalCommits,
		payloadBytes,
	)

	// Confirm zero .runs / .sst files exist before recovery.
	runsDir := filepath.Join(fixture.config.Directory, "runs")
	if mCount, sCount := phase7CountRunArtifacts(t, runsDir); mCount != 0 || sCount != 0 {
		t.Fatalf("pre-recovery runs artifacts = (%d manifests, %d ssts), want (0, 0)", mCount, sCount)
	}

	// Reopen: recovery must periodically flush to SST runs so heap stays bounded.
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatalf("Open with %d unflushed WAL commits: %v", totalCommits, err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened: %v", err)
		}
	}()

	manifestCount, sstCount := phase7CountRunArtifacts(t, runsDir)
	if manifestCount < 15 || sstCount < 15 {
		t.Fatalf("expected >= 15 periodic recovery flushes, got %d .runs manifests and %d .sst files", manifestCount, sstCount)
	}

	reopened.mu.Lock()
	postOpenMemBytes := reopened.memtable.totalBytes
	reopened.mu.Unlock()
	if postOpenMemBytes >= flushThreshold {
		t.Fatalf("post-open memtable.totalBytes = %d, want < flushThreshold %d (total replayed payload %d)", postOpenMemBytes, flushThreshold, totalPayload)
	}
	if postOpenMemBytes*10 >= totalPayload {
		t.Fatalf("post-open memtable.totalBytes = %d is not well below total replayed payload %d", postOpenMemBytes, totalPayload)
	}

	st, err := reopened.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.LogPosition != totalCommits || st.CommitTS != model.Timestamp(totalCommits*10) {
		t.Fatalf("recovered state = %+v, want pos=%d commitTS=%d", st, totalCommits, totalCommits*10)
	}

	fence, err := fixture.authority.Issue(t.Context(), st.ClosedTS, st.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatalf("issue fence: %v", err)
	}
	reader, err := reopened.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: st.ClosedTS, Fence: fence})
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close snapshot: %v", err)
		}
	}()

	for k := int64(1); k <= totalCommits; k++ {
		row, err := reader.Get(t.Context(), storage.ReadRequest{
			Target:    target,
			Key:       backendKey(k),
			ColumnIDs: []model.ColumnID{1},
			Layout:    strCol,
		})
		if err != nil {
			t.Fatalf("Get key %d: %v", k, err)
		}
		if len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].StrVal != expected[k] {
			t.Fatalf("key %d value mismatch: got len=%d", k, len(row.Values[0].StrVal))
		}
	}

	// Verify RecoverToTimestamp PITR over an existing directory with pre-existing
	// .runs/.sst files also periodically flushes without colliding with old runs
	// and cleanly truncates commits beyond targetTS.
	if err := reader.Close(); err != nil {
		t.Fatalf("close snapshot before PITR: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened before PITR: %v", err)
	}

	pitrFresh := openBackendFixtureWithLimits(t, limits)
	if err := pitrFresh.backend.Close(); err != nil {
		t.Fatalf("close pitrFresh init: %v", err)
	}
	phase7WriteUnflushedWALCommits(
		t,
		filepath.Join(pitrFresh.config.Directory, "commits"),
		pitrFresh,
		target,
		strCol,
		totalCommits,
		payloadBytes,
	)
	// First normal Open+Close writes coalesced .runs/.sst files for all 65 commits (without advancing ClosedTS).
	prePITR, err := Open(t.Context(), pitrFresh.config)
	if err != nil {
		t.Fatalf("pre-PITR Open: %v", err)
	}
	if err := prePITR.Close(); err != nil {
		t.Fatalf("pre-PITR Close: %v", err)
	}

	const pitrTargetCommit = 50
	pitrTS := model.Timestamp(pitrTargetCommit * 10)
	pitrDB, err := RecoverToTimestamp(t.Context(), pitrFresh.config, pitrTS)
	if err != nil {
		t.Fatalf("RecoverToTimestamp(%d): %v", pitrTS, err)
	}
	defer func() {
		if err := pitrDB.Close(); err != nil {
			t.Errorf("close pitrDB: %v", err)
		}
	}()

	pitrDB.mu.Lock()
	pitrMemBytes := pitrDB.memtable.totalBytes
	pitrDB.mu.Unlock()
	if pitrMemBytes >= flushThreshold {
		t.Fatalf("post-PITR memtable.totalBytes = %d, want < flushThreshold %d", pitrMemBytes, flushThreshold)
	}
	pitrManifests, pitrSSTs := phase7CountRunArtifacts(t, filepath.Join(pitrFresh.config.Directory, "runs"))
	if pitrManifests < 10 || pitrSSTs < 10 {
		t.Fatalf("expected >= 10 periodic PITR flushes, got %d manifests and %d ssts", pitrManifests, pitrSSTs)
	}

	pitrState, err := pitrDB.State(t.Context())
	if err != nil {
		t.Fatalf("pitrDB.State: %v", err)
	}
	if pitrState.LogPosition != pitrTargetCommit || pitrState.CommitTS != pitrTS {
		t.Fatalf("pitrDB state = %+v, want pos=%d commitTS=%d", pitrState, pitrTargetCommit, pitrTS)
	}
	pitrFence, err := pitrFresh.authority.Issue(t.Context(), pitrState.ClosedTS, pitrState.LogPosition, pitrFresh.epoch)
	if err != nil {
		t.Fatalf("issue pitr fence: %v", err)
	}
	pitrReader, err := pitrDB.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: pitrState.ClosedTS, Fence: pitrFence})
	if err != nil {
		t.Fatalf("pitrDB OpenSnapshot: %v", err)
	}
	defer func() {
		if err := pitrReader.Close(); err != nil {
			t.Errorf("close pitrReader: %v", err)
		}
	}()
	for k := int64(1); k <= pitrTargetCommit; k++ {
		row, err := pitrReader.Get(t.Context(), storage.ReadRequest{
			Target:    target,
			Key:       backendKey(k),
			ColumnIDs: []model.ColumnID{1},
			Layout:    strCol,
		})
		if err != nil {
			t.Fatalf("PITR Get key %d: %v", k, err)
		}
		if len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].StrVal != expected[k] {
			t.Fatalf("PITR key %d value mismatch", k)
		}
	}
	if _, err := pitrReader.Get(t.Context(), storage.ReadRequest{
		Target:    target,
		Key:       backendKey(pitrTargetCommit + 1),
		ColumnIDs: []model.ColumnID{1},
		Layout:    strCol,
	}); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("PITR key %d after targetTS: err = %v, want ErrRowNotFound", pitrTargetCommit+1, err)
	}
}

// TestPhase7RecoveryIdentityBytesPruning verifies that replaying more WAL commits
// than fit in IdentityBytes prunes the oldest transaction records instead of
// failing with ordered.ErrBudget, keeps all rows readable, keeps recent commits
// resolvable via ResolveCommit, and preserves ErrBudget when IdentityBytes cannot
// fit even a single transaction record.
func TestPhase7RecoveryIdentityBytesPruning(t *testing.T) {
	target := encTestTarget()
	strCol := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}},
	}

	limits := DefaultLimits()
	// ~18 ordered.Tree nodes total (112 bytes/node); holds ~10-12 commits before Set hits ErrBudget.
	limits.IdentityBytes = 2048

	fixture := openBackendFixtureWithLimits(t, limits)
	if err := fixture.backend.Close(); err != nil {
		t.Fatalf("initial Close: %v", err)
	}

	const totalCommits = 40
	txIDs, expected, _ := phase7WriteUnflushedWALCommits(
		t,
		filepath.Join(fixture.config.Directory, "commits"),
		fixture,
		target,
		strCol,
		totalCommits,
		64,
	)

	// Edge case: if IdentityBytes cannot hold even 1 transaction record, recovery must still return ErrBudget.
	tinyDir := t.TempDir()
	copyDirectoryTree(t, fixture.config.Directory, tinyDir)
	tinyCfg := fixture.config
	tinyCfg.Directory = tinyDir
	tinyCfg.Limits.IdentityBytes = 64 // < 112 bytes per ordered.Tree node
	badDB, err := Open(t.Context(), tinyCfg)
	if badDB != nil {
		_ = badDB.Close()
		t.Fatalf("Open with tiny IdentityBytes=64 unexpectedly succeeded")
	}
	if !errors.Is(err, ordered.ErrBudget) {
		t.Fatalf("Open with tiny IdentityBytes=64 err = %v, want ordered.ErrBudget", err)
	}

	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatalf("Open with %d WAL commits and IdentityBytes=%d failed: %v", totalCommits, limits.IdentityBytes, err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened: %v", err)
		}
	}()

	reopened.mu.Lock()
	prunedPos := reopened.prunedPosition
	prunedTS := reopened.prunedCommitTS
	retainedLen := reopened.transactions.Len()
	retainedBytes := reopened.transactions.NodeBytes()
	reopened.mu.Unlock()

	if prunedPos == 0 || prunedTS == 0 {
		t.Fatalf("expected identity pruning during recovery, got prunedPos=%d prunedTS=%d", prunedPos, prunedTS)
	}
	if prunedPos+retainedLen != totalCommits {
		t.Fatalf("prunedPos(%d) + retainedLen(%d) != totalCommits(%d)", prunedPos, retainedLen, totalCommits)
	}
	if retainedBytes > limits.IdentityBytes {
		t.Fatalf("transactions.NodeBytes() = %d exceeds IdentityBytes = %d", retainedBytes, limits.IdentityBytes)
	}
	if retainedLen == 0 {
		t.Fatalf("expected non-empty recent transaction window, got 0")
	}

	// All 40 commits' rows must be readable.
	st, err := reopened.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	fence, err := fixture.authority.Issue(t.Context(), st.ClosedTS, st.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatalf("issue fence: %v", err)
	}
	reader, err := reopened.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: st.ClosedTS, Fence: fence})
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	for k := int64(1); k <= totalCommits; k++ {
		row, err := reader.Get(t.Context(), storage.ReadRequest{
			Target:    target,
			Key:       backendKey(k),
			ColumnIDs: []model.ColumnID{1},
			Layout:    strCol,
		})
		if err != nil {
			t.Fatalf("Get key %d: %v", k, err)
		}
		if len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].StrVal != expected[k] {
			t.Fatalf("key %d value mismatch: got %q want %q", k, row.Values[0].StrVal, expected[k])
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}

	// Recent commits (after prunedPos) must remain resolvable via ResolveCommit.
	for pos := prunedPos + 1; pos <= totalCommits; pos++ {
		res, err := reopened.ResolveCommit(t.Context(), txIDs[pos-1])
		if err != nil {
			t.Fatalf("ResolveCommit(pos=%d): %v", pos, err)
		}
		if res.State != storage.CommitCommitted || res.Result.LogPosition != pos {
			t.Fatalf("ResolveCommit(pos=%d) = %+v, want CommitCommitted at pos %d", pos, res, pos)
		}
	}

	// Maintenance Collect() must succeed cleanly over the pruned recovered state.
	if err := reopened.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatalf("Collect after pruned recovery: %v", err)
	}
}

// TestPhase7MemtableStageBatchCumulativeNodeBudget verifies stageBatchWithBudget
// charges cumulative ordered.Tree NodeBytes against nodeBudget so a memtable
// cannot grow past its node budget before flush, while failing atomically.
func TestPhase7MemtableStageBatchCumulativeNodeBudget(t *testing.T) {
	target := encTestTarget()
	state := databaseStateFixture()
	cols := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}},
	}
	makeBatch := func(seq uint64, rows int) storage.MutationBatch {
		b := storage.MutationBatch{
			CommitTS:        model.Timestamp(seq * 10),
			Epoch:           state.Closed.Epoch,
			EncodingVersion: storage.MutationEncodingVersion,
			Layouts:         []storage.ObjectLayout{{Target: target, Columns: cols}},
			Mutations:       make([]storage.Mutation, rows),
		}
		binary.LittleEndian.PutUint64(b.TxID[:], seq)
		for i := range rows {
			k := int64(seq)*1000 + int64(i)
			m := storage.NewUpsertMutation(target, backendKey(k), []model.ColumnID{1}, []model.Value{model.Int64Val(k)})
			m.Ordinal = uint64(i + 1)
			b.Mutations[i] = m
		}
		var err error
		b.ChecksumCRC, err = b.ComputeChecksum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	var mem memtable
	const tightNodeBudget = 2048 // holds ~20 memtable tree nodes total
	var stagedCommits int
	var hitErr error
	for seq := uint64(1); seq <= 20; seq++ {
		b := makeBatch(seq, 5)
		id, err := b.Identity(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		dec := runDecision{Position: seq, TxID: b.TxID, CommitTS: b.CommitTS, Identity: id, MutationCount: b.Count()}
		if err := mem.stageBatchWithBudget(t.Context(), dec, b.Epoch, b, 1<<20, tightNodeBudget); err != nil {
			hitErr = err
			break
		}
		stagedCommits++
	}
	if !errors.Is(hitErr, ordered.ErrBudget) {
		t.Fatalf("stageBatchWithBudget cumulative budget err = %v, want ordered.ErrBudget", hitErr)
	}
	if stagedCommits == 0 {
		t.Fatalf("expected at least 1 batch to fit before hitting nodeBudget")
	}
	if len(mem.commits) != stagedCommits {
		t.Fatalf("failed batch mutated memtable commits: got %d want %d", len(mem.commits), stagedCommits)
	}
	for _, ts := range mem.targets {
		if ts.entries.NodeBytes() > tightNodeBudget {
			t.Fatalf("memtable target NodeBytes = %d exceeds tightNodeBudget %d", ts.entries.NodeBytes(), tightNodeBudget)
		}
	}
}

// TestPhase7RecoveryReadOnlySkipsSSTFlush verifies that recoverPublishedRuns does
// not write SST or .runs files to disk when ReadOnly is true.
func TestPhase7RecoveryReadOnlySkipsSSTFlush(t *testing.T) {
	target := encTestTarget()
	strCol := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}},
	}
	limits := DefaultLimits()
	limits.ResidentBytes = 128 << 10
	limits.WriteBytes = 128 << 10
	limits.OverlayBytes = 64 << 10
	limits.PublicationBytes = 128 << 10

	state := databaseStateFixture()
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := Config{
		Directory:     dir,
		Scale:         state.Scales[0].Scale,
		KeyEncoder:    backendIntEncoder{},
		FenceVerifier: authority,
		Limits:        limits,
	}
	db, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	fixture := backendFixture{config: cfg, authority: authority, epoch: state.Closed.Epoch}
	phase7WriteUnflushedWALCommits(t, filepath.Join(dir, "commits"), fixture, target, strCol, 10, 12<<10)

	directory, err := openDatabaseDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	}()
	dbState, err := readDatabaseState(t.Context(), directory.root, limits.MetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverPublishedRuns(t.Context(), directory, dbState, runRecoveryLimits{
		Log:           limits.recovery(),
		Preparation:   limits.preparation(),
		Resident:      limits.resident(),
		StagingBytes:  limits.ResidentBytes,
		IdentityBytes: limits.IdentityBytes,
		ReadOnly:      true,
	}, nil)
	if err != nil {
		t.Fatalf("read-only recoverPublishedRuns: %v", err)
	}
	defer func() {
		if err := recovered.Set.Close(); err != nil {
			t.Error(err)
		}
	}()

	manifests, ssts := phase7CountRunArtifacts(t, filepath.Join(dir, "runs"))
	if manifests != 0 || ssts != 0 {
		t.Fatalf("read-only recovery wrote run artifacts: %d manifests, %d ssts, want 0/0", manifests, ssts)
	}
	if recovered.Memtable.totalBytes < memtableMaxBytes(limits) {
		t.Fatalf("expected read-only recovery to retain all unflushed commits in memtable, got %d bytes", recovered.Memtable.totalBytes)
	}
	if recovered.Position.LogPosition != 10 {
		t.Fatalf("read-only recovered LogPosition = %d, want 10", recovered.Position.LogPosition)
	}
}
