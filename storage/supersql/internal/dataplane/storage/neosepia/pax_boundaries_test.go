package neosepia

import (
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestPAXEmptyArrayRemainsDistinctFromNull(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 23, Storage: model.TypeInt64}}}}
	batch, closeBuilder := encTestBuildBatch(t, []model.TypeLayout{layout}, [][]model.Value{{{Type: model.TypeArray}}, {model.NullVal(model.TypeArray)}})
	defer closeBuilder()
	versions := blockVersions{Keys: encTestKeyVector("a", "b"), CommitTS: []model.Timestamp{1, 2}, Ordinals: []uint64{1, 1}, Kinds: []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert}}
	data, err := encodeBlock(t.Context(), encTestTarget(), versions, &batch, []model.ColumnID{70000}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	block, err := openBlock(data, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := block.Close(); err != nil {
			t.Error(err)
		}
	})
	v, err := block.Column(t.Context(), 70000)
	if err != nil {
		t.Fatal(err)
	}
	if v.IsNull(0) || !v.IsNull(1) {
		t.Fatal("empty and NULL arrays were conflated")
	}
	if v.Ranges[0].Length != 0 || v.Children[0].Length != 0 {
		t.Fatal("empty array acquired elements")
	}
}

func TestPAXVisibilityOnlyReadFitsWithoutRowObjects(t *testing.T) {
	const count = 512
	keys := make([]string, count)
	versions := blockVersions{CommitTS: make([]model.Timestamp, count), Ordinals: make([]uint64, count), Kinds: make([]storage.MutationOp, count)}
	for i := range count {
		keys[i] = fmt.Sprintf("k%04d", i)
		versions.CommitTS[i] = model.Timestamp(i + 1)
		versions.Ordinals[i] = 1
		versions.Kinds[i] = storage.MutationUpsert
	}
	versions.Keys = encTestKeyVector(keys...)
	batch := columnar.Batch{Length: count}
	limits := encTestLimits()
	limits.MaxDecodedBytes = 256
	data, err := encodeBlock(t.Context(), encTestTarget(), versions, &batch, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	block, err := openBlock(data, limits)
	if err != nil {
		t.Fatalf("metadata read must not materialize per-row objects: %v", err)
	}
	t.Cleanup(func() {
		if err := block.Close(); err != nil {
			t.Error(err)
		}
	})
	key, _, timestamp, ordinal, kind, err := block.Version(511)
	if err != nil {
		t.Fatal(err)
	}
	if string(key) != "k0511" || timestamp != 512 || ordinal != 1 || kind != storage.MutationUpsert {
		t.Fatal("visibility metadata changed")
	}
}
