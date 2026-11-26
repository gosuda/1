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

func TestFutureBlocksArePrunedBeforePayloadRead(t *testing.T) {
	for _, direction := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
		t.Run(map[storage.ScanDirection]string{storage.ScanForward: "forward", storage.ScanReverse: "reverse"}[direction], func(t *testing.T) {
			path := writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 10), runBlock(t, "b", "", 30, storage.MutationUpsert, 30), runBlock(t, "c", "", 10, storage.MutationUpsert, 40))
			run := openRun(t, path)
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil { t.Fatal(err) }
			_, writeErr := file.WriteAt([]byte{0}, int64(run.entry(1).offset))
			if err := errors.Join(writeErr, file.Close()); err != nil { t.Fatal(err) }
			var source *runVersionCursor
			if direction == storage.ScanReverse {
				source, err = newReverseRunVersionCursor(run, 0, run.count, 1<<20, mergeLimits{MaxBytes: 64<<10, MaxKeyBytes: 64}, nil)
			} else {
				source, err = newRunVersionCursor(run, 0, run.count, 1<<20, nil)
			}
			if err != nil { t.Fatal(err) }
			cursor, err := newMVCCCursor(t.Context(), []*runVersionCursor{source}, 20, mergeLimits{MaxBytes: 64<<10, MaxKeyBytes: 64}, direction)
			if err != nil { t.Fatal(err) }
			defer func() { if err := cursor.Close(); err != nil { t.Error(err) } }()
			var keys []string
			for {
				row, err := cursor.Next(t.Context())
				if err == io.EOF { break }
				if err != nil { t.Fatal(err) }
				keys = append(keys, string(row.key))
			}
			want := []string{"a", "c"}
			if direction == storage.ScanReverse { slices.Reverse(want) }
			if !slices.Equal(keys, want) { t.Fatalf("visible keys = %v, want %v", keys, want) }
		})
	}
}

func TestZonePruningPreservesShadowingWithoutPredicateScratch(t *testing.T) {
	base := openRun(t, writeRun(t, runBlock(t, "a", "", 10, storage.MutationUpsert, 10)))
	latest := openRun(t, writeRun(t, runBlock(t, "a", "", 20, storage.MutationUpsert, 20)))
	spec := scanFixtureSpec()
	program, accepted := compilePredKernel(t, []storage.PredicateNode{{Kind: storage.PredicateColumn, ColumnID: 1, Type: spec.Layout[0].Type}, predConstNode(0, spec.Layout[0].Type), predCmpNode(410, 0, 1)}, nil, []model.Value{model.Int64Val(10)}, spec.Layout)
	if !accepted { t.Fatal("integer predicate was not accepted") }
	spec.Predicate, spec.PredicateBytes = program, 1
	cursor := scanFixture(t, []*sstFile{base, latest}, 20, spec)
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err != nil { t.Fatal(err) }
	if out.Rows() != 0 { t.Fatalf("zone rejection resurrected %d old matching rows", out.Rows()) }
	if err := cursor.Next(t.Context(), &out); err != io.EOF { t.Fatalf("after zone rejection: %v", err) }
}

func TestTierCompactionRetainsFixedSnapshotsAcrossReopen(t *testing.T) {
	fixture := openBackendFixture(t)
	for i, ts := range []model.Timestamp{10, 20, 30} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, byte(i+1), ts, int64(ts), 100-int64(ts))); err != nil { t.Fatal(err) }
	}
	pinned := fixture.snapshot(t, 20)
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 15}); err != nil { t.Fatal(err) }
	if got := backendBalance(t, pinned, 0); got != 20 { t.Fatalf("pinned value = %d, want 20", got) }
	if err := pinned.Close(); err != nil { t.Fatal(err) }
	if err := fixture.backend.Close(); err != nil { t.Fatal(err) }
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil { t.Fatal(err) }
	defer func() { if err := reopened.Close(); err != nil { t.Error(err) } }()
	fixture.backend = reopened
	for _, tc := range []struct { timestamp model.Timestamp; want int64 }{{15, 10}, {20, 20}, {30, 30}} {
		reader := fixture.snapshot(t, tc.timestamp)
		if got := backendBalance(t, reader, 0); got != tc.want { t.Fatalf("snapshot %d = %d, want %d", tc.timestamp, got, tc.want) }
		if err := reader.Close(); err != nil { t.Fatal(err) }
	}
	if err := reopened.Collect(t.Context(), storage.GCRequest{Boundary: 25}); err != nil { t.Fatal(err) }
	reader := fixture.snapshot(t, 25)
	if got := backendBalance(t, reader, 0); got != 20 { t.Fatalf("retained boundary anchor = %d, want 20", got) }
}

