package txn

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestPublicationBindingStatementFailureCannotAdoptReplacement(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	failure := errors.New("reject statement after binding")
	var abandoned *model.ProvisionalBinding
	err := transaction.Statement(t.Context(), func(ctx context.Context) error {
		mutation := storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)})
		if err := transaction.ApplyMutations(ctx, []storage.Mutation{mutation}); err != nil {
			return err
		}
		view, err := transaction.ReadView(ctx, []ReadDependency{{Target: fixture.request.Target, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{contractSpan(1, 2)}}})
		if err != nil {
			return err
		}
		row, err := view.Get(ctx, fixture.request)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		abandoned, err = view.(*transactionView).ProvisionalBinding(row.Ordinal)
		return errors.Join(failure, err, view.Close())
	})
	if !errors.Is(err, failure) || abandoned == nil {
		t.Fatalf("statement = %v, binding = %v", err, abandoned)
	}
	contractApply(t, transaction, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
	contractCommit(t, transaction)
	if abandoned.Usable(transaction.PublicationOwner()) || abandoned.Usable(nil) {
		t.Fatal("abandoned statement binding adopted committed replacement")
	}
}

func TestRetainedPublicationBindingDoesNotPinTransactionSnapshot(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	contractApply(t, transaction, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
	view := contractView(t, transaction, fixture.request.Target, contractSpan(1, 2))
	row, err := view.Get(t.Context(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := view.(*transactionView).ProvisionalBinding(row.Ordinal)
	if err != nil {
		t.Fatal(err)
	}
	committed := contractCommit(t, transaction)
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: committed.CommitTS}); err != nil {
		t.Fatalf("retained binding pinned snapshot: %v", err)
	}
	if !binding.Usable(nil) {
		t.Fatal("known commit did not publish retained binding")
	}
	runtime.KeepAlive(binding)
}
