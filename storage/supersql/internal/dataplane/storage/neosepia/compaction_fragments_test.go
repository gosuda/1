package neosepia

import (
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSparseL0CompactionPreservesResidualFragmentsOnRecovery(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()
	apply := func(id byte, keys, values []int64) {
		batch := storage.MutationBatch{TxID: model.TxnID{id}, CommitTS: model.Timestamp(id)*10, Epoch: fixture.epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: target, Columns: scanFixtureSpec().Layout}}}
		for i, key := range keys {
			mutation := storage.NewUpsertMutation(target, backendKey(key), []model.ColumnID{1}, []model.Value{model.Int64Val(values[i])})
			mutation.Ordinal = uint64(i+1)
			batch.Mutations = append(batch.Mutations, mutation)
		}
		var err error
		batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
		if err != nil { t.Fatal(err) }
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil { t.Fatal(err) }
	}
	compact := func() {
		task, err := fixture.backend.ScheduleCompaction(t.Context(), storage.CompactionRequest{Target: target})
		if err != nil { t.Fatal(err) }
		if err := task.Wait(t.Context()); err != nil { t.Fatal(err) }
	}
	for i, key := range []int64{1, 50, 100} {
		apply(byte(i+1), []int64{key}, []int64{key})
		compact()
	}
	apply(4, []int64{1, 100}, []int64{111, 333})
	pinned := fixture.snapshot(t, 30)
	compact()
	if err := fixture.backend.Close(); err != nil { t.Fatal(err) }
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil { t.Fatal(err) }
	fixture.backend = reopened
	defer func() { if err := reopened.Close(); err != nil { t.Error(err) } }()
	for _, tc := range []struct { timestamp model.Timestamp; want []int64 }{{30, []int64{1, 50, 100}}, {40, []int64{111, 50, 333}}} {
		reader := fixture.snapshot(t, tc.timestamp)
		for _, direction := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
			request := storage.PushdownRequest{Target: target, Layout: scanFixtureSpec().Layout, ColumnIDs: []model.ColumnID{1}, Direction: direction, KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: scanFixtureSpec().Layout[0].Type}}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}
			plan, err := reader.PlanScan(t.Context(), request)
			if err != nil { t.Fatal(err) }
			cursor, err := reader.VectorizedScan(t.Context(), plan)
			if err != nil { t.Fatal(err) }
			var got []int64
			out := columnar.Batch{MaxRows: 1}
			for {
				err := cursor.Next(t.Context(), &out)
				if err == io.EOF { break }
				if err != nil { t.Fatal(err) }
				for i := range out.Rows() {
					value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(i), 1024)
					if err != nil { t.Fatal(err) }
					got = append(got, value.IntVal)
				}
			}
			if err := cursor.Close(); err != nil { t.Fatal(err) }
			if err := reader.ReleasePlan(plan); err != nil { t.Fatal(err) }
			want := slices.Clone(tc.want)
			if direction == storage.ScanReverse { slices.Reverse(want) }
			if !slices.Equal(got, want) { t.Fatalf("snapshot %d direction %d: got %v, want %v", tc.timestamp, direction, got, want) }
		}
		if err := reader.Close(); err != nil { t.Fatal(err) }
	}
	if got := backendBalance(t, pinned, 0); got != 1 { t.Fatalf("retired input pin changed: %d", got) }
	if err := pinned.Close(); err != nil { t.Fatal(err) }
	fence, err := fixture.authority.Issue(t.Context(), 40, 4, fixture.epoch)
	if err != nil { t.Fatal(err) }
	checkpoint, err := reopened.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 40, Fence: fence}})
	if err != nil { t.Fatal(err) }
	checkpointDirectory := t.TempDir()
	copyCheckpointFiles(t, checkpoint, checkpointDirectory)
	root, err := os.OpenRoot(checkpointDirectory)
	if err != nil { t.Fatal(err) }
	verifier := CheckpointVerifier{FenceVerifier: fixture.authority, Limits: DefaultLimits()}
	verifyErr := verifier.VerifyCheckpoint(t.Context(), root, checkpointManifest(t, checkpoint))
	rootErr, checkpointErr := root.Close(), checkpoint.Close()
	if err := errors.Join(verifyErr, rootErr, checkpointErr); err != nil {
		t.Fatalf("checkpoint verification: %v; root close: %v; checkpoint close: %v", verifyErr, rootErr, checkpointErr)
	}
	compact()
	latest := fixture.snapshot(t, 40)
	if got := backendBalance(t, latest, 0); got != 111 { t.Fatalf("fragment completion changed latest: %d", got) }
	if err := latest.Close(); err != nil { t.Fatal(err) }
	remove := storage.NewDeleteRangeMutation(target, backendKey(1), backendKey(101))
	insert := storage.NewUpsertMutation(target, backendKey(50), []model.ColumnID{1}, []model.Value{model.Int64Val(777)})
	remove.Ordinal, insert.Ordinal = 1, 2
	batch := storage.MutationBatch{TxID: model.TxnID{5}, CommitTS: 50, Epoch: fixture.epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: target, Columns: scanFixtureSpec().Layout}}, Mutations: []storage.Mutation{remove, insert}}
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil { t.Fatal(err) }
	if _, err := reopened.ApplyMutations(t.Context(), batch); err != nil { t.Fatal(err) }
	compact()
	deleted := fixture.snapshot(t, 50)
	for _, key := range []int64{1, 100} {
		_, err := deleted.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(key), Layout: scanFixtureSpec().Layout, ColumnIDs: []model.ColumnID{1}})
		if !errors.Is(err, storage.ErrRowNotFound) { t.Fatalf("range tombstone lost across fragment at key %d: %v", key, err) }
	}
	row, err := deleted.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(50), Layout: scanFixtureSpec().Layout, ColumnIDs: []model.ColumnID{1}})
	if err != nil || row.Values[0].IntVal != 777 { t.Fatalf("same-commit insert after range delete: row=%+v err=%v", row, err) }
}

// TestCompactionExcludesRewrittenDestinationFromLaterIntervals pins the level
// invariant after many intervals reuse one wide level-1 destination. A pass
// that already rewrote that destination must not select it again; otherwise
// each later pass re-links its fragments and the same level publishes
// overlapping files, which fails publication with ErrCorruptStorage.
func TestCompactionExcludesRewrittenDestinationFromLaterIntervals(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()
	flush := 0
	apply := func(keys []int64) {
		flush++
		batch := storage.MutationBatch{TxID: model.TxnID{byte(flush), byte(flush >> 8)}, CommitTS: model.Timestamp(flush) * 10, Epoch: fixture.epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: target, Columns: scanFixtureSpec().Layout}}}
		for i, key := range keys {
			mutation := storage.NewUpsertMutation(target, backendKey(key), []model.ColumnID{1}, []model.Value{model.Int64Val(key)})
			mutation.Ordinal = uint64(i + 1)
			batch.Mutations = append(batch.Mutations, mutation)
		}
		var err error
		batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	compact := func() {
		task, err := fixture.backend.ScheduleCompaction(t.Context(), storage.CompactionRequest{Target: target})
		if err != nil {
			t.Fatal(err)
		}
		if err := task.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var wide []int64
	for key := int64(1); key <= 400; key++ {
		wide = append(wide, key*10)
	}
	apply(wide)
	compact()
	for key := int64(1); key <= 150; key++ {
		apply([]int64{key*10 + 5})
	}
	compact()
	if err := fixture.backend.Close(); err != nil {
		t.Fatalf("close after interval compaction: %v", err)
	}
}
