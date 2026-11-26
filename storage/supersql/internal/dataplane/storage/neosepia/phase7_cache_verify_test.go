package neosepia

import (
	"encoding/binary"
	"errors"
	"os"
	"sync"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

func buildCoalescedCheckpointFixture(t *testing.T, commitCount int, rowsPerCommit int) (storage.CheckpointManifest, *os.Root, storage.FenceVerifier) {
	t.Helper()
	fixture := openBackendFixture(t)
	target := encTestTarget()
	var maxTS model.Timestamp
	var lastPos uint64
	for c := 1; c <= commitCount; c++ {
		ts := model.Timestamp(c * 10)
		keys := make([]int64, rowsPerCommit)
		vals := make([]int64, rowsPerCommit)
		for r := range rowsPerCommit {
			k := int64((c-1)*rowsPerCommit + r + 1)
			keys[r] = k
			vals[r] = k * 7
		}
		res, err := fixture.backend.ApplyMutations(t.Context(), makeIntRowsBatch(t, fixture, uint64(c), ts, target, keys, vals))
		if err != nil {
			t.Fatalf("ApplyMutations(%d): %v", c, err)
		}
		maxTS = res.CommitTS
		lastPos = res.LogPosition
	}
	fence, err := fixture.authority.Issue(t.Context(), maxTS, lastPos, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{
		Snapshot: storage.SnapshotSpec{Timestamp: maxTS, Fence: fence},
	})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	manifest := checkpointManifest(t, cp)
	dir := t.TempDir()
	copyCheckpointFiles(t, cp, dir)
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return manifest, root, fixture.authority
}

func TestPhase7CacheVerifyCoalescedIdentityBudget(t *testing.T) {
	const (
		commits       = 25
		rowsPerCommit = 10
	)
	manifest, root, fenceVerifier := buildCoalescedCheckpointFixture(t, commits, rowsPerCommit)

	// Baseline: DefaultLimits succeeds on the coalesced NSRUNS02 checkpoint.
	baseVerifier := CheckpointVerifier{FenceVerifier: fenceVerifier, Limits: DefaultLimits()}
	if err := baseVerifier.VerifyCheckpoint(t.Context(), root, manifest); err != nil {
		t.Fatalf("baseline VerifyCheckpoint failed: %v", err)
	}

	// Compute the cumulative identity bytes for all coalesced mutations.
	// A budget big enough for several single commits (so per-commit-only checks
	// would bypass it) but smaller than cumulative identities + transaction tree
	// must be rejected with columnar.ErrBatchBudget or ordered.ErrBudget.
	totalIdentitiesBytes := uint64(commits*rowsPerCommit) * uint64(unsafe.Sizeof(checkpointCoalescedIdentity{}))
	singleCommitIdentities := uint64(rowsPerCommit) * uint64(unsafe.Sizeof(checkpointCoalescedIdentity{}))
	if totalIdentitiesBytes <= singleCommitIdentities*4 {
		t.Fatalf("unexpectedly small identity span: total=%d single=%d", totalIdentitiesBytes, singleCommitIdentities)
	}

	// Case 1: Tight IdentityBytes that fits single commits easily, but cannot hold
	// all coalesced identities cumulatively -> must reject with columnar.ErrBatchBudget
	// (or ordered.ErrBudget when transactions.Set hits the tightened remaining budget).
	tightLimits := DefaultLimits()
	tightLimits.IdentityBytes = totalIdentitiesBytes / 2
	err := (CheckpointVerifier{FenceVerifier: fenceVerifier, Limits: tightLimits}).VerifyCheckpoint(t.Context(), root, manifest)
	if !errors.Is(err, columnar.ErrBatchBudget) && !errors.Is(err, ordered.ErrBudget) {
		t.Fatalf("VerifyCheckpoint(IdentityBytes=%d) = %v, want columnar.ErrBatchBudget or ordered.ErrBudget", tightLimits.IdentityBytes, err)
	}

	// Case 2: IdentityBytes just enough for allIdentities alone, leaving no room
	// for check.transactions.Set once cumulative identitiesBytes is subtracted.
	// Without subtracting cumulative identitiesBytes in check.transactions.Set,
	// Set would have seen check.limits.IdentityBytes - check.transactions.NodeBytes()
	// and bypassed the budget!
	setTightLimits := DefaultLimits()
	setTightLimits.IdentityBytes = totalIdentitiesBytes + 64
	err = (CheckpointVerifier{FenceVerifier: fenceVerifier, Limits: setTightLimits}).VerifyCheckpoint(t.Context(), root, manifest)
	if !errors.Is(err, columnar.ErrBatchBudget) && !errors.Is(err, ordered.ErrBudget) {
		t.Fatalf("VerifyCheckpoint(IdentityBytes=%d) = %v, want columnar.ErrBatchBudget or ordered.ErrBudget", setTightLimits.IdentityBytes, err)
	}
}

func TestPhase7CacheVerifyCoalescedMetadataBudget(t *testing.T) {
	const (
		commits       = 30
		rowsPerCommit = 2
	)
	manifest, root, fenceVerifier := buildCoalescedCheckpointFixture(t, commits, rowsPerCommit)

	// Measure exact pre-decision metadata overhead (manifest + state) so we can
	// target verifyCoalescedDecisions's decisions slice and cumulative layoutsBytes.
	unit := uint64(unsafe.Sizeof(storage.CheckpointFile{})) + 1
	baseMeta := uint64(len(manifest.Files))*unit + uint64(len(manifest.Scale.Name)) + uint64(len(manifest.Scale.CanonicalLeapHistory))
	var stateFileSize uint64
	for _, f := range manifest.Files {
		baseMeta += uint64(len(f.Name))
		if f.Name == databaseStateName {
			stateFileSize = f.Size
		}
	}
	baseMeta += stateFileSize + uint64(unsafe.Sizeof(scaleVersion{}))

	decisionsBytes := uint64(commits) * uint64(unsafe.Sizeof(runDecision{}))

	// Case 1: Enough MetadataBytes to pass validateManifest + readDatabaseState,
	// plus 1 byte headroom, but NOT enough for the coalesced decisions slice.
	// Must fail with columnar.ErrBatchBudget BEFORE allocating decisions.
	decisionsTight := DefaultLimits()
	decisionsTight.MetadataBytes = baseMeta + decisionsBytes/2
	err := (CheckpointVerifier{FenceVerifier: fenceVerifier, Limits: decisionsTight}).VerifyCheckpoint(t.Context(), root, manifest)
	if !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("VerifyCheckpoint(decisions MetadataBytes=%d) = %v, want columnar.ErrBatchBudget", decisionsTight.MetadataBytes, err)
	}

	// Case 2: Enough MetadataBytes for baseMeta + decisionsBytes + a few commits'
	// layouts, but NOT enough for all 30 commits' cumulative layoutsBytes.
	perCommitLayout := uint64(unsafe.Sizeof(storage.ObjectLayout{})) + uint64(len(scanFixtureSpec().Layout))*uint64(unsafe.Sizeof(storage.ColumnLayout{}))
	allLayoutsBytes := uint64(commits) * perCommitLayout
	layoutsTight := DefaultLimits()
	layoutsTight.MetadataBytes = baseMeta + decisionsBytes + allLayoutsBytes/2
	err = (CheckpointVerifier{FenceVerifier: fenceVerifier, Limits: layoutsTight}).VerifyCheckpoint(t.Context(), root, manifest)
	if !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("VerifyCheckpoint(layouts MetadataBytes=%d) = %v, want columnar.ErrBatchBudget", layoutsTight.MetadataBytes, err)
	}
}

