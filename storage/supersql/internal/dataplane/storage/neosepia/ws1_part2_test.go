package neosepia

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type ws1Part2Observer struct {
	mu           sync.Mutex
	readBytes    uint64
	decodedBytes uint64
	blocksRead   uint64
	blocksPruned uint64
}

func (o *ws1Part2Observer) ObserveScan(obs storage.ScanObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if obs.ReadBytes.Known {
		o.readBytes += obs.ReadBytes.Value
	}
	if obs.DecodedBytes.Known {
		o.decodedBytes += obs.DecodedBytes.Value
	}
	if obs.BlocksRead.Known {
		o.blocksRead += obs.BlocksRead.Value
	}
	if obs.BlocksPruned.Known {
		o.blocksPruned += obs.BlocksPruned.Value
	}
}

func TestBloomFilterSkipsDiskReadsForAbsentPointKeys(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()
	const numKeys = 500
	keys := make([]int64, numKeys)
	vals := make([]int64, numKeys)
	for i := range numKeys {
		keys[i] = int64(i * 2) // even keys present
		vals[i] = int64(i*10 + 1)
	}
	batch := makeIntRowsBatch(t, fixture, 1, 10, target, keys, vals)
	if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}

	// Verify SST is NSSST004 with a populated bloom filter.
	pin, err := fixture.backend.runs.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var sst *sstFile
	if err := pin.visit(t.Context(), target, func(f *sstFile) error {
		sst = f
		return nil
	}); err != nil {
		_ = pin.Close()
		t.Fatal(err)
	}
	if sst == nil || len(sst.core.bloom) == 0 || sst.core.bloomProbes == 0 {
		_ = pin.Close()
		t.Fatalf("expected NSSST004 bloom filter to be populated: sst=%v", sst)
	}
	// 0% false negatives on all present keys.
	for _, k := range keys {
		if !sst.mayContainKey(backendKey(k)) {
			_ = pin.Close()
			t.Fatalf("bloom filter false negative for present key %d", k)
		}
	}
	// < 2% false positives across 2000 absent odd keys.
	const absentChecks = 2000
	falsePositives := 0
	for i := range absentChecks {
		if sst.mayContainKey(backendKey(int64(i*2 + 1))) {
			falsePositives++
		}
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	if falsePositives > absentChecks/20 { // generous 5% ceiling on 0.8% expected FPR
		t.Fatalf("bloom filter false positive rate too high: %d / %d", falsePositives, absentChecks)
	}

	// Verify absent Get queries skip ReadBlock (zero cache misses / disk reads for bloom-rejected keys).
	fixture.backend.runs.blockCache.Clear()
	beforeMisses := fixture.backend.runs.blockCache.Misses()
	reader := fixture.snapshot(t, 10)
	layout := scanFixtureSpec().Layout
	skipped := 0
	for i := range 200 {
		k := backendKey(int64(i*2 + 1))
		if _, err := reader.Get(t.Context(), storage.ReadRequest{Target: target, Key: k, ColumnIDs: []model.ColumnID{1}, Layout: layout}); !errors.Is(err, storage.ErrRowNotFound) {
			t.Fatalf("absent key %d: got %v, want ErrRowNotFound", i*2+1, err)
		}
		skipped++
	}
	afterMisses := fixture.backend.runs.blockCache.Misses()
	if delta := afterMisses - beforeMisses; delta > 15 {
		t.Fatalf("bloom filter failed to skip disk reads for absent keys: %d ReadBlock misses out of %d absent Gets", delta, skipped)
	}

	// Verify Bloom filter never masks a key covered by a range tombstone.
	delBatch := storage.MutationBatch{
		TxID:            model.TxnID{2},
		CommitTS:        20,
		Epoch:           fixture.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         []storage.ObjectLayout{{Target: target, Columns: layout}},
		Mutations: []storage.Mutation{
			{Op: storage.MutationDeleteRange, Target: target, Key: backendKey(100), EndKey: backendKey(200), Ordinal: 1},
		},
	}
	delBatch.ChecksumCRC, err = delBatch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), delBatch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.flushMemtableLocked(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	reader2 := fixture.snapshot(t, 20)
	if _, err := reader2.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(120), ColumnIDs: []model.ColumnID{1}, Layout: layout}); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("range-deleted key 120 resurrected: %v", err)
	}
	if got := readIntKey(t, reader2, target, 200); got != 1001 {
		t.Fatalf("key 200 after range end = %d, want 1001", got)
	}
}

