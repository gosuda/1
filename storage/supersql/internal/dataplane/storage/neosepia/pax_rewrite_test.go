package neosepia

import (
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestRewrittenPAXPreservesRangeHistory(t *testing.T) {
	versions := blockVersions{Keys: encTestKeyVector("a", "b", "c"), EndKeys: encTestKeyVector("", "z", ""), CommitTS: []model.Timestamp{10, 30, 40}, Ordinals: []uint64{1, 7, 2}, Kinds: []storage.MutationOp{storage.MutationUpsert, storage.MutationDeleteRange, storage.MutationUpsert}}
	batch := columnar.Batch{Length: 3, Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 3, Nulls: []byte{2}, Int64: []int64{10, 0, 30}}}}
	encoded, err := encodeBlock(t.Context(), encTestTarget(), versions, &batch, []model.ColumnID{1}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	source, err := openBlock(encoded, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	rewritten, err := rewriteBlock(t.Context(), source, []uint32{1, 2}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	compacted := openRun(t, writeRun(t, rewritten))
	base := openRun(t, writeRun(t, runBlock(t, "c", "", 20, storage.MutationUpsert, 100)))
	if got := visibleRunVersions(t, []*sstFile{base, compacted}, 35); len(got) != 0 {
		t.Fatalf("rewritten range resurrected history: %v", got)
	}
	if got := visibleRunVersions(t, []*sstFile{base, compacted}, 40); !slices.Equal(got, []visibleVersion{{key: "c", value: 30}}) {
		t.Fatalf("rewritten future version: %v", got)
	}
	block, err := openBlock(rewritten, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := block.Close(); err != nil {
			t.Error(err)
		}
	})
	key, end, timestamp, ordinal, kind, err := block.Version(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(key) != "b" || string(end) != "z" || timestamp != 30 || ordinal != 7 || kind != storage.MutationDeleteRange {
		t.Fatalf("rewritten tombstone: key=%q end=%q timestamp=%d ordinal=%d kind=%v", key, end, timestamp, ordinal, kind)
	}
}
