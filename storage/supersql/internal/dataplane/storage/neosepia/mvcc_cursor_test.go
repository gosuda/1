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

func TestMVCCCursorDoesNotTreatCorruptionAsEOF(t *testing.T) {
	path := writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 10))
	run := openRun(t, path)
	source, err := newRunVersionCursor(run, 0, 1, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, sstHeaderSize); err != nil {
		t.Fatal(err)
	}
	cursor, err := newMVCCCursor(t.Context(), []*runVersionCursor{source}, 10, mergeLimits{MaxBytes: 4096, MaxKeyBytes: 64}, storage.ScanForward)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	_, err = cursor.Next(t.Context())
	if !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("truncated PAX run became an empty scan: %v", err)
	}
}

type visibleVersion struct {
	key   string
	value int64
}

func visibleRunVersions(t *testing.T, runs []*sstFile, timestamp model.Timestamp) []visibleVersion {
	t.Helper()
	sources := make([]*runVersionCursor, len(runs))
	for i, run := range runs {
		source, err := newRunVersionCursor(run, 0, run.count, 1<<20, nil)
		if err != nil {
			t.Fatal(err)
		}
		sources[i] = source
	}
	cursor, err := newMVCCCursor(t.Context(), sources, timestamp, mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}, storage.ScanForward)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	}()
	var result []visibleVersion
	for {
		row, err := cursor.Next(t.Context())
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		column, err := row.lease.block.Column(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, visibleVersion{key: string(row.key), value: column.Int64[row.row]})
	}
}

func TestMVCCCursorSelectsVersionBeforePayloadFiltering(t *testing.T) {
	runs := []*sstFile{
		openRun(t, writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 10), runBlock(t, "b", "", 10, storage.MutationUpsert, 100), runBlock(t, "c", "", 10, storage.MutationUpsert, 100), runBlock(t, "d", "", 10, storage.MutationUpsert, 100))),
		openRun(t, writeRun(t, runBlock(t, "a", "", 20, storage.MutationUpsert, 20), runBlock(t, "b", "", 30, storage.MutationDelete, 0), runBlock(t, "c", "d", 20, storage.MutationDeleteRange, 0))),
		openRun(t, writeRun(t, runBlock(t, "a", "d", 25, storage.MutationDeleteRange, 0), runBlock(t, "b", "", 26, storage.MutationUpsert, 200), runBlock(t, "c", "", 24, storage.MutationUpsert, 200))),
	}
	cases := []struct {
		timestamp model.Timestamp
		expected  []visibleVersion
	}{
		{10, []visibleVersion{{"a", 10}, {"b", 100}, {"c", 100}, {"d", 100}}},
		{20, []visibleVersion{{"a", 20}, {"b", 100}, {"d", 100}}},
		{24, []visibleVersion{{"a", 20}, {"b", 100}, {"c", 200}, {"d", 100}}},
		{25, []visibleVersion{{"d", 100}}},
		{26, []visibleVersion{{"b", 200}, {"d", 100}}},
		{30, []visibleVersion{{"d", 100}}},
	}
	for _, test := range cases {
		actual := visibleRunVersions(t, runs, test.timestamp)
		if !slices.Equal(actual, test.expected) {
			t.Fatalf("snapshot %d: %v, want %v", test.timestamp, actual, test.expected)
		}
	}
}

func TestMVCCCursorPreservesPointRangeStatementOrder(t *testing.T) {
	base := openRun(t, writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 10), runBlock(t, "b", "", 10, storage.MutationUpsert, 100)))
	for _, first := range []storage.MutationOp{storage.MutationUpsert, storage.MutationDeleteRange} {
		kinds := []storage.MutationOp{first, storage.MutationDeleteRange}
		if first == storage.MutationDeleteRange {
			kinds[1] = storage.MutationUpsert
		}
		versions := blockVersions{Keys: encTestKeyVector("a", "a"), CommitTS: []model.Timestamp{20, 20}, Ordinals: []uint64{2, 1}, Kinds: kinds}
		vector := columnar.Vector{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 2, Int64: []int64{20, 20}, Nulls: make([]byte, 1)}
		if first == storage.MutationUpsert {
			versions.EndKeys = encTestKeyVector("", "z")
			vector.Nulls[0] = 2
		} else {
			versions.EndKeys = encTestKeyVector("z", "")
			vector.Nulls[0] = 1
		}
		batch := columnar.Batch{Length: 2, Columns: []columnar.Vector{vector}}
		data, err := encodeBlock(t.Context(), encTestTarget(), versions, &batch, []model.ColumnID{1}, encTestLimits())
		if err != nil {
			t.Fatal(err)
		}
		change := openRun(t, writeRun(t, data))
		actual := visibleRunVersions(t, []*sstFile{base, change}, 20)
		var expected []visibleVersion
		if first == storage.MutationUpsert {
			expected = []visibleVersion{{"a", 20}}
		}
		if !slices.Equal(actual, expected) {
			t.Fatalf("last statement %v: %v, want %v", first, actual, expected)
		}
	}
}