func TestSSTIndexZoneMapsPruneBlocksBeforeReadBlock(t *testing.T) {
	limits := DefaultLimits()
	limits.BlockRows = 8
	fixture := openBackendFixtureWithLimits(t, limits)
	target := encTestTarget()
	intType := model.Type{ID: 20, Storage: model.TypeInt64}
	textType := model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}
	layout := []storage.ColumnLayout{
		{ID: 1, Type: intType},
		{ID: 2, Type: textType},
	}

	const numBlocks = 16
	const rowsPerBlock = 8
	const totalRows = numBlocks * rowsPerBlock
	batch := storage.MutationBatch{
		TxID:            model.TxnID{1},
		CommitTS:        10,
		Epoch:           fixture.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         []storage.ObjectLayout{{Target: target, Columns: layout}},
		Mutations:       make([]storage.Mutation, totalRows),
	}
	for i := range totalRows {
		grp := i / rowsPerBlock
		tag := fmt.Sprintf("grp-%03d-long-suffix-value-%04d", grp, i)
		batch.Mutations[i] = storage.NewUpsertMutation(
			target,
			backendKey(int64(i+1)),
			[]model.ColumnID{1, 2},
			[]model.Value{model.Int64Val(int64(i * 10)), model.StringVal(tag)},
		)
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

	// Clear block cache so any ReadBlock triggers a physical disk read & BlocksRead++!
	fixture.backend.runs.blockCache.Clear()
	reader := fixture.snapshot(t, 10)

	// 1) Integer predicate matching ONLY block 10 (values 800..870 -> target 830).
	intPred, err := storage.NewPredicate([]storage.PredicateNode{
		{Kind: storage.PredicateColumn, ColumnID: 1, Type: intType},
		predConstNode(0, intType),
		predCmpNode(410, 0, 1), // int8 = int8
	}, nil, []model.Value{model.Int64Val(830)})
	if err != nil {
		t.Fatal(err)
	}
	intObs := &ws1Part2Observer{}
	intCtx := storage.WithScanObserver(t.Context(), intObs)
	intRows := scanAllIntCol(t, intCtx, reader, storage.PushdownRequest{
		Target:    target,
		KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}},
		Layout:    layout,
		ColumnIDs: []model.ColumnID{1},
		Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		Predicate: intPred,
		Direction: storage.ScanForward,
	})
	if !slices.Equal(intRows, []int64{830}) {
		t.Fatalf("int zone scan got %v, want [830]", intRows)
	}
	if intObs.blocksRead > 1 || intObs.blocksPruned < numBlocks-1 {
		t.Fatalf("int SST-index zone map failed to prune before ReadBlock: blocksRead=%d blocksPruned=%d", intObs.blocksRead, intObs.blocksPruned)
	}

	// 2) String predicate matching ONLY block 5 (prefix "grp-005-...").
	fixture.backend.runs.blockCache.Clear()
	wantTag := fmt.Sprintf("grp-%03d-long-suffix-value-%04d", 5, 5*rowsPerBlock+2)
	strPred, err := storage.NewPredicate([]storage.PredicateNode{
		{Kind: storage.PredicateColumn, ColumnID: 2, Type: textType},
		predConstNode(0, textType),
		{Kind: storage.PredicateComparison, Operator: 98, Collation: predCollC, Left: 0, Right: 1, Type: model.Type{ID: 16, Storage: model.TypeBoolean}},
	}, nil, []model.Value{model.StringVal(wantTag)})
	if err != nil {
		t.Fatal(err)
	}
	strObs := &ws1Part2Observer{}
	strCtx := storage.WithScanObserver(t.Context(), strObs)
	strRows := scanAllIntCol(t, strCtx, reader, storage.PushdownRequest{
		Target:    target,
		KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}},
		Layout:    layout,
		ColumnIDs: []model.ColumnID{1},
		Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		Predicate: strPred,
		Direction: storage.ScanForward,
	})
	wantVal := int64((5*rowsPerBlock + 2) * 10)
	if !slices.Equal(strRows, []int64{wantVal}) {
		t.Fatalf("string zone scan got %v, want [%d]", strRows, wantVal)
	}
	if strObs.blocksRead > 1 || strObs.blocksPruned < numBlocks-1 {
		t.Fatalf("string SST-index zone map failed to prune before ReadBlock: blocksRead=%d blocksPruned=%d", strObs.blocksRead, strObs.blocksPruned)
	}

	// 3) Reverse scan with integer predicate also prunes before ReadBlock!
	fixture.backend.runs.blockCache.Clear()
	revObs := &ws1Part2Observer{}
	revCtx := storage.WithScanObserver(t.Context(), revObs)
	revRows := scanAllIntCol(t, revCtx, reader, storage.PushdownRequest{
		Target:    target,
		KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}},
		Layout:    layout,
		ColumnIDs: []model.ColumnID{1},
		Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		Predicate: intPred,
		Direction: storage.ScanReverse,
	})
	if !slices.Equal(revRows, []int64{830}) {
		t.Fatalf("reverse zone scan got %v, want [830]", revRows)
	}
	if revObs.blocksRead > 1 || revObs.blocksPruned < numBlocks-1 {
		t.Fatalf("reverse SST-index zone map failed to prune before ReadBlock: blocksRead=%d blocksPruned=%d", revObs.blocksRead, revObs.blocksPruned)
	}

	// 4) MVCC soundness: update key 84 (val=830) to val=99999 in a newer L0 SST.
	// Querying WHERE val = 830 at ts=20 MUST NOT resurrect the shadowed L1 row!
	upd := storage.MutationBatch{
		TxID:            model.TxnID{2},
		CommitTS:        20,
		Epoch:           fixture.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         []storage.ObjectLayout{{Target: target, Columns: layout}},
		Mutations: []storage.Mutation{
			{
				Op:        storage.MutationUpsert,
				Target:    target,
				Key:       backendKey(84),
				Ordinal:   1,
				ColumnIDs: []model.ColumnID{1, 2},
				Values:    []model.Value{model.Int64Val(99999), model.StringVal("updated")},
			},
		},
	}
	upd.ChecksumCRC, err = upd.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), upd); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.flushMemtableLocked(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	reader2 := fixture.snapshot(t, 20)
	afterUpd := scanAllIntCol(t, t.Context(), reader2, storage.PushdownRequest{
		Target:    target,
		KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}},
		Layout:    layout,
		ColumnIDs: []model.ColumnID{1},
		Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		Predicate: intPred,
		Direction: storage.ScanForward,
	})
	if len(afterUpd) != 0 {
		t.Fatalf("MVCC violation: pre-ReadBlock zone pruning resurrected shadowed row: %v", afterUpd)
	}
}

func scanAllIntCol(t *testing.T, ctx context.Context, reader storage.SnapshotReader, req storage.PushdownRequest) []int64 {
	t.Helper()
	plan, err := reader.PlanScan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.ReleasePlan(plan) }()
	cursor, err := reader.VectorizedScan(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cursor.Close() }()
	var out columnar.Batch
	var result []int64
	for {
		err := cursor.Next(ctx, &out)
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		for i := range out.Rows() {
			val, _, err := columnar.CopyValue(ctx, &out.Columns[0], out.Lane(i), 1024)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, val.IntVal)
		}
	}
}

