package neosepia

import (
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func reverseScanFixture(t *testing.T, run *sstFile, spec paxScanSpec) storage.BatchCursor {
	t.Helper()
	source, err := newReverseRunVersionCursor(run, 0, run.count, 1<<20, mergeLimits{MaxBytes: 4096, MaxKeyBytes: 64}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	visible, err := newMVCCCursor(t.Context(), []*runVersionCursor{source}, 10, mergeLimits{MaxBytes: 1 << 20, MaxKeyBytes: 64}, storage.ScanReverse)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := newPAXScanCursor(t.Context(), visible, spec)
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

func TestPAXReverseOwnTombstonesRespectExclusiveEnd(t *testing.T) {
	owner, view := scanOwnView(t, []storage.Mutation{
		scanOwnMutation(storage.MutationDeleteRange, "a", "c", 0),
		scanOwnMutation(storage.MutationUpsert, "b", "", 99),
		scanOwnMutation(storage.MutationDelete, "d", "", 0),
	})
	spec := scanFixtureSpec()
	spec.Overlay = view
	spec.OverlayLimits = mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}
	spec.OverlayBlockBytes = 1 << 20
	cursor := reverseScanFixture(t, multiRowRun(t), spec)
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if got := collectScanInts(t, cursor); !slices.Equal(got, []int64{30, 99}) {
		t.Fatalf("reverse frozen overlay: got %v, want [30 99]", got)
	}
}

func TestPAXReverseSpansApplyFilterBeforeLimit(t *testing.T) {
	spec := scanFixtureSpec()
	spec.Spans = []encodedSpan{
		{lower: keyCut{prefix: []byte("a")}, upper: keyCut{prefix: []byte("b")}},
		{lower: keyCut{prefix: []byte("c")}, upper: keyCut{prefix: []byte("e")}},
	}
	predicate, accepted := compilePredKernel(t, []storage.PredicateNode{{Kind: storage.PredicateColumn, ColumnID: 1, Type: spec.Layout[0].Type}, predConstNode(0, spec.Layout[0].Type), predCmpNode(412, 0, 1)}, nil, []model.Value{model.Int64Val(35)}, spec.Layout)
	if !accepted {
		t.Fatal("int8 comparison was not executable")
	}
	spec.Predicate = predicate
	spec.Limit = storage.ScanLimit{Enabled: true, Count: 2}
	cursor := reverseScanFixture(t, multiRowRun(t), spec)
	if got := collectScanInts(t, cursor); !slices.Equal(got, []int64{30, 10}) {
		t.Fatalf("reverse span/filter/LIMIT: got %v, want [30 10]", got)
	}
}
