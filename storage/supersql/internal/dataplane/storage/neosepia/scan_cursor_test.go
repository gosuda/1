package neosepia

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func scanFixture(t *testing.T, runs []*sstFile, timestamp model.Timestamp, spec paxScanSpec) storage.BatchCursor {
	t.Helper()
	sources := make([]*runVersionCursor, len(runs))
	for i, run := range runs {
		source, err := newRunVersionCursor(run, 0, run.count, 1<<20, nil)
		if err != nil {
			t.Fatal(err)
		}
		sources[i] = source
	}
	merge, err := newMVCCCursor(t.Context(), sources, timestamp, mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}, storage.ScanForward)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := newPAXScanCursor(t.Context(), merge, spec)
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

func scanFixtureSpec() paxScanSpec {
	return paxScanSpec{Target: encTestTarget(), Layout: []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}}, Projection: []model.ColumnID{1}, Spans: []encodedSpan{{lower: keyCut{infinity: -1}, upper: keyCut{infinity: 1}}}, MaxRows: 1024, MaxBytes: 1 << 20, PredicateBytes: 1 << 20}
}

func TestPAXScanFiltersAfterVisibilityAndLimitsAfterFiltering(t *testing.T) {
	base := openRun(t, writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 10), runBlock(t, "b", "", 10, storage.MutationUpsert, 10)))
	change := openRun(t, writeRun(t, runBlock(t, "a", "", 20, storage.MutationUpsert, 20)))
	spec := scanFixtureSpec()
	predicate, accepted := compilePredKernel(t, []storage.PredicateNode{{Kind: storage.PredicateColumn, ColumnID: 1, Type: spec.Layout[0].Type}, predConstNode(0, spec.Layout[0].Type), predCmpNode(410, 0, 1)}, nil, []model.Value{model.Int64Val(10)}, spec.Layout)
	if !accepted {
		t.Fatal("int8 equality was not executable")
	}
	spec.Predicate = predicate
	spec.Limit = storage.ScanLimit{Enabled: true, Count: 1}
	cursor := scanFixture(t, []*sstFile{base, change}, 20, spec)
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 0 || len(out.Columns) != 1 {
		t.Fatal("newer nonmatching row resurrected an old match")
	}
	if err := out.Validate(1024); err != nil {
		t.Fatal(err)
	}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 {
		t.Fatalf("limit hid the first surviving row: %d", out.Rows())
	}
	value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(0), 1024)
	if err != nil || value.IntVal != 10 {
		t.Fatalf("surviving row: %v, %v", value, err)
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("limit did not terminate: %v", err)
	}
}

func multiRowRun(t *testing.T) *sstFile {
	t.Helper()
	versions := blockVersions{Keys: encTestKeyVector("a", "b", "c", "d"), CommitTS: []model.Timestamp{10, 10, 10, 10}, Ordinals: []uint64{1, 2, 3, 4}, Kinds: []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert, storage.MutationUpsert, storage.MutationUpsert}}
	batch := columnar.Batch{Length: 4, Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 4, Int64: []int64{10, 20, 30, 40}}}}
	data, err := encodeBlock(t.Context(), encTestTarget(), versions, &batch, []model.ColumnID{1}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	return openRun(t, writeRun(t, data))
}

func TestPAXScanHonorsOneRowCallerCapacity(t *testing.T) {
	cursor := scanFixture(t, []*sstFile{multiRowRun(t)}, 10, scanFixtureSpec())
	out := columnar.Batch{MaxRows: 1}
	for _, expected := range []int64{10, 20, 30, 40} {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Rows() != 1 {
			t.Fatalf("one-row caller received %d rows", out.Rows())
		}
		if err := out.Validate(1024); err != nil {
			t.Fatal(err)
		}
		value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(0), 1024)
		if err != nil || value.IntVal != expected {
			t.Fatalf("row value %v, want %d: %v", value, expected, err)
		}
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("extra scan row: %v", err)
	}
}