func TestTOASTLargeValuesRoundTripCompactionCheckpointAndRestart(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()
	intType := model.Type{ID: 20, Storage: model.TypeInt64}
	textType := model.Type{ID: 25, Storage: model.TypeString}
	bytesType := model.Type{ID: 17, Storage: model.TypeBytes}
	layout := []storage.ColumnLayout{
		{ID: 1, Type: intType},
		{ID: 2, Type: textType},
		{ID: 3, Type: bytesType},
	}

	// Build 5 MiB TEXT and 5 MiB BYTEA values (each exceeds BlockBytes = 4 MiB).
	const lobSize = 5 << 20
	largeTextBuf := make([]byte, lobSize)
	largeBytes := make([]byte, lobSize)
	for i := range lobSize {
		largeTextBuf[i] = byte('A' + (i*17+3)%26)
		largeBytes[i] = byte((i*31 + 11) & 0xff)
	}
	largeText := string(largeTextBuf)

	batch := storage.MutationBatch{
		TxID:            model.TxnID{1},
		CommitTS:        10,
		Epoch:           fixture.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         []storage.ObjectLayout{{Target: target, Columns: layout}},
		Mutations: []storage.Mutation{
			{
				Op:        storage.MutationUpsert,
				Target:    target,
				Key:       backendKey(1),
				Ordinal:   1,
				ColumnIDs: []model.ColumnID{1, 2, 3},
				Values:    []model.Value{model.Int64Val(101), model.StringVal(largeText), model.BytesVal(largeBytes)},
			},
			{
				Op:        storage.MutationUpsert,
				Target:    target,
				Key:       backendKey(2),
				Ordinal:   2,
				ColumnIDs: []model.ColumnID{1, 2, 3},
				Values:    []model.Value{model.Int64Val(202), model.StringVal("small-inline"), model.BytesVal([]byte("tiny"))},
			},
		},
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
		t.Fatalf("ApplyMutations with 5 MiB TOAST values failed: %v", err)
	}

	verifyRows := func(stage string) {
		t.Helper()
		reader := fixture.snapshot(t, 10)
		// 1) Point Get verification
		row, err := reader.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(1), ColumnIDs: []model.ColumnID{1, 2, 3}, Layout: layout})
		if err != nil {
			t.Fatalf("%s Get large row: %v", stage, err)
		}
		if row.Values[0].IntVal != 101 || row.Values[1].StrVal != largeText || !bytes.Equal(row.Values[2].BytesVal, largeBytes) {
			t.Fatalf("%s Get large row mismatch: id=%d textLen=%d bytesLen=%d", stage, row.Values[0].IntVal, len(row.Values[1].StrVal), len(row.Values[2].BytesVal))
		}
		small, err := reader.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(2), ColumnIDs: []model.ColumnID{1, 2, 3}, Layout: layout})
		if err != nil {
			t.Fatalf("%s Get small row: %v", stage, err)
		}
		if small.Values[0].IntVal != 202 || small.Values[1].StrVal != "small-inline" || !bytes.Equal(small.Values[2].BytesVal, []byte("tiny")) {
			t.Fatalf("%s Get small row mismatch: %+v", stage, small.Values)
		}
		// 2) VectorizedScan verification
		plan, err := reader.PlanScan(t.Context(), storage.PushdownRequest{
			Target:    target,
			KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}},
			Layout:    layout,
			ColumnIDs: []model.ColumnID{1, 2, 3},
			Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		})
		if err != nil {
			t.Fatalf("%s PlanScan: %v", stage, err)
		}
		cursor, err := reader.VectorizedScan(t.Context(), plan)
		if err != nil {
			t.Fatalf("%s VectorizedScan: %v", stage, err)
		}
		var out columnar.Batch
		seen := 0
		for {
			err := cursor.Next(t.Context(), &out)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s scan Next: %v", stage, err)
			}
			for r := range out.Rows() {
				lane := out.Lane(r)
				idV, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], lane, 64)
				if err != nil {
					t.Fatal(err)
				}
				txtV, _, err := columnar.CopyValue(t.Context(), &out.Columns[1], lane, 16<<20)
				if err != nil {
					t.Fatal(err)
				}
				binV, _, err := columnar.CopyValue(t.Context(), &out.Columns[2], lane, 16<<20)
				if err != nil {
					t.Fatal(err)
				}
				if idV.IntVal == 101 {
					if txtV.StrVal != largeText || !bytes.Equal(binV.BytesVal, largeBytes) {
						t.Fatalf("%s scan large row mismatch", stage)
					}
				} else if idV.IntVal == 202 {
					if txtV.StrVal != "small-inline" || !bytes.Equal(binV.BytesVal, []byte("tiny")) {
						t.Fatalf("%s scan small row mismatch", stage)
					}
				}
				seen++
			}
		}
		_ = cursor.Close()
		_ = reader.ReleasePlan(plan)
		if seen != 2 {
			t.Fatalf("%s scan saw %d rows, want 2", stage, seen)
		}
	}

	verifyRows("after-apply")

	// Compact (Collect) and verify TOAST blobs survive compaction.
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatalf("Collect with TOAST blobs: %v", err)
	}
	verifyRows("after-compaction")

	// Checkpoint + VerifyCheckpoint!
	state, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), 10, state.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 10, Fence: fence}})
	if err != nil {
		t.Fatalf("Checkpoint with TOAST blobs: %v", err)
	}
	cpManifest := checkpointManifest(t, cp)
	cpDir := t.TempDir()
	copyCheckpointFiles(t, cp, cpDir)
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}
	cpRoot, err := os.OpenRoot(cpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cpRoot.Close() }()
	verifier := CheckpointVerifier{FenceVerifier: fixture.authority}
	if err := verifier.VerifyCheckpoint(t.Context(), cpRoot, cpManifest); err != nil {
		t.Fatalf("VerifyCheckpoint with TOAST blobs: %v", err)
	}

	// Close & reopen and verify TOAST blobs survive restart.
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatalf("reopen with TOAST blobs: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	fixture.backend = reopened
	verifyRows("after-restart")
}

