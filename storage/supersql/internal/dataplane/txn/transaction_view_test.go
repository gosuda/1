package txn

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestManagedReleasedPlansReuseProbeCapacity(t *testing.T) {
	for _, mode := range []struct {
		name string
		mode TransactionMode
	}{
		{name: "read write", mode: ReadWrite},
		{name: "read only", mode: ReadOnly},
		{name: "historical", mode: HistoricalReadOnly},
	} {
		t.Run(mode.name, func(t *testing.T) {
			fixture := newCommitFixture(t, nil)
			options := contractBalanceOptions(t, fixture)
			options.Mode, options.MaxViews = mode.mode, 3
			if mode.mode == HistoricalReadOnly {
				options.Timestamp = 20
			}
			transaction := contractBegin(t, fixture.manager, options)
			want := int64(100)
			if mode.mode == ReadWrite {
				want = 90
				contractApply(t, transaction, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(want)}))
			}
			view := contractView(t, transaction, fixture.request.Target, contractSpan(1, 3))
			request := storage.PushdownRequest{Target: fixture.request.Target, Layout: fixture.request.Layout, KeyLayout: contractKeyLayout(), ColumnIDs: fixture.request.ColumnIDs}
			var stale storage.PushdownPlan
			for probe := range 128 {
				key := int64(1 + probe%2)
				request.Spans = []storage.KeySpan{contractSpan(key, key+1)}
				plan, err := view.PlanScan(t.Context(), request)
				if err != nil {
					t.Fatalf("probe %d planning: %v", probe, err)
				}
				if probe != 0 {
					if _, err := view.VectorizedScan(t.Context(), stale); !errors.Is(err, storage.ErrInvalidScanPlan) {
						t.Fatalf("probe %d reopened retired binding: %v", probe, err)
					}
				}
				cursor, err := view.VectorizedScan(t.Context(), plan)
				if err != nil {
					t.Fatal(err)
				}
				batch := columnar.Batch{MaxRows: 1}
				err = cursor.Next(t.Context(), &batch)
				if key == 2 {
					if err != io.EOF {
						t.Fatalf("absent probe %d: %v, want EOF", probe, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if batch.Rows() != 1 {
						t.Fatalf("probe %d rows = %d, want 1", probe, batch.Rows())
					}
					value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(0), 4096)
					if err != nil || value.IntVal != want {
						t.Fatalf("probe %d value = %+v, %v; want %d", probe, value, err, want)
					}
					if err := cursor.Next(t.Context(), &batch); err != io.EOF {
						t.Fatalf("probe %d duplicate row: %v", probe, err)
					}
				}
				if err := cursor.Close(); err != nil {
					t.Fatal(err)
				}
				if err := view.ReleasePlan(plan); err != nil {
					t.Fatal(err)
				}
				stale = plan
			}
			contractValues(t, view, fixture.request, want)
		})
	}
}

func TestManagedReleaseRequiresAllBindingCursorsClosed(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := TransactionOptions{Mode: HistoricalReadOnly, Timestamp: 20, MaxViews: 3}
	transaction := contractBegin(t, fixture.manager, options)
	view := contractView(t, transaction, fixture.request.Target, contractSpan(1, 2))
	request := storage.PushdownRequest{Target: fixture.request.Target, Layout: fixture.request.Layout, KeyLayout: contractKeyLayout(), ColumnIDs: fixture.request.ColumnIDs, Spans: []storage.KeySpan{contractSpan(1, 2)}}
	plan, err := view.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	foreignTransaction := contractBegin(t, fixture.manager, options)
	foreignView := contractView(t, foreignTransaction, fixture.request.Target, contractSpan(1, 2))
	foreignPlan, err := foreignView.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []storage.PushdownPlan{{}, foreignPlan} {
		if err := view.ReleasePlan(invalid); !errors.Is(err, storage.ErrInvalidScanPlan) {
			t.Fatalf("unregistered release = %v, want invalid plan", err)
		}
	}
	first, err := view.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := view.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := view.ReleasePlan(plan); !errors.Is(err, storage.ErrInvalidScanPlan) {
		t.Fatalf("active release = %v, want invalid plan", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := view.ReleasePlan(plan); !errors.Is(err, storage.ErrInvalidScanPlan) {
		t.Fatalf("second cursor still active: %v", err)
	}
	batch := columnar.Batch{MaxRows: 1}
	if err := second.Next(t.Context(), &batch); err != nil {
		t.Fatalf("rejected release invalidated active cursor: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	view.(*transactionView).cancel(context.Canceled)
	if err := view.ReleasePlan(plan); err != nil {
		t.Fatalf("cancellation prevented release: %v", err)
	}
	if err := view.ReleasePlan(plan); !errors.Is(err, storage.ErrInvalidScanPlan) {
		t.Fatalf("stale release = %v, want invalid plan", err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := view.ReleasePlan(plan); err != nil {
		t.Fatalf("closed view cleanup: %v", err)
	}
	if transaction.resources.Load() != 0 {
		t.Fatalf("closed view retained %d resources", transaction.resources.Load())
	}
	cursor, err := foreignView.VectorizedScan(t.Context(), foreignPlan)
	if err != nil {
		t.Fatalf("foreign release invalidated its owner: %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedReleaseReclaimsOnlyPlanBytes(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, TransactionOptions{Mode: HistoricalReadOnly, Timestamp: 20, MaxViews: 3})
	view := contractView(t, transaction, fixture.request.Target, contractSpan(1, 2)).(*transactionView)
	request := storage.PushdownRequest{Target: fixture.request.Target, Layout: fixture.request.Layout, KeyLayout: contractKeyLayout(), ColumnIDs: fixture.request.ColumnIDs, Spans: []storage.KeySpan{contractSpan(1, 2)}}
	plan, err := view.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	// The allowance is shared by open views, so exhausting it means giving it no
	// headroom beyond what this view already holds.
	transaction.viewSpace.limit = transaction.viewSpace.used
	if _, err := view.PlanScan(t.Context(), request); !errors.Is(err, errTransactionBudget) {
		t.Fatalf("full byte budget accepted another plan: %v", err)
	}
	invalid := plan
	invalid.SnapshotTS++
	if _, err := view.VectorizedScan(t.Context(), invalid); !errors.Is(err, storage.ErrInvalidScanPlan) {
		t.Fatalf("modified plan = %v, want invalid plan", err)
	}
	if err := view.ReleasePlan(plan); err != nil {
		t.Fatalf("failed cursor opening retained registration: %v", err)
	}
	transaction.viewSpace.limit--
	if _, err := view.PlanScan(t.Context(), request); !errors.Is(err, errTransactionBudget) {
		t.Fatalf("release refunded retained registry storage: %v", err)
	}
	transaction.viewSpace.limit++
	for range 16 {
		plan, err = view.PlanScan(t.Context(), request)
		if err != nil {
			t.Fatalf("released bytes unavailable: %v", err)
		}
		cursor, err := view.VectorizedScan(t.Context(), plan)
		if err != nil {
			t.Fatalf("failed scan leaked cursor capacity: %v", err)
		}
		if err := cursor.Close(); err != nil {
			t.Fatal(err)
		}
		if err := view.ReleasePlan(plan); err != nil {
			t.Fatal(err)
		}
	}
}
