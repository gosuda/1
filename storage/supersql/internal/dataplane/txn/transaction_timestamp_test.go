package txn

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestManagedReadViewKeepsActualTimestampAfterAnotherCommit(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	fixed := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	view, err := fixed.ReadView(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	})
	timestamp := view.Timestamp()
	writer := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	contractApply(t, writer, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
	committed := contractCommit(t, writer)
	if committed.CommitTS <= timestamp {
		t.Fatalf("writer timestamp %d is not after snapshot %d", committed.CommitTS, timestamp)
	}
	if view.Timestamp() != timestamp {
		t.Fatalf("view timestamp followed publication frontier: got %d, want %d", view.Timestamp(), timestamp)
	}
	contractValues(t, view, fixture.request, 100)
	plan, err := view.PlanScan(t.Context(), storage.PushdownRequest{Target: fixture.request.Target, Layout: fixture.request.Layout, KeyLayout: contractKeyLayout(), ColumnIDs: fixture.request.ColumnIDs, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.SnapshotTS != timestamp {
		t.Fatalf("catalog timestamp %d differs from actual scan timestamp %d", timestamp, plan.SnapshotTS)
	}
}