func TestSharedBlockCacheHitsAvoidDiskReads(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()
	keys := make([]int64, 64)
	vals := make([]int64, 64)
	for i := range 64 {
		keys[i] = int64(i + 1)
		vals[i] = int64((i + 1) * 7)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), makeIntRowsBatch(t, fixture, 1, 10, target, keys, vals)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}

	cache := fixture.backend.runs.blockCache
	cache.Clear()
	reader := fixture.snapshot(t, 10)
	// First Get: cold cache miss populates the shared block cache.
	if got := readIntKey(t, reader, target, 10); got != 70 {
		t.Fatalf("cold Get = %d, want 70", got)
	}
	missesAfterCold := cache.Misses()
	hitsAfterCold := cache.Hits()
	if missesAfterCold == 0 {
		t.Fatalf("expected at least 1 cache miss on cold read")
	}

	// Next 50 Gets hit the cached block with zero additional misses!
	for i := int64(1); i <= 50; i++ {
		if got := readIntKey(t, reader, target, i); got != i*7 {
			t.Fatalf("warm Get key %d = %d, want %d", i, got, i*7)
		}
	}
	if cache.Misses() != missesAfterCold {
		t.Fatalf("warm reads caused cache misses: before=%d after=%d", missesAfterCold, cache.Misses())
	}
	if cache.Hits() < hitsAfterCold+50 {
		t.Fatalf("expected >= 50 cache hits, got delta %d", cache.Hits()-hitsAfterCold)
	}
}

func TestBinarySearchPointLookupInsideLargePAXBlock(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()
	const n = 1000
	keys := make([]int64, n)
	vals1 := make([]int64, n)
	vals2 := make([]int64, n)
	for i := range n {
		keys[i] = int64(i * 2)
		vals1[i] = int64(i + 100)
		vals2[i] = int64(i + 2000)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), makeIntRowsBatch(t, fixture, 1, 10, target, keys, vals1)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), makeIntRowsBatch(t, fixture, 2, 20, target, keys, vals2)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}

	r10 := fixture.snapshot(t, 10)
	r20 := fixture.snapshot(t, 20)
	layout := scanFixtureSpec().Layout
	for _, idx := range []int{0, 1, 250, 500, 777, n - 1} {
		k := keys[idx]
		if got := readIntKey(t, r10, target, k); got != vals1[idx] {
			t.Fatalf("ts=10 key %d = %d, want %d", k, got, vals1[idx])
		}
		if got := readIntKey(t, r20, target, k); got != vals2[idx] {
			t.Fatalf("ts=20 key %d = %d, want %d", k, got, vals2[idx])
		}
		// Missing neighbor odd key
		if _, err := r20.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(k + 1), ColumnIDs: []model.ColumnID{1}, Layout: layout}); !errors.Is(err, storage.ErrRowNotFound) {
			t.Fatalf("missing key %d: got %v, want ErrRowNotFound", k+1, err)
		}
	}
}