func TestPAXScanUsesDeclaredHistoricalArrayDefault(t *testing.T) {
	spec := scanFixtureSpec()
	spec.Target.DescriptorVersion++
	spec.Layout = append(spec.Layout, storage.ColumnLayout{ID: 70000, Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}, HasMissingValue: true, MissingValue: model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -2}}, ArrayVal: []model.Value{model.Int64Val(7), model.NullVal(model.TypeInt64)}}})
	spec.Projection = []model.ColumnID{70000}
	cursor := scanFixture(t, []*sstFile{multiRowRun(t)}, 10, spec)
	out := columnar.Batch{MaxRows: 1}
	for range 4 {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(0), 1<<16)
		if err != nil {
			t.Fatal(err)
		}
		if len(value.Dimensions) != 1 || value.Dimensions[0].LowerBound != -2 || len(value.ArrayVal) != 2 {
			t.Fatal("historical default lost its shape")
		}
		if value.ArrayVal[0].IntVal != 7 || !value.ArrayVal[1].IsNull {
			t.Fatal("historical default changed nested values")
		}
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("extra default row: %v", err)
	}
}

func TestPAXScanCancellationClearsBorrowedBatch(t *testing.T) {
	cursor := scanFixture(t, []*sstFile{multiRowRun(t)}, 10, scanFixtureSpec())
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cursor.Next(ctx, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if out.Rows() != 0 || len(out.Columns) != 0 {
		t.Fatal("failed scan retained its prior borrowed output")
	}
}

func TestPAXScanStopsBeforeUnrequestedCorruption(t *testing.T) {
	first := runBlock(t, "a", "", 10, storage.MutationUpsert, 10)
	second := runBlock(t, "b", "", 10, storage.MutationUpsert, 20)
	path := writeRun(t, first, second, runBlock(t, "c", "", 10, storage.MutationUpsert, 30))
	run := openRun(t, path)
	if err := os.Truncate(path, int64(sstHeaderSize+len(first)+len(second))); err != nil {
		t.Fatal(err)
	}
	spec := scanFixtureSpec()
	spec.Spans = []encodedSpan{{lower: keyCut{prefix: []byte("a")}, upper: keyCut{prefix: []byte("b")}}}
	cursor := scanFixture(t, []*sstFile{run}, 10, spec)
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows() != 1 {
		t.Fatalf("bounded span returned %d rows", out.Rows())
	}
	value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(0), 1024)
	if err != nil || value.IntVal != 10 {
		t.Fatalf("bounded result: %v, %v", value, err)
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("read beyond the requested span: %v", err)
	}
}

// TestPAXScanPublishesFullExtentWithShortSelection pins the published batch shape
// the caller's row request is validated against: the leased block is the physical
// extent every projected vector carries, while Selection names the surviving
// lanes. A caller that validates with its own request (the modify path) must
// accept the result, and lane values must index the physical vectors.
func TestPAXScanPublishesFullExtentWithShortSelection(t *testing.T) {
	spec := scanFixtureSpec()
	predicate, accepted := compilePredKernel(t, []storage.PredicateNode{{Kind: storage.PredicateColumn, ColumnID: 1, Type: spec.Layout[0].Type}, predConstNode(0, spec.Layout[0].Type), predCmpNode(410, 0, 1)}, nil, []model.Value{model.Int64Val(20)}, spec.Layout)
	if !accepted {
		t.Fatal("int8 equality was not executable")
	}
	spec.Predicate = predicate
	cursor := scanFixture(t, []*sstFile{multiRowRun(t)}, 10, spec)
	out := columnar.Batch{MaxRows: 2}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Length != 4 || out.Rows() != 1 {
		t.Fatalf("physical extent %d, logical rows %d; want 4 and 1", out.Length, out.Rows())
	}
	if err := out.Validate(out.MaxRows); err != nil {
		t.Fatalf("caller request rejected the filtered batch: %v", err)
	}
	value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(0), 1024)
	if err != nil || value.IntVal != 20 {
		t.Fatalf("lane %d selected %v, %v", out.Lane(0), value, err)
	}
}