func TestCompactedSSTPreservesHistoricalBlockLayouts(t *testing.T) {
	target := encTestTarget()
	target.DescriptorVersion++
	versions := blockVersions{Keys: encTestKeyVector("a"), CommitTS: []model.Timestamp{20}, Ordinals: []uint64{1}, Kinds: []storage.MutationOp{storage.MutationUpsert}}
	batch := columnar.Batch{Length: 1, Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 1, Int64: []int64{20}}}}
	newer, err := encodeBlock(t.Context(), target, versions, &batch, []model.ColumnID{1}, encTestLimits())
	if err != nil { t.Fatal(err) }
	file, err := os.CreateTemp(t.TempDir(), "mixed-*.sst")
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) { t.Error(err) } })
	writer, err := newSSTWriter(file, target, runLimits())
	if err != nil { t.Fatal(err) }
	if err := writer.Append(t.Context(), newer); err != nil { t.Fatal(err) }
	if err := writer.Append(t.Context(), runBlock(t, "a", "", 10, storage.MutationUpsert, 10)); err != nil { t.Fatal(err) }
	if err := errors.Join(writer.Finish(t.Context()), file.Close()); err != nil { t.Fatal(err) }
	run := openRun(t, file.Name())
	for _, timestamp := range []model.Timestamp{10, 20} {
		spec := scanFixtureSpec()
		if timestamp == 20 { spec.Target = target }
		cursor := scanFixture(t, []*sstFile{run}, timestamp, spec)
		if got := collectScanInts(t, cursor); !slices.Equal(got, []int64{int64(timestamp)}) { t.Fatalf("snapshot %d layout: %v", timestamp, got) }
	}
}

func TestPointReadSkipsPayloadHolesUnderRangeCoverage(t *testing.T) {
	path := writeRun(t, runBlock(t, "a", "z", 20, storage.MutationDeleteRange, 0), runBlock(t, "b", "", 10, storage.MutationUpsert, 10), runBlock(t, "y", "", 30, storage.MutationUpsert, 30))
	run := openRun(t, path)
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil { t.Fatal(err) }
	_, writeErr := file.WriteAt([]byte{0}, int64(run.entry(1).offset))
	if err := errors.Join(writeErr, file.Close()); err != nil { t.Fatal(err) }
	set, err := newRunSet(runSetLimits{MaxBytes: 1<<20, MaxFiles: 4, MaxPins: 4})
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { if err := set.Close(); err != nil { t.Error(err) } })
	if err := set.publish(t.Context(), 1, 30, []*sstFile{run}); err != nil { t.Fatal(err) }
	pin, err := set.pin(t.Context())
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { if err := pin.Close(); err != nil { t.Error(err) } })
	spec := scanFixtureSpec()
	request := storage.ReadRequest{Target: spec.Target, Key: []byte("y"), Layout: spec.Layout, ColumnIDs: spec.Projection}
	limits := pinnedPointLimits{SourceBytes: 3<<20, BlockBytes: 1<<20, Merge: mergeLimits{MaxBytes: 64<<10, MaxKeyBytes: 64}, Point: pointReadLimits{ScanBytes: 1<<20, RowBytes: 4096}}
	row, err := pin.get(t.Context(), 30, nil, request, limits)
	if err != nil { t.Fatal(err) }
	if row.Values[0].IntVal != 30 { t.Fatalf("newer point lost under range tombstone: %+v", row) }
	if _, err := pin.get(t.Context(), 20, nil, request, limits); !errors.Is(err, storage.ErrRowNotFound) { t.Fatalf("covered historical point: %v", err) }
}