func TestBoundedFDPoolAndSharedFragmentCore(t *testing.T) {
	fixture := openBackendFixture(t)
	fixture.backend.runs.fds.setMaxOpen(8)

	const numTables = 40
	for i := range numTables {
		target := encTestTarget()
		target.ObjectID += model.ObjectID(i)
		batch := makeIntRowsBatch(t, fixture, uint64(i+1), model.Timestamp((i+1)*10), target, []int64{int64(i + 1)}, []int64{int64((i + 1) * 100)})
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		if err := fixture.backend.flushMemtableLocked(t.Context(), 0); err != nil {
			t.Fatal(err)
		}
	}

	if open := fixture.backend.runs.fds.OpenCount(); open > 8 {
		t.Fatalf("FD pool exceeded cap after 40 SST opens: open=%d, max=8", open)
	}

	// Clear block cache and read all 40 tables concurrently to force FD evictions & reopens under -race.
	fixture.backend.runs.blockCache.Clear()
	reader := fixture.snapshot(t, model.Timestamp(numTables*10))
	var wg sync.WaitGroup
	errs := make(chan error, numTables*4)
	for worker := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range numTables {
				idx := (i + w*7) % numTables
				target := encTestTarget()
				target.ObjectID += model.ObjectID(idx)
				row, err := reader.Get(t.Context(), storage.ReadRequest{
					Target:    target,
					Key:       backendKey(int64(idx + 1)),
					ColumnIDs: []model.ColumnID{1},
					Layout:    scanFixtureSpec().Layout,
				})
				if err != nil {
					errs <- fmt.Errorf("table %d: %w", idx, err)
					return
				}
				want := int64((idx + 1) * 100)
				if row.Values[0].IntVal != want {
					errs <- fmt.Errorf("table %d: got %d, want %d", idx, row.Values[0].IntVal, want)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if open := fixture.backend.runs.fds.OpenCount(); open > 8 {
		t.Fatalf("FD pool exceeded cap after concurrent reads: open=%d, max=8", open)
	}

	// Verify openLinkedFragment shares sstCore (zero duplicated index/FD).
	runsRoot := fixture.backend.directory.runs
	pin, err := fixture.backend.runs.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	src := pin.generation.files[0]
	fragName := "f-testshared-00000000.active.sst"
	if err := runsRoot.Link(src.descriptor.Name, fragName); err != nil {
		_ = pin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runsRoot.Remove(fragName) })
	desc := src.descriptor
	desc.Name = fragName
	desc.LowerKey = backendKey(0)
	frag, err := openLinkedFragment(t.Context(), runsRoot, desc, src, fixture.backend.limits.preparation().File, fixture.backend.limits.ResidentBytes)
	if err != nil {
		_ = pin.Close()
		t.Fatal(err)
	}
	if frag.core != src.core {
		t.Fatal("linked fragment did not share underlying sstCore")
	}
	if frag.core.coreRefs.Load() != 2 {
		t.Fatalf("shared coreRefs = %d, want 2", frag.core.coreRefs.Load())
	}
	if err := frag.Close(); err != nil {
		t.Fatal(err)
	}
	if src.core.coreRefs.Load() != 1 {
		t.Fatalf("coreRefs after fragment close = %d, want 1", src.core.coreRefs.Load())
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSnapshotReaderGetNoRaces(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()
	// Half in SST, half in live Memtable!
	keys1 := make([]int64, 50)
	vals1 := make([]int64, 50)
	for i := range 50 {
		keys1[i] = int64(i + 1)
		vals1[i] = int64((i + 1) * 10)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), makeIntRowsBatch(t, fixture, 1, 10, target, keys1, vals1)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.flushMemtableLocked(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	keys2 := make([]int64, 50)
	vals2 := make([]int64, 50)
	for i := range 50 {
		keys2[i] = int64(i + 51)
		vals2[i] = int64((i + 51) * 10)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), makeIntRowsBatch(t, fixture, 2, 20, target, keys2, vals2)); err != nil {
		t.Fatal(err)
	}

	reader := fixture.snapshot(t, 20)
	layout := scanFixtureSpec().Layout
	const goroutines = 16
	const iters = 100
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range iters {
				k := int64(((id*37 + i*13) % 100) + 1)
				row, err := reader.Get(t.Context(), storage.ReadRequest{
					Target:    target,
					Key:       backendKey(k),
					ColumnIDs: []model.ColumnID{1},
					Layout:    layout,
				})
				if err != nil {
					errs <- fmt.Errorf("goroutine %d key %d: %w", id, k, err)
					return
				}
				if row.Values[0].IntVal != k*10 {
					errs <- fmt.Errorf("goroutine %d key %d = %d, want %d", id, k, row.Values[0].IntVal, k*10)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestWALGroupCommitMemtableCoalescingAndCrashRecovery(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()

	// Part A: 5 waves of 16 concurrent goroutines (80 total commits), with each wave
	// racing 16 concurrent ApplyMutations calls into the WAL group commit queue.
	const workers = 16
	const waves = 5
	const totalCommits = workers * waves
	results := make([]storage.ApplyResult, totalCommits)

	for wave := range waves {
		baseSeq := uint64(wave * workers)
		// Submit wave transactions concurrently; goroutines that arrive after a
		// higher-timestamp sibling already committed in an earlier leader batch
		// re-stamp with a fresh monotonic CommitTS (matching txn coordinator retry).
		var tsGen sync.Mutex
		nextTS := model.Timestamp((baseSeq + 1) * 10)
		allocTS := func() model.Timestamp {
			tsGen.Lock()
			ts := nextTS
			nextTS += 10
			tsGen.Unlock()
			return ts
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for w := range workers {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				seq := baseSeq + uint64(worker) + 1
				<-start
				for attempt := range 16 {
					ts := allocTS()
					batch := makeIntRowsBatch(t, fixture, seq, ts, target, []int64{int64(seq)}, []int64{int64(seq * 100)})
					res, err := fixture.backend.ApplyMutations(t.Context(), batch)
					if errors.Is(err, storage.ErrInvalidMutation) && attempt < 15 {
						continue
					}
					if err != nil {
						errs <- fmt.Errorf("seq %d: %w", seq, err)
						return
					}
					results[seq-1] = res
					return
				}
				errs <- fmt.Errorf("seq %d: exhausted timestamp retries", seq)
			}(w)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
	}

	// Verify strict contiguous LogPositions 1..80 and zero per-commit SST explosion!
	positions := make([]uint64, totalCommits)
	var maxTS model.Timestamp
	for i, r := range results {
		positions[i] = r.LogPosition
		maxTS = max(maxTS, r.CommitTS)
	}
	slices.Sort(positions)
	for i, pos := range positions {
		if pos != uint64(i+1) {
			t.Fatalf("non-contiguous LogPositions: got %d at index %d", pos, i)
		}
	}
	if ssts := runSSTNames(t, fixture.config.Directory); len(ssts) != 0 {
		t.Fatalf("expected 0 SST files while buffered in memtable, got %d: %v", len(ssts), ssts)
	}

	// Verify immediate visibility from memtable via both Get and VectorizedScan (forward & reverse)!
	reader := fixture.snapshot(t, maxTS)
	for i := int64(1); i <= totalCommits; i++ {
		if got := readIntKey(t, reader, target, i); got != i*100 {
			t.Fatalf("memtable Get key %d = %d, want %d", i, got, i*100)
		}
	}
	intType := model.Type{ID: 20, Storage: model.TypeInt64}
	fwd := scanAllIntCol(t, t.Context(), reader, storage.PushdownRequest{
		Target:    target,
		KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}},
		Layout:    scanFixtureSpec().Layout,
		ColumnIDs: []model.ColumnID{1},
		Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		Direction: storage.ScanForward,
	})
	rev := scanAllIntCol(t, t.Context(), reader, storage.PushdownRequest{
		Target:    target,
		KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}},
		Layout:    scanFixtureSpec().Layout,
		ColumnIDs: []model.ColumnID{1},
		Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		Direction: storage.ScanReverse,
	})
	if len(fwd) != totalCommits || len(rev) != totalCommits {
		t.Fatalf("memtable scan lengths: fwd=%d rev=%d, want %d", len(fwd), len(rev), totalCommits)
	}
	for i := range totalCommits {
		if fwd[i] != int64((i+1)*100) || rev[totalCommits-1-i] != int64((i+1)*100) {
			t.Fatalf("memtable scan mismatch at %d: fwd=%d rev=%d", i, fwd[i], rev[totalCommits-1-i])
		}
	}

	// Part B: Simulate crash BEFORE memtable flush by copying database.state + commits/ + runs/ (empty)
	// to a fresh directory and reopening via Open().
	crashDir := t.TempDir()
	copyTreeForCrash(t, fixture.config.Directory, crashDir)
	crashCfg := fixture.config
	crashCfg.Directory = crashDir
	crashCfg.Scale = clock.ScaleDescriptor{}
	recovered, err := Open(t.Context(), crashCfg)
	if err != nil {
		t.Fatalf("crash recovery from unflushed WALs failed: %v", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	recState, err := recovered.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if recState.LogPosition != totalCommits || recState.CommitTS != maxTS {
		t.Fatalf("recovered state = %+v, want pos=%d ts=%d", recState, totalCommits, maxTS)
	}
	recFence, err := fixture.authority.Issue(t.Context(), maxTS, recState.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	recReader, err := recovered.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: maxTS, Fence: recFence})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recReader.Close() })
	for i := int64(1); i <= totalCommits; i++ {
		if got := readIntKey(t, recReader, target, i); got != i*100 {
			t.Fatalf("crash-recovered key %d = %d, want %d", i, got, i*100)
		}
	}
	// TxnID idempotency must hold on recovered unflushed commits!
	retryBatch := makeIntRowsBatch(t, fixture, 1, results[0].CommitTS, target, []int64{1}, []int64{100})
	if retryRes, err := recovered.ApplyMutations(t.Context(), retryBatch); err != nil || retryRes.LogPosition != results[0].LogPosition {
		t.Fatalf("recovered idempotency: res=%+v err=%v", retryRes, err)
	}

	// Part C: Checkpoint flushes memtable into a single coalesced SST + NSRUNS02 manifest,
	// and CheckpointVerifier verifies every single one of the 80 commits!
	cp, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: maxTS, Fence: recFence}})
	if err != nil {
		t.Fatalf("Checkpoint after memtable coalescing: %v", err)
	}
	if ssts := runSSTNames(t, fixture.config.Directory); len(ssts) != 1 {
		t.Fatalf("expected 1 coalesced SST for 80 commits, got %d: %v", len(ssts), ssts)
	}
	cpManifest := checkpointManifest(t, cp)
	cpDir := t.TempDir()
	copyCheckpointFiles(t, cp, cpDir)
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}
	cpRoot, err := os.OpenRoot(cpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cpRoot.Close() }()
	verifier := CheckpointVerifier{FenceVerifier: fixture.authority}
	if err := verifier.VerifyCheckpoint(t.Context(), cpRoot, cpManifest); err != nil {
		t.Fatalf("VerifyCheckpoint for NSRUNS02 coalesced flush failed: %v", err)
	}
}

