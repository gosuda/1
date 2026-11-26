package neosepia

import (
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func buildTestMemBlocks(t *testing.T, maxRows int, entries []*memEntry) []*memPAXBlock {
	t.Helper()
	target := encTestTarget()
	layout := storage.ObjectLayout{
		Target: target,
		Columns: []storage.ColumnLayout{{
			ID:   1,
			Type: model.Type{ID: 20, Storage: model.TypeInt64},
		}},
	}
	state := &memTargetState{
		layouts: map[uint64]storage.ObjectLayout{
			target.DescriptorVersion: layout,
		},
	}
	for _, e := range entries {
		e.target = target
		if e.op == storage.MutationUpsert {
			e.columnIDs = []model.ColumnID{1}
		}
		vk := memVersionKey{key: e.key, commitTS: e.commitTS, ordinal: e.ordinal}
		nextTree, _, err := state.entries.Set(vk, e, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		state.entries = nextTree
		if e.op == storage.MutationDeleteRange {
			state.ranges = append(state.ranges, e)
		}
		state.rows++
	}
	limits := encTestLimits()
	limits.MaxRows = maxRows
	blocks, err := buildMemTargetBlocks(t.Context(), state, 0, limits)
	if err != nil {
		t.Fatal(err)
	}
	return blocks
}

func testMemUpsert(key string, ts model.Timestamp, ord uint64, val int64) *memEntry {
	return &memEntry{
		key:      []byte(key),
		commitTS: ts,
		ordinal:  ord,
		op:       storage.MutationUpsert,
		values:   []model.Value{model.Int64Val(val)},
	}
}

func testMemDelete(key string, ts model.Timestamp, ord uint64) *memEntry {
	return &memEntry{
		key:      []byte(key),
		commitTS: ts,
		ordinal:  ord,
		op:       storage.MutationDelete,
	}
}

func testMemDeleteRange(start, end string, ts model.Timestamp, ord uint64) *memEntry {
	return &memEntry{
		key:      []byte(start),
		endKey:   []byte(end),
		commitTS: ts,
		ordinal:  ord,
		op:       storage.MutationDeleteRange,
	}
}

func scanMemMVCC(t *testing.T, blocks []*memPAXBlock, snapshotTS model.Timestamp, dir storage.ScanDirection, mem *sourceMemory) []string {
	t.Helper()
	src := newMemVersionCursor(blocks, dir, 1<<20, mem)
	cursor, err := newMVCCCursor(t.Context(), []*runVersionCursor{src}, snapshotTS, mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cursor.Close(); err != nil {
			t.Errorf("close mvcc cursor: %v", err)
		}
	}()
	var out []string
	for {
		row, err := cursor.Next(t.Context())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		vec, err := row.lease.block.Column(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s=%d", row.key, vec.Int64[row.row]))
	}
	return out
}

func formatVersionRow(t *testing.T, row versionRow) string {
	t.Helper()
	switch row.kind {
	case storage.MutationUpsert:
		vec, err := row.lease.block.Column(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("U:%s@%d:%d=%d", row.key, row.timestamp, row.ordinal, vec.Int64[row.row])
	case storage.MutationDelete:
		return fmt.Sprintf("D:%s@%d:%d", row.key, row.timestamp, row.ordinal)
	case storage.MutationDeleteRange:
		return fmt.Sprintf("R:%s..%s@%d:%d", row.key, row.end, row.timestamp, row.ordinal)
	default:
		t.Fatalf("unexpected mutation op %v", row.kind)
		return ""
	}
}

func TestMemReverseScanMultiBlockSplitSameKey(t *testing.T) {
	// With MaxRows: 2, sorted entries pack as:
	//   block 0: [k1@10(U=101), k2@60(D)]
	//   block 1: [k2@50(U=205), k2@40(D)]
	//   block 2: [k2@30:2(U=203), k2@30:1(U=202)]
	//   block 3: [k3@35(U=303), k3@15(U=301)]
	//   block 4: [k4@25(U=401)]
	// Key "k2" is split across 3 memPAXBlocks (blocks 0, 1, 2).
	blocks := buildTestMemBlocks(t, 2, []*memEntry{
		testMemUpsert("k1", 10, 1, 101),
		testMemDelete("k2", 60, 1),
		testMemUpsert("k2", 50, 1, 205),
		testMemDelete("k2", 40, 1),
		testMemUpsert("k2", 30, 2, 203),
		testMemUpsert("k2", 30, 1, 202),
		testMemUpsert("k3", 35, 1, 303),
		testMemUpsert("k3", 15, 1, 301),
		testMemUpsert("k4", 25, 1, 401),
	})

	if len(blocks) != 5 {
		t.Fatalf("got %d blocks, want 5", len(blocks))
	}
	k2Blocks := 0
	for _, mb := range blocks {
		for r := 0; r < mb.rows; r++ {
			if string(mb.keyAt(r)) == "k2" {
				k2Blocks++
				break
			}
		}
	}
	if k2Blocks != 3 {
		t.Fatalf("k2 spans %d blocks, want 3", k2Blocks)
	}

	// 1. Direct newMemVersionCursor reverse order check (strictly key DESC, commitTS DESC, ordinal DESC).
	t.Run("DirectVersionCursor", func(t *testing.T) {
		mem := &sourceMemory{limit: 1 << 20}
		cur := newMemVersionCursor(blocks, storage.ScanReverse, 1<<20, mem)
		defer func() {
			if err := cur.Close(); err != nil {
				t.Errorf("close cursor: %v", err)
			}
		}()

		var got []string
		for {
			row, err := cur.Next(t.Context())
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, formatVersionRow(t, row))
		}
		want := []string{
			"U:k4@25:1=401",
			"U:k3@35:1=303",
			"U:k3@15:1=301",
			"D:k2@60:1",
			"U:k2@50:1=205",
			"D:k2@40:1",
			"U:k2@30:2=203",
			"U:k2@30:1=202",
			"U:k1@10:1=101",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("direct reverse cursor:\ngot:  %v\nwant: %v", got, want)
		}
		if err := cur.Close(); err != nil {
			t.Fatal(err)
		}
		if mem.used != 0 {
			t.Fatalf("source memory leak: used=%d, want 0", mem.used)
		}
	})

	// 2. MVCC reverse parity with forward scan across snapshots, exercising:
	//    - latest upsert overriding older tombstone (ts=50: U@50 overrides D@40)
	//    - latest tombstone hiding older upsert across blocks (ts=40: D@40 hides U@30; ts=60: D@60 in block 0 hides U@50 in block 1 & U@30 in block 2)
	//    - ordinal tie-breaking (ts=30: U@30:2 beats U@30:1)
	snapshots := []struct {
		ts   model.Timestamp
		want []string
	}{
		{15, []string{"k3=301", "k1=101"}},
		{30, []string{"k4=401", "k3=301", "k2=203", "k1=101"}},
		{40, []string{"k4=401", "k3=303", "k1=101"}},
		{50, []string{"k4=401", "k3=303", "k2=205", "k1=101"}},
		{60, []string{"k4=401", "k3=303", "k1=101"}},
	}
	for _, tc := range snapshots {
		t.Run(fmt.Sprintf("MVCC_ts_%d", tc.ts), func(t *testing.T) {
			fwdMem := &sourceMemory{limit: 1 << 20}
			revMem := &sourceMemory{limit: 1 << 20}
			fwd := scanMemMVCC(t, blocks, tc.ts, storage.ScanForward, fwdMem)
			rev := scanMemMVCC(t, blocks, tc.ts, storage.ScanReverse, revMem)

			wantFromFwd := slices.Clone(fwd)
			slices.Reverse(wantFromFwd)
			if !slices.Equal(rev, wantFromFwd) {
				t.Fatalf("snapshot %d reverse/forward mismatch: rev=%v, reversed(fwd)=%v", tc.ts, rev, wantFromFwd)
			}
			if !slices.Equal(rev, tc.want) {
				t.Fatalf("snapshot %d: got %v, want %v", tc.ts, rev, tc.want)
			}
			if fwdMem.used != 0 || revMem.used != 0 {
				t.Fatalf("source memory leak: fwd=%d rev=%d", fwdMem.used, revMem.used)
			}
		})
	}
}

func TestMemReverseScanMultiBlockWithDeleteRangeAndSnapshotPruning(t *testing.T) {
	// With MaxRows: 2, sorted entries pack as:
	//   block 0: [k1@10(U=10), k1@5(U=5)]                 minCommitTS=5
	//   block 1: [DR(k1a..k1z)@30, k1m@20(U=15)]          minCommitTS=20
	//   block 2: [k2@100(U=2100), k2@90(U=2090)]          minCommitTS=90  (leading k2 block, pruned at ts=50)
	//   block 3: [DR(k2..k3)@85, k2@70(D)]                minCommitTS=70  (middle k2 block, pruned at ts=50)
	//   block 4: [DR(k2..k5)@35, k2@20(U=2020)]           minCommitTS=20  (trailing k2 block)
	//   block 5: [k3@10(U=3010), k4@100(U=4100)]          minCommitTS=10  (leading k4 block)
	//   block 6: [k4@90(U=4090), k4@80(D)]                minCommitTS=80  (middle k4 block, pruned at ts=50)
	//   block 7: [k4@40(U=4040), k4@20(U=4020)]           minCommitTS=20  (trailing k4 block)
	//   block 8: [k5@10(U=5010), k6@95(U=6095)]           minCommitTS=10
	//   block 9: [k7@95(U=7095)]                          minCommitTS=95  (trailing block, pruned at ts=50)
	blocks := buildTestMemBlocks(t, 2, []*memEntry{
		testMemUpsert("k1", 10, 1, 10),
		testMemUpsert("k1", 5, 1, 5),
		testMemDeleteRange("k1a", "k1z", 30, 1),
		testMemUpsert("k1m", 20, 1, 15),
		testMemUpsert("k2", 100, 1, 2100),
		testMemUpsert("k2", 90, 1, 2090),
		testMemDeleteRange("k2", "k3", 85, 1),
		testMemDelete("k2", 70, 1),
		testMemDeleteRange("k2", "k5", 35, 1),
		testMemUpsert("k2", 20, 1, 2020),
		testMemUpsert("k3", 10, 1, 3010),
		testMemUpsert("k4", 100, 1, 4100),
		testMemUpsert("k4", 90, 1, 4090),
		testMemDelete("k4", 80, 1),
		testMemUpsert("k4", 40, 1, 4040),
		testMemUpsert("k4", 20, 1, 4020),
		testMemUpsert("k5", 10, 1, 5010),
		testMemUpsert("k6", 95, 1, 6095),
		testMemUpsert("k7", 95, 1, 7095),
	})

	if len(blocks) != 10 {
		t.Fatalf("got %d blocks, want 10", len(blocks))
	}
	// Verify snapshot pruning preconditions at ts=50.
	for _, idx := range []int{2, 3, 6, 9} {
		if blocks[idx].minCommitTS <= 50 {
			t.Fatalf("block %d minCommitTS=%d, want > 50", idx, blocks[idx].minCommitTS)
		}
	}

	// 1. Direct reverse cursor with snapshotTS=50 skips pruned blocks (2, 3, 6, 9)
	//    and skips MutationDeleteRange rows inside point groups while emitting
	//    range tombstones in end DESC order.
	t.Run("DirectVersionCursorWithSnapshotPruning", func(t *testing.T) {
		mem := &sourceMemory{limit: 1 << 20}
		cur := newMemVersionCursor(blocks, storage.ScanReverse, 1<<20, mem)
		cur.snapshotTS = 50
		defer func() {
			if err := cur.Close(); err != nil {
				t.Errorf("close cursor: %v", err)
			}
		}()

		var got []string
		for {
			row, err := cur.Next(t.Context())
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, formatVersionRow(t, row))
		}
		// Blocks 2 (k2@100,90), 3 (DR k2..k3, k2@70), 6 (k4@90,80), 9 (k7@95) are pruned.
		// Note: k6@95 (block 8, minTS=10) and k4@100 (block 5, minTS=10) remain in unpruned blocks
		// and are emitted by the raw cursor (to be filtered by mvccCursor), matching forward nextMem.
		want := []string{
			"U:k6@95:1=6095",
			"U:k5@10:1=5010",
			"R:k2..k5@35:1",
			"U:k4@100:1=4100",
			"U:k4@40:1=4040",
			"U:k4@20:1=4020",
			"U:k3@10:1=3010",
			"R:k2..k3@85:1",
			"U:k2@20:1=2020",
			"R:k1a..k1z@30:1",
			"U:k1m@20:1=15",
			"U:k1@10:1=10",
			"U:k1@5:1=5",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("direct pruned reverse cursor:\ngot:  %v\nwant: %v", got, want)
		}
		if err := cur.Close(); err != nil {
			t.Fatal(err)
		}
		if mem.used != 0 {
			t.Fatalf("source memory leak: used=%d, want 0", mem.used)
		}
	})

	// 2. MVCC reverse parity with forward scan across snapshots.
	snapshots := []struct {
		ts   model.Timestamp
		want []string
	}{
		{15, []string{"k5=5010", "k3=3010", "k1=10"}},
		{25, []string{"k5=5010", "k4=4020", "k3=3010", "k2=2020", "k1m=15", "k1=10"}},
		{32, []string{"k5=5010", "k4=4020", "k3=3010", "k2=2020", "k1=10"}},
		{37, []string{"k5=5010", "k1=10"}},
		{50, []string{"k5=5010", "k4=4040", "k1=10"}},
		{75, []string{"k5=5010", "k4=4040", "k1=10"}},
		{85, []string{"k5=5010", "k1=10"}},
		{90, []string{"k5=5010", "k4=4090", "k2=2090", "k1=10"}},
		{110, []string{"k7=7095", "k6=6095", "k5=5010", "k4=4100", "k2=2100", "k1=10"}},
	}
	for _, tc := range snapshots {
		t.Run(fmt.Sprintf("MVCC_ts_%d", tc.ts), func(t *testing.T) {
			fwdMem := &sourceMemory{limit: 1 << 20}
			revMem := &sourceMemory{limit: 1 << 20}
			fwd := scanMemMVCC(t, blocks, tc.ts, storage.ScanForward, fwdMem)
			rev := scanMemMVCC(t, blocks, tc.ts, storage.ScanReverse, revMem)

			wantFromFwd := slices.Clone(fwd)
			slices.Reverse(wantFromFwd)
			if !slices.Equal(rev, wantFromFwd) {
				t.Fatalf("snapshot %d reverse/forward mismatch: rev=%v, reversed(fwd)=%v", tc.ts, rev, wantFromFwd)
			}
			if !slices.Equal(rev, tc.want) {
				t.Fatalf("snapshot %d: got %v, want %v", tc.ts, rev, tc.want)
			}
			if fwdMem.used != 0 || revMem.used != 0 {
				t.Fatalf("source memory leak: fwd=%d rev=%d", fwdMem.used, revMem.used)
			}
		})
	}
}
