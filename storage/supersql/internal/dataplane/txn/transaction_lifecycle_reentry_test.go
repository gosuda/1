package txn

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestManagedCloseRejectsSynchronousExpressionReentry(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	target := storage.ObjectRef{ObjectID: 2, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	var transaction *Transaction
	policy := contractPolicy(t, target,
		ColumnRule{Layout: storage.ColumnLayout{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, KeyPosition: 1},
		ColumnRule{Layout: storage.ColumnLayout{ID: 2, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, Default: normalizerExpression(func(ctx context.Context, _ []model.Value) (model.Value, error) {
			return model.Value{}, transaction.Close(ctx)
		})},
	)
	transaction = contractBegin(t, fixture.manager, contractOptions(t, policy))
	mutation := storage.NewInsertMutation(target, contractKey(1), []model.ColumnID{1}, []model.Value{model.Int64Val(1)})
	if err := transaction.ApplyMutations(t.Context(), []storage.Mutation{mutation}); !errors.Is(err, errTransactionReentry) {
		t.Fatalf("lifecycle reentry was not rejected: %v", err)
	}
	if transaction.State() != Active {
		t.Fatalf("rejected reentry changed transaction state: %v", transaction.State())
	}
	contractApply(t, transaction, storage.NewInsertMutation(target, contractKey(1), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(1), model.Int64Val(7)}))
	contractCommit(t, transaction)
}
