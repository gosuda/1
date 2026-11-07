package neosepia

import (
	"errors"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func pointSource(t *testing.T, runs []*sstFile, timestamp model.Timestamp) *mvccCursor {
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
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	return cursor
}

func TestPAXPointReadRejectsKeyPrefixNeighbors(t *testing.T) {
	run := openRun(t, writeRun(t, runBlock(t, "ab", "", 10, storage.MutationUpsert, 10)))
	// An unrelated projection must not cause payload decoding for the absent key.
	request := storage.ReadRequest{Target: encTestTarget(), Key: []byte("a"), ColumnIDs: []model.ColumnID{2}, Layout: []storage.ColumnLayout{{ID: 2, Type: model.Type{ID: 20, Storage: model.TypeInt64}}}}
	_, err := readPAXPoint(t.Context(), pointSource(t, []*sstFile{run}, 10), nil, request, pointReadLimits{ScanBytes: 1 << 20, RowBytes: 1024})
	if !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("prefix neighbor became a point result: %v", err)
	}
}

func TestPAXPointReadOwnsHistoricalNestedDefault(t *testing.T) {
	run := multiRowRun(t)
	target := encTestTarget()
	target.DescriptorVersion++
	request := storage.ReadRequest{Target: target, Key: []byte("b"), ColumnIDs: []model.ColumnID{70000}, Layout: []storage.ColumnLayout{{ID: 70000, Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}, HasMissingValue: true, MissingValue: model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -3}}, ArrayVal: []model.Value{model.Int64Val(7)}}}}}
	row, err := readPAXPoint(t.Context(), pointSource(t, []*sstFile{run}, 10), nil, request, pointReadLimits{ScanBytes: 1 << 20, RowBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	request.Key[0] = 'x'
	request.Layout[0].MissingValue.ArrayVal[0].IntVal = 99
	if string(row.Key) != "b" || row.CommitTS != 10 || row.Ordinal != 2 {
		t.Fatalf("point identity changed: %+v", row)
	}
	if row.Values[0].Dimensions[0].LowerBound != -3 || row.Values[0].ArrayVal[0].IntVal != 7 {
		t.Fatal("point result borrowed released default payloads")
	}
}

func TestPAXPointReadHonorsCopyBudget(t *testing.T) {
	run := multiRowRun(t)
	spec := scanFixtureSpec()
	request := storage.ReadRequest{Target: spec.Target, Key: []byte("a"), Layout: spec.Layout, ColumnIDs: spec.Projection}
	_, err := readPAXPoint(t.Context(), pointSource(t, []*sstFile{run}, 10), nil, request, pointReadLimits{ScanBytes: 1 << 20, RowBytes: 1})
	if !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("unbounded point copy: %v", err)
	}
}

func TestPAXPointOwnRangeThenInsert(t *testing.T) {
	_, view := scanOwnView(t, []storage.Mutation{
		scanOwnMutation(storage.MutationDeleteRange, "a", "d", 0),
		scanOwnMutation(storage.MutationUpsert, "b", "", 99),
	})
	run := multiRowRun(t)
	spec := scanFixtureSpec()
	request := storage.ReadRequest{Target: spec.Target, Layout: spec.Layout, ColumnIDs: spec.Projection}
	for _, key := range []string{"a", "b", "c", "d"} {
		request.Key = []byte(key)
		row, err := readPAXPoint(t.Context(), pointSource(t, []*sstFile{run}, 10), view, request, pointReadLimits{ScanBytes: 1 << 20, RowBytes: 4096})
		if key == "a" || key == "c" {
			if !errors.Is(err, storage.ErrRowNotFound) {
				t.Fatalf("own range exposed committed key %s: %v", key, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		expected := int64(40)
		if key == "b" {
			expected = 99
			if row.CommitTS != 0 || row.Ordinal != 2 {
				t.Fatalf("own write acquired a fabricated commit identity: %+v", row)
			}
		}
		if row.Values[0].IntVal != expected {
			t.Fatalf("key %s value = %d, want %d", key, row.Values[0].IntVal, expected)
		}
	}
}

func TestPAXPointOwnResultDoesNotAliasFrozenWrite(t *testing.T) {
	value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -3}}, ArrayVal: []model.Value{model.Int64Val(7)}}
	mutation := storage.NewUpsertMutation(encTestTarget(), []byte("x"), []model.ColumnID{1}, []model.Value{value})
	_, view := scanOwnView(t, []storage.Mutation{mutation})
	request := storage.ReadRequest{Target: encTestTarget(), Key: []byte("x"), ColumnIDs: []model.ColumnID{1}, Layout: []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}}}}
	row, err := readPAXPoint(t.Context(), pointSource(t, nil, 10), view, request, pointReadLimits{ScanBytes: 1 << 20, RowBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	row.Key[0] = 'y'
	row.Values[0].Dimensions[0].LowerBound = 0
	row.Values[0].ArrayVal[0].IntVal = 99
	stored, found, err := view.Lookup(t.Context(), request.Target, request.Key)
	if err != nil || !found {
		t.Fatalf("owned result changed the addressed write: found=%v error=%v", found, err)
	}
	if stored.Values[0].Dimensions[0].LowerBound != -3 || stored.Values[0].ArrayVal[0].IntVal != 7 {
		t.Fatal("modifying the point result modified the frozen write")
	}
}

func TestPAXPointOwnValueDoesNotReadShadowedPAX(t *testing.T) {
	path := writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 10))
	run := openRun(t, path)
	if err := os.Truncate(path, sstHeaderSize); err != nil {
		t.Fatal(err)
	}
	_, view := scanOwnView(t, []storage.Mutation{scanOwnMutation(storage.MutationUpsert, "a", "", 99)})
	spec := scanFixtureSpec()
	request := storage.ReadRequest{Target: spec.Target, Key: []byte("a"), ColumnIDs: spec.Projection, Layout: spec.Layout}
	row, err := readPAXPoint(t.Context(), pointSource(t, []*sstFile{run}, 10), view, request, pointReadLimits{ScanBytes: 1 << 20, RowBytes: 4096})
	if err != nil || row.Values[0].IntVal != 99 {
		t.Fatalf("own point read accessed shadowed storage: row=%+v error=%v", row, err)
	}
	_, err = readPAXPoint(t.Context(), pointSource(t, []*sstFile{run}, 10), nil, request, pointReadLimits{ScanBytes: 1 << 20, RowBytes: 4096})
	if !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("committed point hid required storage corruption: %v", err)
	}
}