func assertBlockCacheInvariants(t testing.TB, c *paxBlockCache) {
	t.Helper()
	var totalUsed, totalBudget uint64
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		if s.used > s.max {
			used, maxB := s.used, s.max
			s.mu.Unlock()
			t.Fatalf("shard %d used (%d) > max (%d)", i, used, maxB)
		}
		var sum uint64
		count := 0
		for n := s.head; n != nil; n = n.next {
			sum += n.bytes
			count++
			if _, ok := s.entries[n.key]; !ok {
				s.mu.Unlock()
				t.Fatalf("shard %d linked list node %+v missing from entries map", i, n.key)
			}
		}
		if count != len(s.entries) {
			entriesLen := len(s.entries)
			s.mu.Unlock()
			t.Fatalf("shard %d list len (%d) != entries map len (%d)", i, count, entriesLen)
		}
		if sum != s.used {
			used := s.used
			s.mu.Unlock()
			t.Fatalf("shard %d sum(node.bytes)=%d != s.used=%d", i, sum, used)
		}
		totalUsed += s.used
		totalBudget += s.max
		s.mu.Unlock()
	}
	if totalUsed > totalBudget {
		t.Fatalf("totalUsed (%d) > totalBudget (%d)", totalUsed, totalBudget)
	}
}

func TestPhase7CacheVerifyBlockCacheDecodedAccountingAndEviction(t *testing.T) {
	const budget = uint64(64 << 10) // 64 KiB total -> 4 KiB per shard across 16 shards
	cache := newPAXBlockCache(budget)
	if cache.BudgetBytes() != budget {
		t.Fatalf("BudgetBytes() = %d, want %d", cache.BudgetBytes(), budget)
	}

	makeBlock := func(rawLen int) *cachedPAXBlock {
		data := make([]byte, rawLen)
		return &cachedPAXBlock{
			data:      data,
			rows:      128,
			columns:   4,
			sizeBytes: uint64(len(data)) + uint64(unsafe.Sizeof(cachedPAXBlock{})),
		}
	}

	// 1. Find 3 distinct keys that map to the SAME shard to deterministically verify LRU eviction.
	var sameShardKeys []blockCacheKey
	targetShard := cache.shard(blockCacheKey{fileID: 1, blockIndex: 0})
	for idx := 0; len(sameShardKeys) < 3; idx++ {
		k := blockCacheKey{fileID: 1, blockIndex: idx}
		if cache.shard(k) == targetShard {
			sameShardKeys = append(sameShardKeys, k)
		}
	}

	// Each block raw cost ~ 512 + sizeof(cachedPAXBlock) + sizeof(blockCacheNode) ~ 720 bytes.
	// 3 blocks fit comfortably in a 4096-byte shard (~2160 bytes).
	blkA := cache.put(sameShardKeys[0], makeBlock(512))
	blkB := cache.put(sameShardKeys[1], makeBlock(512))
	blkC := cache.put(sameShardKeys[2], makeBlock(512))
	if blkA == nil || blkB == nil || blkC == nil {
		t.Fatal("expected small blocks to be cached")
	}
	assertBlockCacheInvariants(t, cache)
	usedBeforeCol := cache.UsedBytes()

	// 2. Attach a decoded Int64 vector (~2400 bytes) to blkC:
	//    blkC + col (~3120) <= 4096, but blkA + blkB + blkC + col (~4560) > 4096,
	//    so LRU victim blkA must be evicted while blkC stays cached with the decoded column!
	medVec := columnar.Vector{
		Type:     model.Type{ID: 20, Storage: model.TypeInt64},
		Encoding: columnar.Flat,
		Length:   260,
		Int64:    make([]int64, 260), // 2080 bytes + Vector overhead ~ 2360 bytes
	}
	if !cache.putColumn(sameShardKeys[2], blkC, 1, medVec) {
		t.Fatal("expected medium decoded column to be cached via LRU eviction")
	}
	assertBlockCacheInvariants(t, cache)
	if cache.get(sameShardKeys[0]) != nil {
		t.Fatal("expected LRU victim blkA to be evicted when decoded column pushed shard over budget")
	}
	if cache.get(sameShardKeys[2]) != blkC {
		t.Fatal("expected blkC to remain cached")
	}
	if _, ok := blkC.getColumn(1); !ok {
		t.Fatal("expected column 1 to be cached on blkC")
	}
	if cache.UsedBytes() == usedBeforeCol {
		t.Fatal("expected UsedBytes to reflect decoded column accounting")
	}

	// 3. Re-inserting the SAME column ID must be a no-op without double-charging!
	usedAfterFirstPut := cache.UsedBytes()
	if cache.putColumn(sameShardKeys[2], blkC, 1, medVec) {
		t.Fatal("expected duplicate putColumn for already-cached column ID to return false")
	}
	if cache.UsedBytes() != usedAfterFirstPut {
		t.Fatalf("duplicate putColumn changed UsedBytes from %d to %d", usedAfterFirstPut, cache.UsedBytes())
	}
	assertBlockCacheInvariants(t, cache)

	// 4. Oversized single decoded vector (> shard budget 4096) must be refused
	//    WITHOUT blowing budget and while keeping blkC cached!
	hugeVec := columnar.Vector{
		Type:     model.Type{ID: 25, Storage: model.TypeString},
		Encoding: columnar.Flat,
		Length:   1,
		Data:     make([]byte, 8192),
		Ranges:   []columnar.Range{{Offset: 0, Length: 8192}},
	}
	if cache.putColumn(sameShardKeys[2], blkC, 2, hugeVec) {
		t.Fatal("expected oversized decoded vector (> shard budget) to be rejected")
	}
	if _, ok := blkC.getColumn(2); ok {
		t.Fatal("oversized decoded vector must not be stored on cachedPAXBlock")
	}
	if cache.get(sameShardKeys[2]) != blkC {
		t.Fatal("blkC raw bytes must stay cached after refusing oversized decoded vector")
	}
	assertBlockCacheInvariants(t, cache)

	// 5. Vector smaller than shard budget alone, but node.bytes + colBytes > shard budget,
	//    must also be refused without blowing shard budget or evicting blkC!
	overflowNodeVec := columnar.Vector{
		Type:     model.Type{ID: 20, Storage: model.TypeInt64},
		Encoding: columnar.Flat,
		Length:   200,
		Int64:    make([]int64, 200), // 1600 bytes; blkC already ~3100 bytes -> > 4096
	}
	if cache.putColumn(sameShardKeys[2], blkC, 3, overflowNodeVec) {
		t.Fatal("expected decoded vector causing node.bytes + colBytes > shard.max to be rejected")
	}
	if _, ok := blkC.getColumn(3); ok {
		t.Fatal("node-overflowing decoded vector must not be stored on cachedPAXBlock")
	}
	assertBlockCacheInvariants(t, cache)

	// 6. Calling putColumn on an evicted block (blkA) must return false and not charge shard.
	usedBeforeEvicted := cache.UsedBytes()
	smallVec := columnar.Vector{
		Type:     model.Type{ID: 20, Storage: model.TypeInt64},
		Encoding: columnar.Flat,
		Length:   10,
		Int64:    make([]int64, 10),
	}
	if cache.putColumn(sameShardKeys[0], blkA, 1, smallVec) {
		t.Fatal("expected putColumn on evicted block to return false")
	}
	if cache.UsedBytes() != usedBeforeEvicted {
		t.Fatalf("putColumn on evicted block changed UsedBytes from %d to %d", usedBeforeEvicted, cache.UsedBytes())
	}

	// 7. Nested vector byte estimation includes children, nulls, indices, ranges, intervals, float32, shape ranges, dimensions.
	nested := columnar.Vector{
		Type:        model.Type{ID: 1016, Storage: model.TypeArray},
		Encoding:    columnar.Dictionary,
		Length:      8,
		Nulls:       make([]byte, 1),
		Indices:     make([]uint32, 8),
		Ranges:      make([]columnar.Range, 8),
		ShapeRanges: make([]columnar.Range, 8),
		Dimensions:  make([]model.ArrayDimension, 8),
		Children: []columnar.Vector{{
			Type:      model.Type{ID: 1186, Storage: model.TypeInterval},
			Encoding:  columnar.Flat,
			Length:    16,
			Intervals: make([]model.Interval, 16),
			Float32:   make([]float32, 4),
			Float64:   make([]float64, 4),
		}},
	}
	if got := estimateCachedVectorBytes(&nested); got <= 2*uint64(unsafe.Sizeof(columnar.Vector{}))+16*uint64(unsafe.Sizeof(model.Interval{})) {
		t.Fatalf("estimateCachedVectorBytes(nested) = %d, expected full recursive size", got)
	}

	// 8. Cache many blocks across all shards with expanded columns: totalUsed() <= totalBudget always holds.
	for i := range 250 {
		key := blockCacheKey{fileID: uint64(i/16 + 10), blockIndex: i % 16}
		blk := cache.put(key, makeBlock(256))
		if blk != nil {
			for col := model.ColumnID(1); col <= 3; col++ {
				v := columnar.Vector{
					Type:     model.Type{ID: 20, Storage: model.TypeInt64},
					Encoding: columnar.Flat,
					Length:   160,
					Int64:    make([]int64, 160),
				}
				cache.putColumn(key, blk, col, v)
			}
		}
		if cache.UsedBytes() > cache.BudgetBytes() {
			t.Fatalf("step %d: UsedBytes (%d) > BudgetBytes (%d)", i, cache.UsedBytes(), cache.BudgetBytes())
		}
	}
	assertBlockCacheInvariants(t, cache)
}

