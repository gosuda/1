package session

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestCursorFetchSelectionTransitions(t *testing.T) {
	t.Run("forward crossing end", func(t *testing.T) {
		cursor := namedCursor{position: 2}
		source := cursorFetchSource{cursor: &cursor}
		source.setForward(5, 10)
		if source.next != 3 || source.step != 1 || source.remaining != 3 || source.boundary != 6 {
			t.Fatalf("forward selection = %+v", source)
		}
	})
	t.Run("backward crossing start", func(t *testing.T) {
		cursor := namedCursor{position: 6}
		source := cursorFetchSource{cursor: &cursor}
		source.setBackward(5, 10)
		if source.next != 5 || source.step != -1 || source.remaining != 5 || source.boundary != 0 {
			t.Fatalf("backward selection = %+v", source)
		}
	})
	t.Run("current preserves boundary state", func(t *testing.T) {
		cursor := namedCursor{position: 6}
		source := cursorFetchSource{cursor: &cursor}
		source.setCurrent(5)
		if source.remaining != 0 || source.boundary != 6 {
			t.Fatalf("current selection = %+v", source)
		}
	})
	if magnitude := fetchMagnitude(math.MinInt64); magnitude != uint64(math.MaxInt64)+1 {
		t.Fatalf("minimum int magnitude = %d", magnitude)
	}
}

func TestSessionIntrospectionOrderingCancellationAndReentry(t *testing.T) {
	preparedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	transactions := &Transactions{named: sessionNamedObjects{prepared: map[string]*namedPreparedStatement{
		"z": {name: "z", statement: "SELECT 2", preparedAt: preparedAt},
		"a": {name: "a", statement: "SELECT 1", preparedAt: preparedAt},
	}}}
	operation := &sessionTransactionOperation{owner: transactions}
	operation.active.Store(true)
	ctx := context.WithValue(context.Background(), sessionTransactionKey{}, operation)
	var names []string
	err := transactions.VisitPreparedStatements(ctx, func(summary model.PreparedStatementSummary) error {
		names = append(names, summary.Name)
		if len(names) == 1 {
			reentry := transactions.VisitPreparedStatements(ctx, func(model.PreparedStatementSummary) error { return nil })
			if !errors.Is(reentry, errSessionTransactionReentry) {
				t.Fatalf("reentry error = %v", reentry)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "z" {
		t.Fatalf("visit order = %v", names)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	visited := false
	if err := transactions.VisitPreparedStatements(canceled, func(model.PreparedStatementSummary) error {
		visited = true
		return nil
	}); err == nil {
		t.Fatal("canceled visit succeeded")
	}
	if visited {
		t.Fatal("canceled visit called visitor")
	}
}

func TestNamedPreparedSearchPathAndDependencyInvalidation(t *testing.T) {
	entry := &namedPreparedStatement{
		searchPath: []string{"first", "public"},
		semantics:  builtin.ResolveOptions{},
	}
	if namedPreparedSettingsChanged(entry, []string{"first", "public"}, builtin.ResolveOptions{}) || cachedPreparedInvalidation(nil) {
		t.Fatal("new earlier same-name relation reanalyzed without a bound dependency change")
	}
	if !namedPreparedSettingsChanged(entry, []string{"second", "public"}, builtin.ResolveOptions{}) {
		t.Fatal("changed search path retained old analysis")
	}
	if !cachedPreparedInvalidation(errPreparedDependency) {
		t.Fatal("descriptor or statistics invalidation retained cached analysis")
	}
	if cachedPreparedInvalidation(context.Canceled) {
		t.Fatal("cancellation treated as cache invalidation")
	}
}

func TestNamedObjectMetadataAndCleanupInvariants(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}
	base := []bind.ResultColumn{{Name: "value", Layout: layout, Nullable: false}}
	changed := []bind.ResultColumn{{Name: "value", Layout: layout, Nullable: true}}
	if sameResultShape(base, changed) {
		t.Fatal("result nullability change was accepted")
	}

	prepared := &namedPreparedStatement{name: "kept", metadataBytes: 7}
	cursor := &namedCursor{name: "open", metadataBytes: 5}
	transactions := &Transactions{named: sessionNamedObjects{
		prepared:      map[string]*namedPreparedStatement{"kept": prepared},
		cursors:       map[string]*namedCursor{"open": cursor},
		metadataBytes: 12,
	}}
	if err := transactions.closeCursor(bind.CloseCursorCommand{Name: "open"}); err != nil {
		t.Fatal(err)
	}
	if transactions.named.metadataBytes != 7 || len(transactions.named.cursors) != 0 {
		t.Fatalf("cursor cleanup retained state: bytes=%d cursors=%d", transactions.named.metadataBytes, len(transactions.named.cursors))
	}
	if err := transactions.deallocatePrepared(bind.DeallocateCommand{Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	if transactions.named.metadataBytes != 0 || len(transactions.named.prepared) != 0 {
		t.Fatalf("deallocate retained state: bytes=%d statements=%d", transactions.named.metadataBytes, len(transactions.named.prepared))
	}
	err := transactions.deallocatePrepared(bind.DeallocateCommand{Name: "missing"})
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != "26000" {
		t.Fatalf("missing prepared statement error = %v", err)
	}
}
