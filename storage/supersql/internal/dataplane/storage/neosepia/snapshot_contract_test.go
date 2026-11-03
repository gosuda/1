package neosepia

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSnapshotScanContract(t *testing.T) {
	state := databaseStateFixture()
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.BlockRows = 1
	fixture := backendFixture{
		config:    Config{Directory: t.TempDir(), Scale: state.Scales[0].Scale, KeyEncoder: backendIntEncoder{}, FenceVerifier: authority, Limits: limits},
		authority: authority,
		epoch:     state.Closed.Epoch,
	}
	fixture.backend, err = Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fixture.backend.Close(); err != nil {
			t.Error(err)
		}
	})

	intType := model.Type{ID: 20, Storage: model.TypeInt64}
	layout := []storage.ColumnLayout{{ID: 1, Type: intType}, {ID: 2, Type: intType}}
	base := encTestTarget()
	index := base
	index.ObjectID++
	// The covering index is a separate stored object, not a SQL planner hint.
	layouts := []storage.ObjectLayout{{Target: base, Columns: layout}, {Target: index, Columns: layout}}
	upsert := func(target storage.ObjectRef, key, id, balance int64) storage.Mutation {
		return storage.NewUpsertMutation(target, backendKey(key), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(id), model.Int64Val(balance)})
	}
	remove := func(target storage.ObjectRef, key int64) storage.Mutation {
		return storage.NewDeleteMutation(target, backendKey(key))
	}
	batches := [][]storage.Mutation{
		{upsert(base, 0, 0, 0), upsert(index, 0, 0, 0), upsert(base, 1, 1, 10), upsert(index, 10, 1, 10)},
		{upsert(base, 1, 1, 20), remove(index, 10), upsert(index, 20, 1, 20)},
		{remove(base, 1), remove(index, 20)},
	}
	for i, mutations := range batches {
		for j := range mutations {
			mutations[j].Ordinal = uint64(j + 1)
		}
		batch := storage.MutationBatch{TxID: model.TxnID{byte(i + 1)}, CommitTS: model.Timestamp((i + 1) * 10), Epoch: fixture.epoch, Layouts: layouts, Mutations: mutations, EncodingVersion: storage.MutationEncodingVersion}
		batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatalf("seed T%d base and covering index: %v", i+1, err)
		}
	}
	predicate, err := storage.NewPredicate([]storage.PredicateNode{
		{Kind: storage.PredicateColumn, ColumnID: 2, Type: intType},
		predConstNode(0, intType),
		predCmpNode(410, 0, 1),
	}, nil, []model.Value{model.Int64Val(10)})
	if err != nil {
		t.Fatal(err)
	}
	requests := []struct {
		name    string
		request storage.PushdownRequest
	}{
		{"base", storage.PushdownRequest{Target: base, KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}}}},
		{"covering_index", storage.PushdownRequest{Target: index, KeyLayout: []storage.KeyColumn{{ColumnID: 2, Type: intType}}}},
	}
	for i := range requests {
		requests[i].request.Layout = layout
		requests[i].request.ColumnIDs = []model.ColumnID{1}
		requests[i].request.Spans = []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
		requests[i].request.Predicate = predicate
		requests[i].request.SafeLimit = storage.ScanLimit{Enabled: true, Count: 1}
	}
	assertScans := func(t *testing.T, timestamp model.Timestamp, overlay storage.OverlayView, want []int64) {
		t.Helper()
		reader := snapshotContractReader(t, fixture, timestamp, overlay)
		defer snapshotContractClose(t, reader)
		for _, scan := range requests {
			t.Run(scan.name, func(t *testing.T) {
				cursor := snapshotContractCursor(t, reader, scan.request)
				defer snapshotContractClose(t, cursor)
				got := snapshotContractIDs(t, cursor)
				if !slices.Equal(got, want) {
					t.Fatalf("balance=10 at timestamp %d: got IDs %v, want %v", timestamp, got, want)
				}
			})
		}
	}
	assertHistory := func(t *testing.T) {
		t.Helper()
		for _, sample := range []struct {
			name      string
			timestamp model.Timestamp
			want      []int64
		}{{"T1", 10, []int64{1}}, {"T2", 20, nil}, {"T3", 30, nil}} {
			t.Run(sample.name, func(t *testing.T) {
				assertScans(t, sample.timestamp, nil, sample.want)
			})
		}
	}
	t.Run("before_collection", assertHistory)

	t.Run("frozen_own_writes", func(t *testing.T) {
		overlay, err := txn.NewOverlay(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		defer snapshotContractClose(t, overlay)
		var ordinal uint64
		stage := func(mutations ...storage.Mutation) storage.OverlayView {
			for i := range mutations {
				ordinal++
				mutations[i].Ordinal = ordinal
			}
			if err := overlay.StageOwned(t.Context(), mutations); err != nil {
				t.Fatal(err)
			}
			view, err := overlay.Freeze(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { snapshotContractClose(t, view) })
			return view
		}
		updated := stage(upsert(base, 1, 1, 20), remove(index, 10), upsert(index, 20, 1, 20), upsert(base, 2, 2, 10), upsert(index, 10, 2, 10))
		deleted := stage(remove(base, 2), remove(index, 10))
		// A newer point survives the range; a still newer range shadows it.
		rangeThenPoint := stage(
			storage.NewDeleteRangeMutation(base, backendKey(1), backendKey(4)),
			storage.NewDeleteRangeMutation(index, backendKey(10), backendKey(31)),
			upsert(base, 3, 3, 10), upsert(index, 10, 3, 10),
		)
		pointThenRange := stage(
			storage.NewDeleteRangeMutation(base, backendKey(1), backendKey(4)),
			storage.NewDeleteRangeMutation(index, backendKey(10), backendKey(31)),
		)
		for _, sample := range []struct {
			name string
			view storage.OverlayView
			want []int64
		}{
			{"update_before_filter_and_limit", updated, []int64{2}},
			{"delete_before_filter_and_limit", deleted, nil},
			{"point_after_range", rangeThenPoint, []int64{3}},
			{"range_after_point", pointThenRange, nil},
			{"committed_view_unchanged", nil, []int64{1}},
		} {
			t.Run(sample.name, func(t *testing.T) { assertScans(t, 10, sample.view, sample.want) })
		}
	})

	reader := snapshotContractReader(t, fixture, 10, nil)
	cursor := snapshotContractCursor(t, reader, requests[0].request)
	t.Cleanup(func() { snapshotContractClose(t, cursor) })
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	var oldRuns []string
	for generation := cursor.(*pinnedScanCursor).pin.generation; generation != nil; generation = generation.parent {
		for _, file := range generation.files {
			oldRuns = append(oldRuns, file.descriptor.Name)
		}
	}
	if len(oldRuns) == 0 {
		t.Fatal("seeded backend has no SST files to exercise reclamation")
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 10}); err != nil {
		t.Fatal(err)
	}
	retiredRuns := make(map[string]struct{}, len(oldRuns))
	for _, name := range oldRuns {
		retiredRuns[name] = struct{}{}
	}
	current, err := fixture.backend.runs.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for generation := current.generation; generation != nil; generation = generation.parent {
		for _, file := range generation.files {
			delete(retiredRuns, file.descriptor.Name)
		}
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	if len(retiredRuns) == 0 {
		t.Fatal("collection did not retire any cursor-owned SST")
	}
	for _, name := range oldRuns {
		if _, err := os.Stat(filepath.Join(fixture.config.Directory, "runs", name)); err != nil {
			t.Fatalf("cursor's pinned SST %q reclaimed before close: %v", name, err)
		}
	}
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatalf("cursor cannot read after its reader closed and collection: %v", err)
	}
	if out.Rows() != 0 {
		t.Fatalf("retained cursor's first filtered block has %d rows, want zero", out.Rows())
	}
	t.Run("after_collection", assertHistory)
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 10}); err != nil {
		t.Fatal(err)
	}
	for name := range retiredRuns {
		if _, err := os.Stat(filepath.Join(fixture.config.Directory, "runs", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SST %q remains after early cursor close and collection: %v", name, err)
		}
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	fixture.backend = reopened
	t.Run("after_reopen", assertHistory)
}

func snapshotContractReader(t *testing.T, fixture backendFixture, timestamp model.Timestamp, overlay storage.OverlayView) storage.SnapshotReader {
	t.Helper()
	state, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), max(timestamp, state.ClosedTS), state.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: timestamp, Fence: fence, Overlay: overlay})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snapshotContractClose(t, reader) })
	return reader
}