func TestPhase7CacheVerifyBlockCacheConcurrentEndToEndRace(t *testing.T) {
	// Write real SSTs with compressible RLE/Delta columns so raw encoded blocks
	// are tiny while decoded Int64 vectors expand significantly, then hammer
	// concurrent readers against a tight 64 KiB paxBlockCache under -race.
	limits := DefaultLimits()
	limits.ResidentBytes = 64 << 10 // runSet sets blockCache budget = min(ResidentBytes, 32 MiB) = 64 KiB
	limits.BlockRows = 256
	fixture := openBackendFixtureWithLimits(t, limits)
	target := encTestTarget()

	const (
		batches       = 6
		rowsPerBatch  = 256
	)
	var lastRes storage.ApplyResult
	for b := 1; b <= batches; b++ {
		keys := make([]int64, rowsPerBatch)
		vals := make([]int64, rowsPerBatch)
		for r := range rowsPerBatch {
			k := int64((b-1)*rowsPerBatch + r + 1)
			keys[r] = k
			// Constant value per batch encourages RLE compression in PAX blocks,
			// maximizing decoded-vs-encoded expansion ratio.
			vals[r] = int64(b * 1000)
		}
		res, err := fixture.backend.ApplyMutations(t.Context(), makeIntRowsBatch(t, fixture, uint64(b), model.Timestamp(b*10), target, keys, vals))
		if err != nil {
			t.Fatalf("ApplyMutations(%d): %v", b, err)
		}
		lastRes = res
	}
	// Force flush of memtable to SST via Checkpoint so reads go through sstFile + paxBlockCache.
	fence, err := fixture.authority.Issue(t.Context(), lastRes.CommitTS, lastRes.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{
		Snapshot: storage.SnapshotSpec{Timestamp: lastRes.CommitTS, Fence: fence},
	})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}

	cache := fixture.backend.runs.blockCache
	if cache == nil {
		t.Fatal("expected non-nil blockCache")
	}
	if cache.BudgetBytes() > 64<<10 {
		t.Fatalf("expected blockCache budget <= 64 KiB, got %d", cache.BudgetBytes())
	}

	const workers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			reader, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: lastRes.CommitTS, Fence: fence})
			if err != nil {
				errCh <- err
				return
			}
			defer func() { _ = reader.Close() }()
			for iter := range 40 {
				keyID := int64((worker*37+iter*29)%(batches*rowsPerBatch) + 1)
				row, err := reader.Get(t.Context(), storage.ReadRequest{
					Target:    target,
					Key:       backendKey(keyID),
					ColumnIDs: []model.ColumnID{1},
					Layout:    scanFixtureSpec().Layout,
				})
				if err != nil {
					errCh <- err
					return
				}
				wantBatch := (keyID-1)/rowsPerBatch + 1
				if len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].IntVal != wantBatch*1000 {
					errCh <- errors.New("unexpected row value from cached block")
					return
				}
				if used, maxB := cache.UsedBytes(), cache.BudgetBytes(); used > maxB {
					errCh <- errors.New("block cache exceeded budget during concurrent reads")
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	assertBlockCacheInvariants(t, cache)

	// Dedicated concurrent stress test on paxBlockCache (put / get / putColumn / getColumn / UsedBytes).
	stressCache := newPAXBlockCache(64 << 10)
	var swg sync.WaitGroup
	for g := range 12 {
		swg.Add(1)
		go func(gid int) {
			defer swg.Done()
			for i := range 200 {
				var keyBuf [8]byte
				binary.LittleEndian.PutUint64(keyBuf[:], uint64(i%32))
				key := blockCacheKey{fileID: uint64(i%4 + 1), blockIndex: i % 16}
				blk := stressCache.get(key)
				if blk == nil {
					blk = stressCache.put(key, &cachedPAXBlock{
						data:      append([]byte(nil), keyBuf[:]...),
						rows:      64,
						columns:   4,
						sizeBytes: 256 + uint64(unsafe.Sizeof(cachedPAXBlock{})),
					})
				}
				if blk != nil {
					colID := model.ColumnID((gid+i)%4 + 1)
					if _, ok := blk.getColumn(colID); !ok {
						// Mix fitting and oversized vectors under concurrency.
						n := 80
						if (gid+i)%7 == 0 {
							n = 1024 // oversized (> 4 KiB shard max)
						}
						v := columnar.Vector{
							Type:     model.Type{ID: 20, Storage: model.TypeInt64},
							Encoding: columnar.Flat,
							Length:   n,
							Int64:    make([]int64, n),
						}
						stressCache.putColumn(key, blk, colID, v)
					}
				}
				if stressCache.UsedBytes() > stressCache.BudgetBytes() {
					t.Errorf("stressCache UsedBytes > BudgetBytes")
					return
				}
			}
		}(g)
	}
	swg.Wait()
	assertBlockCacheInvariants(t, stressCache)
}
