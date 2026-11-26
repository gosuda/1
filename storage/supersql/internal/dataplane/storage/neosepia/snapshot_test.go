package neosepia

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func snapshotFixture(t *testing.T) (storage.SnapshotReader, *runSet) {
	t.Helper()
	set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 4, MaxPins: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := set.publish(t.Context(), 1, 10, []*sstFile{multiRowRun(t)}); err != nil {
		t.Fatal(err)
	}
	pin, err := set.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := newSnapshotReader(t.Context(), pin, storage.SnapshotSpec{Timestamp: 10, Fence: storage.ReadFence{ID: [16]byte{1}, ClosedTS: 10, LogPosition: 1}}, nil, readLimits{
		SourceBytes: 8 << 20, BlockBytes: 1 << 20, MergeBytes: 64 << 10, KeyBytes: 4096, ScanBytes: 1 << 20, RowBytes: 1 << 20, PredicateBytes: 1 << 20,
		OverlayBytes: 64 << 10, OverlayBlockBytes: 1 << 20, PlanBytes: 8 << 20, MaxRows: 1024, MaxNodes: 128, MaxPlans: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	return reader, set
}

func snapshotRequest() storage.PushdownRequest {
	spec := scanFixtureSpec()
	return storage.PushdownRequest{Target: spec.Target, Layout: spec.Layout, ColumnIDs: spec.Projection, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}
}

func TestSnapshotEmptySpansHaveNoCommittedScanCost(t *testing.T) {
	reader, _ := snapshotFixture(t)
	request := snapshotRequest()
	request.Spans = nil
	plan, err := reader.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Estimate.Rows != 0 || plan.Estimate.VisibilityChecks != 0 || plan.Estimate.ReadBytes != 0 {
		t.Fatalf("empty spans charged committed scan work: %+v", plan.Estimate)
	}
}

func TestSnapshotRejectsPlanTamperingAndRetainsActiveCursor(t *testing.T) {
	reader, set := snapshotFixture(t)
	request := snapshotRequest()
	request.Direction = storage.ScanReverse
	request.SafeLimit = storage.ScanLimit{Enabled: true, Count: 2}
	predicate, err := storage.NewPredicate([]storage.PredicateNode{{Kind: storage.PredicateColumn, ColumnID: 1, Type: request.Layout[0].Type}, predConstNode(0, request.Layout[0].Type), predCmpNode(412, 0, 1)}, nil, []model.Value{model.Int64Val(35)})
	if err != nil {
		t.Fatal(err)
	}
	request.Predicate = predicate
	plan, err := reader.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Layout[0].ID = 99
	plan.Projection[0] = 99
	if cursor, err := reader.VectorizedScan(t.Context(), plan); !errors.Is(err, storage.ErrInvalidScanPlan) {
		if cursor != nil {
			cursor.Close()
		}
		t.Fatalf("modified projection accepted: %v", err)
	}
	plan.Projection[0] = 1
	cursor, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	if got := collectScanInts(t, cursor); !slices.Equal(got, []int64{30, 20}) {
		t.Fatalf("bound reverse filter/LIMIT: %v", got)
	}
}

func TestSnapshotResidualAddsInputsAndDefersLimit(t *testing.T) {
	reader, _ := snapshotFixture(t)
	request := snapshotRequest()
	request.ColumnIDs = nil
	request.SafeLimit = storage.ScanLimit{Enabled: true, Count: 1}
	predicate, err := storage.NewPredicate([]storage.PredicateNode{{Kind: storage.PredicateColumn, ColumnID: 1, Type: request.Layout[0].Type}, predConstNode(0, request.Layout[0].Type), predCmpNode(model.OperatorID(^uint32(0)), 0, 1)}, nil, []model.Value{model.Int64Val(35)})
	if err != nil {
		t.Fatal(err)
	}
	request.Predicate = predicate
	plan, err := reader.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := collectScanInts(t, cursor); !slices.Equal(got, []int64{10, 20, 30, 40}) {
		t.Fatalf("residual input stream was pruned: %v", got)
	}
}

func TestSnapshotRejectsForeignReaderPlan(t *testing.T) {
	first, _ := snapshotFixture(t)
	second, _ := snapshotFixture(t)
	plan, err := first.PlanScan(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := second.VectorizedScan(t.Context(), plan)
	if cursor != nil {
		cursor.Close()
	}
	if !errors.Is(err, storage.ErrInvalidScanPlan) {
		t.Fatalf("foreign reader plan: %v", err)
	}
}

func TestSnapshotNaNPredicatePlanRemainsExecutable(t *testing.T) {
	reader, _ := snapshotFixture(t)
	request := snapshotRequest()
	typ := model.Type{ID: 701, Storage: model.TypeFloat64}
	predicate, err := storage.NewPredicate(
		[]storage.PredicateNode{predConstNode(0, typ), predConstNode(0, typ), predCmpNode(670, 0, 1)},
		nil, []model.Value{{Type: model.TypeFloat64, FloatVal: math.NaN()}},
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Predicate = predicate
	request.SafeLimit = storage.ScanLimit{Enabled: true, Count: 1}
	plan, err := reader.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := collectScanInts(t, cursor); !slices.Equal(got, []int64{10}) {
		t.Fatalf("PostgreSQL NaN equality: %v", got)
	}
}