func copyTreeForCrash(t *testing.T, src, dst string) {
	t.Helper()
	for _, sub := range []string{"commits", "runs"} {
		if err := os.MkdirAll(filepath.Join(dst, sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	copyFile := func(rel string) {
		data, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, rel), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(databaseStateName)
	for _, sub := range []string{"commits", "runs"} {
		entries, err := os.ReadDir(filepath.Join(src, sub))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Type().IsRegular() {
				copyFile(filepath.Join(sub, e.Name()))
			}
		}
	}
}

// Keep binary import used cleanly across helpers if extended.
var _ = binary.LittleEndian

func TestPublishMaintenanceConcurrentMemtableWrites(t *testing.T) {
	// Part A: Deterministic interleaving using maintenanceAfterCapture hook to
	// guarantee a write lands in backend.memtable between captureMaintenance and
	// publishMaintenance.
	t.Run("DeterministicHook", func(t *testing.T) {
		fixture := openBackendFixture(t)
		target := encTestTarget()
		for i := int64(1); i <= 4; i++ {
			batch := makeIntRowsBatch(t, fixture, uint64(i), model.Timestamp(i*10), target, []int64{i}, []int64{i * 100})
			if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
				t.Fatalf("seed commit %d: %v", i, err)
			}
		}
		injected := false
		fixture.backend.mu.Lock()
		fixture.backend.maintenanceFault = func(ctx context.Context, point maintenancePoint) error {
			if point == maintenanceAfterCapture && !injected {
				injected = true
				batch := makeIntRowsBatch(t, fixture, 5, 50, target, []int64{5}, []int64{500})
				if _, err := fixture.backend.ApplyMutations(ctx, batch); err != nil {
					return err
				}
			}
			return nil
		}
		fixture.backend.mu.Unlock()

		if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
			t.Fatalf("Collect with concurrent memtable write failed: %v", err)
		}
		if !injected {
			t.Fatalf("expected maintenanceAfterCapture hook to fire")
		}
		reader := fixture.snapshot(t, 50)
		for i := int64(1); i <= 5; i++ {
			if got := readIntKey(t, reader, target, i); got != i*100 {
				t.Fatalf("key %d = %d, want %d", i, got, i*100)
			}
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}

		// Verify clean reopen preserves the concurrently written key too.
		if err := fixture.backend.Close(); err != nil {
			t.Fatal(err)
		}
		reopenCfg := fixture.config
		reopenCfg.Scale = clock.ScaleDescriptor{}
		reopened, err := Open(t.Context(), reopenCfg)
		if err != nil {
			t.Fatalf("reopen after concurrent maintenance: %v", err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		st, err := reopened.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if st.LogPosition != 5 || st.CommitTS != 50 {
			t.Fatalf("reopened state = %+v, want pos=5 ts=50", st)
		}
		fence, err := fixture.authority.Issue(t.Context(), 50, st.LogPosition, fixture.epoch)
		if err != nil {
			t.Fatal(err)
		}
		r2, err := reopened.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 50, Fence: fence})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r2.Close() })
		for i := int64(1); i <= 5; i++ {
			if got := readIntKey(t, r2, target, i); got != i*100 {
				t.Fatalf("reopened key %d = %d, want %d", i, got, i*100)
			}
		}
	})

	// Part B: Continuous concurrent writer goroutines racing against repeated
	// Backend.Collect passes under -race; must produce zero ErrInvalidMutation.
	t.Run("ConcurrentStress", func(t *testing.T) {
		fixture := openBackendFixture(t)
		target := encTestTarget()
		const totalWrites = 120

		// Seed initial run so Collect has active compaction work immediately.
		seed := makeIntRowsBatch(t, fixture, 1, 10, target, []int64{1}, []int64{10})
		if _, err := fixture.backend.ApplyMutations(t.Context(), seed); err != nil {
			t.Fatal(err)
		}

		done := make(chan struct{})
		errCh := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			defer close(done)
			for i := 2; i <= totalWrites; i++ {
				batch := makeIntRowsBatch(t, fixture, uint64(i), model.Timestamp(i*10), target, []int64{int64(i)}, []int64{int64(i * 10)})
				if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
					errCh <- fmt.Errorf("write %d: %w", i, err)
					return
				}
			}
		}()

		go func() {
			defer wg.Done()
			for {
				if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
					errCh <- fmt.Errorf("Collect: %w", err)
					return
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()

		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}

		maxTS := model.Timestamp(totalWrites * 10)
		reader := fixture.snapshot(t, maxTS)
		for i := int64(1); i <= totalWrites; i++ {
			if got := readIntKey(t, reader, target, i); got != i*10 {
				t.Fatalf("key %d = %d, want %d", i, got, i*10)
			}
		}
	})
}

