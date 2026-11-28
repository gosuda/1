package neosepia

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func scanOwnMutation(op storage.MutationOp, key, end string, value int64) storage.Mutation {
	mutation := storage.Mutation{Op: op, Target: encTestTarget(), Key: []byte(key), EndKey: []byte(end)}
	if op == storage.MutationUpsert {
		mutation.ColumnIDs = []model.ColumnID{1}
		mutation.Values = []model.Value{model.Int64Val(value)}
	}
	return mutation
}

func scanOwnView(t *testing.T, mutations []storage.Mutation) (*txn.Overlay, storage.OverlayView) {
	t.Helper()
	overlay, err := txn.NewOverlay(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := overlay.Close(); err != nil {
			t.Error(err)
		}
	})
	for i := range mutations {
		mutations[i].Ordinal = uint64(i + 1)
	}
	if err := overlay.StageOwned(t.Context(), mutations); err != nil {
		t.Fatal(err)
	}
	view, err := overlay.Freeze(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	})
	return overlay, view
}

func collectScanInts(t *testing.T, cursor storage.BatchCursor) []int64 {
	t.Helper()
	var values []int64
	out := columnar.Batch{MaxRows: 1}
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			return values
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := out.Validate(1024); err != nil {
			t.Fatal(err)
		}
		for row := 0; row < out.Rows(); row++ {
			value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(row), 1024)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, value.IntVal)
		}
	}
}

func TestPAXOwnWritesSuppressCommittedMatchesBeforeLimit(t *testing.T) {
	mutations := []storage.Mutation{
		scanOwnMutation(storage.MutationUpsert, "a", "", 99),
		scanOwnMutation(storage.MutationDelete, "b", "", 0),
		scanOwnMutation(storage.MutationUpsert, "c", "", 33),
		scanOwnMutation(storage.MutationDeleteRange, "c", "e", 0),
		scanOwnMutation(storage.MutationUpsert, "d", "", 44),
	}
	overlay, view := scanOwnView(t, mutations)
	spec := scanFixtureSpec()
	spec.Overlay = view
	spec.OverlayLimits = mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}
	spec.OverlayBlockBytes = 1 << 20
	predicate, accepted := compilePredKernel(t, []storage.PredicateNode{{Kind: storage.PredicateColumn, ColumnID: 1, Type: spec.Layout[0].Type}, predConstNode(0, spec.Layout[0].Type), predCmpNode(412, 0, 1)}, nil, []model.Value{model.Int64Val(50)}, spec.Layout)
	if !accepted {
		t.Fatal("int8 comparison was not executable")
	}
	spec.Predicate = predicate
	spec.Limit = storage.ScanLimit{Enabled: true, Count: 1}
	cursor := scanFixture(t, []*sstFile{multiRowRun(t)}, 10, spec)
	later := scanOwnMutation(storage.MutationUpsert, "a", "", 1)
	later.Ordinal = 6
	if err := overlay.StageOwned(t.Context(), []storage.Mutation{later}); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := overlay.Close(); err != nil {
		t.Fatal(err)
	}
	if got := collectScanInts(t, cursor); !slices.Equal(got, []int64{44}) {
		t.Fatalf("frozen filtered LIMIT result = %v, want [44]", got)
	}
}

func TestPAXOwnRangePointOrdinalPrecedence(t *testing.T) {
	for _, rangeFirst := range []bool{false, true} {
		name := "insert_then_range"
		expected := []int64{10, 20, 40}
		if rangeFirst {
			name = "range_then_insert"
			expected = []int64{10, 20, 33, 40}
		}
		t.Run(name, func(t *testing.T) {
			mutations := []storage.Mutation{scanOwnMutation(storage.MutationUpsert, "c", "", 33), scanOwnMutation(storage.MutationDeleteRange, "c", "d", 0)}
			if rangeFirst {
				slices.Reverse(mutations)
			}
			_, view := scanOwnView(t, mutations)
			spec := scanFixtureSpec()
			spec.Overlay = view
			spec.OverlayLimits = mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}
			spec.OverlayBlockBytes = 1 << 20
			cursor := scanFixture(t, []*sstFile{multiRowRun(t)}, 10, spec)
			if got := collectScanInts(t, cursor); !slices.Equal(got, expected) {
				t.Fatalf("visible values = %v, want %v", got, expected)
			}
		})
	}
}

func TestPAXOwnProjectionFlushesWithinByteBudget(t *testing.T) {
	mutations := make([]storage.Mutation, 20)
	expected := make([]string, len(mutations))
	for i := range mutations {
		key := fmt.Sprintf("%02d", i)
		expected[i] = key + strings.Repeat(".", 512)
		mutations[i] = storage.NewUpsertMutation(encTestTarget(), []byte(key), []model.ColumnID{1, 70000}, []model.Value{model.StringVal(expected[i]), model.BytesVal([]byte(strings.Repeat("x", 8192)))})
	}
	_, view := scanOwnView(t, mutations)
	spec := scanFixtureSpec()
	spec.Layout = []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 25, Storage: model.TypeString}}}
	spec.Overlay = view
	spec.OverlayLimits = mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}
	spec.OverlayBlockBytes = 4096
	cursor := scanFixture(t, nil, 10, spec)
	var out columnar.Batch
	var got []string
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := out.Validate(1024); err != nil {
			t.Fatal(err)
		}
		for row := 0; row < out.Rows(); row++ {
			value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(row), 1024)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, value.StrVal)
		}
	}
	if !slices.Equal(got, expected) {
		t.Fatalf("bounded projection lost or duplicated values: got %d rows, want %d", len(got), len(expected))
	}
}