func snapshotContractCursor(t *testing.T, reader storage.SnapshotReader, request storage.PushdownRequest) storage.BatchCursor {
	t.Helper()
	plan, err := reader.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	return cursor
}

func snapshotContractIDs(t *testing.T, cursor storage.BatchCursor) []int64 {
	t.Helper()
	out := columnar.Batch{MaxRows: 1}
	var ids []int64
	for call := range 16 {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			if call == 0 {
				t.Fatal("scan skipped the nonmatching first block instead of returning an empty batch")
			}
			return ids
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := out.Validate(1024); err != nil {
			t.Fatal(err)
		}
		if len(out.Columns) != 1 || out.Rows() > 1 {
			t.Fatalf("narrow one-row output: columns=%d rows=%d", len(out.Columns), out.Rows())
		}
		if call == 0 && out.Rows() != 0 {
			t.Fatalf("first filtered block has %d rows, want zero", out.Rows())
		}
		for row := range out.Rows() {
			value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(row), 1024)
			if err != nil {
				t.Fatal(err)
			}
			if value.Type != model.TypeInt64 || value.IsNull {
				t.Fatalf("projected id is not a nonnull integer: %v", value)
			}
			ids = append(ids, value.IntVal)
		}
	}
	t.Fatal("scan did not terminate within the fixture's bounded row count")
	return nil
}

func snapshotContractClose(t *testing.T, resource io.Closer) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Error(err)
	}
}