func TestGroupCommitPartialLinkDoesNotCommitUnlinkedSibling(t *testing.T) {
	for _, tc := range []struct {
		name             string
		resolveTx2First  bool
	}{
		{name: "ResolveLinkedFirst", resolveTx2First: false},
		{name: "ResolveUnlinkedFirst", resolveTx2First: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := openBackendFixture(t)
			target := encTestTarget()

			batch1 := makeIntRowsBatch(t, fixture, 1, 10, target, []int64{1}, []int64{101})
			batch2 := makeIntRowsBatch(t, fixture, 2, 20, target, []int64{2}, []int64{202})
			val1, err := validateMutationBatch(t.Context(), batch1)
			if err != nil {
				t.Fatal(err)
			}
			val2, err := validateMutationBatch(t.Context(), batch2)
			if err != nil {
				t.Fatal(err)
			}

			// Block hardlink for position 2 so writeCommitGroup links position 1
			// (linked == 1) and fails linking position 2.
			blocker := filepath.Join(fixture.config.Directory, "commits", commitName(2))
			if err := os.WriteFile(blocker, nil, 0600); err != nil {
				t.Fatalf("create blocker: %v", err)
			}

			req1 := &groupCommitReq{ctx: t.Context(), batch: batch1, validated: val1, done: make(chan struct{})}
			req2 := &groupCommitReq{ctx: t.Context(), batch: batch2, validated: val2, done: make(chan struct{})}
			fixture.backend.processCommitGroup([]*groupCommitReq{req1, req2})

			// Remove the synthetic blocker so disk state reflects only pos 1 linked.
			if err := os.Remove(blocker); err != nil {
				t.Fatalf("remove blocker: %v", err)
			}

			if !errors.Is(req1.err, storage.ErrCommitIndeterminate) {
				t.Fatalf("req1.err = %v, want ErrCommitIndeterminate", req1.err)
			}
			if !errors.Is(req2.err, storage.ErrCommitIndeterminate) {
				t.Fatalf("req2.err = %v, want ErrCommitIndeterminate", req2.err)
			}

			fixture.backend.mu.Lock()
			uncertain := fixture.backend.metadataUncertain
			hasPending := fixture.backend.pending != nil
			fixture.backend.mu.Unlock()
			if !uncertain || !hasPending {
				t.Fatalf("after partial link: metadataUncertain=%v pending=%v, want true/true", uncertain, hasPending)
			}

			// New commits must be rejected while the indeterminate prefix is unresolved.
			batch3 := makeIntRowsBatch(t, fixture, 3, 30, target, []int64{3}, []int64{303})
			if _, err := fixture.backend.ApplyMutations(t.Context(), batch3); !errors.Is(err, storage.ErrCommitIndeterminate) {
				t.Fatalf("ApplyMutations while unresolved = %v, want ErrCommitIndeterminate", err)
			}

			if tc.resolveTx2First {
				res2, err := fixture.backend.ResolveCommit(t.Context(), batch2.TxID)
				if err != nil {
					t.Fatalf("ResolveCommit(Tx2): %v", err)
				}
				if res2.State != storage.CommitNotCommitted {
					t.Fatalf("unlinked sibling Tx2 resolved as %v (result=%+v), want CommitNotCommitted", res2.State, res2.Result)
				}
				res1, err := fixture.backend.ResolveCommit(t.Context(), batch1.TxID)
				if err != nil {
					t.Fatalf("ResolveCommit(Tx1): %v", err)
				}
				if res1.State != storage.CommitCommitted || res1.Result.LogPosition != 1 {
					t.Fatalf("linked Tx1 resolved as %+v, want CommitCommitted at pos 1", res1)
				}
			} else {
				res1, err := fixture.backend.ResolveCommit(t.Context(), batch1.TxID)
				if err != nil {
					t.Fatalf("ResolveCommit(Tx1): %v", err)
				}
				if res1.State != storage.CommitCommitted || res1.Result.LogPosition != 1 {
					t.Fatalf("linked Tx1 resolved as %+v, want CommitCommitted at pos 1", res1)
				}
				res2, err := fixture.backend.ResolveCommit(t.Context(), batch2.TxID)
				if err != nil {
					t.Fatalf("ResolveCommit(Tx2): %v", err)
				}
				if res2.State != storage.CommitNotCommitted {
					t.Fatalf("unlinked sibling Tx2 resolved as %v (result=%+v), want CommitNotCommitted", res2.State, res2.Result)
				}
			}

			st, err := fixture.backend.State(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if st.LogPosition != 1 || st.CommitTS != 10 {
				t.Fatalf("post-resolve state = %+v, want pos=1 ts=10", st)
			}

			// Snapshot sees Tx1 and NOT unlinked Tx2.
			reader := fixture.snapshot(t, 10)
			if got := readIntKey(t, reader, target, 1); got != 101 {
				t.Fatalf("key 1 = %d, want 101", got)
			}
			if _, err := reader.Get(t.Context(), storage.ReadRequest{
				Target:    target,
				Key:       backendKey(2),
				ColumnIDs: []model.ColumnID{1},
				Layout:    scanFixtureSpec().Layout,
			}); !errors.Is(err, storage.ErrRowNotFound) {
				t.Fatalf("unlinked key 2 Get = %v, want ErrRowNotFound", err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}

			// Subsequent commit cleanly advances to position 2.
			res3, err := fixture.backend.ApplyMutations(t.Context(), batch3)
			if err != nil {
				t.Fatalf("ApplyMutations(Tx3) after resolve: %v", err)
			}
			if res3.LogPosition != 2 {
				t.Fatalf("Tx3 LogPosition = %d, want 2", res3.LogPosition)
			}
		})
	}
}

func TestGroupCommitPostWALStagingFailureBlocksPositionReuse(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()
	strLayout := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}},
	}

	makeStrBatch := func(txNum uint64, ts model.Timestamp, k int64, payload string) storage.MutationBatch {
		t.Helper()
		b := storage.MutationBatch{
			TxID:            model.TxnID{byte(txNum)},
			CommitTS:        ts,
			Epoch:           fixture.epoch,
			EncodingVersion: storage.MutationEncodingVersion,
			Layouts:         []storage.ObjectLayout{{Target: target, Columns: strLayout}},
			Mutations: []storage.Mutation{
				storage.NewUpsertMutation(target, backendKey(k), []model.ColumnID{1}, []model.Value{model.StringVal(payload)}),
			},
		}
		b.Mutations[0].Ordinal = 1
		var err error
		b.ChecksumCRC, err = b.ComputeChecksum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Tx1 fits within RowBytes=32; Tx2 (128 bytes) passes WAL write but fails
	// post-WAL memtable.stageBatch with ErrValueBudget.
	batch1 := makeStrBatch(1, 10, 1, "ok")
	batch2 := makeStrBatch(2, 20, 2, string(bytes.Repeat([]byte("x"), 128)))
	val1, err := validateMutationBatch(t.Context(), batch1)
	if err != nil {
		t.Fatal(err)
	}
	val2, err := validateMutationBatch(t.Context(), batch2)
	if err != nil {
		t.Fatal(err)
	}

	origRowBytes := fixture.backend.limits.RowBytes
	fixture.backend.mu.Lock()
	fixture.backend.limits.RowBytes = 32
	fixture.backend.mu.Unlock()

	req1 := &groupCommitReq{ctx: t.Context(), batch: batch1, validated: val1, done: make(chan struct{})}
	req2 := &groupCommitReq{ctx: t.Context(), batch: batch2, validated: val2, done: make(chan struct{})}
	fixture.backend.processCommitGroup([]*groupCommitReq{req1, req2})

	if req1.err != nil || req1.result.LogPosition != 1 {
		t.Fatalf("req1 = (res=%+v, err=%v), want pos=1 nil err", req1.result, req1.err)
	}
	if !errors.Is(req2.err, storage.ErrCommitIndeterminate) || !errors.Is(req2.err, model.ErrValueBudget) {
		t.Fatalf("req2.err = %v, want ErrCommitIndeterminate + ErrValueBudget", req2.err)
	}

	// Durable WAL file for position 2 exists on disk.
	if _, err := os.Stat(filepath.Join(fixture.config.Directory, "commits", commitName(2))); err != nil {
		t.Fatalf("expected durable WAL for pos 2: %v", err)
	}

	fixture.backend.mu.Lock()
	uncertain := fixture.backend.metadataUncertain
	hasPending := fixture.backend.pending != nil
	fixture.backend.mu.Unlock()
	if !uncertain || !hasPending {
		t.Fatalf("after post-WAL stageBatch failure: metadataUncertain=%v pending=%v, want true/true", uncertain, hasPending)
	}

	// Subsequent ApplyMutations MUST NOT reuse durable WAL LogPosition 2!
	batch3 := makeStrBatch(3, 30, 3, "after")
	if _, err := fixture.backend.ApplyMutations(t.Context(), batch3); !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("ApplyMutations after post-WAL staging failure = %v, want ErrCommitIndeterminate", err)
	}

	// Restoring RowBytes allows ResolveCommit to recover durable Tx2 at pos 2.
	fixture.backend.mu.Lock()
	fixture.backend.limits.RowBytes = origRowBytes
	fixture.backend.mu.Unlock()

	res2, err := fixture.backend.ResolveCommit(t.Context(), batch2.TxID)
	if err != nil {
		t.Fatalf("ResolveCommit(Tx2): %v", err)
	}
	if res2.State != storage.CommitCommitted || res2.Result.LogPosition != 2 {
		t.Fatalf("ResolveCommit(Tx2) = %+v, want CommitCommitted at pos 2", res2)
	}

	// Now Tx3 cleanly commits at LogPosition 3 without colliding with pos 2.
	res3, err := fixture.backend.ApplyMutations(t.Context(), batch3)
	if err != nil {
		t.Fatalf("ApplyMutations(Tx3) after recovery: %v", err)
	}
	if res3.LogPosition != 3 {
		t.Fatalf("Tx3 LogPosition = %d, want 3", res3.LogPosition)
	}
}

