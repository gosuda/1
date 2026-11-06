package txn

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestManagedPrimaryKeyChangeRequiresDeleteAndInsert(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	policy, request := contractTable(t)
	transaction := contractBegin(t, fixture.manager, contractOptions(t, policy))
	request.Key = contractKey(2)
	contractApply(t, transaction, storage.NewInsertMutation(request.Target, request.Key, []model.ColumnID{1}, []model.Value{model.Int64Val(2)}))
	err := transaction.ApplyMutations(t.Context(), []storage.Mutation{storage.NewUpdateMutation(request.Target, request.Key, []model.ColumnID{1}, []model.Value{model.Int64Val(3)})})
	if !errors.Is(err, storage.ErrInvalidMutation) {
		t.Fatalf("in-place primary key change: %v", err)
	}
	contractValues(t, transaction, request, 2, 7, 9)
	contractApply(t, transaction,
		storage.NewDeleteMutation(request.Target, request.Key),
		storage.NewInsertMutation(request.Target, contractKey(3), []model.ColumnID{1}, []model.Value{model.Int64Val(3)}),
	)
	contractAbsent(t, transaction, request)
	request.Key = contractKey(3)
	contractValues(t, transaction, request, 3, 7, 9)
	contractCommit(t, transaction)
	reader := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	contractValues(t, reader, request, 3, 7, 9)
	request.Key = contractKey(2)
	contractAbsent(t, reader, request)
}

func TestManagedExpressionReentryRollsBackWithOuterStatement(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	failure := errors.New("generated value rejected")
	target := storage.ObjectRef{ObjectID: 2, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	var transaction *Transaction
	policy := contractPolicy(t, target,
		ColumnRule{Layout: storage.ColumnLayout{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, KeyPosition: 1},
		ColumnRule{Layout: storage.ColumnLayout{ID: 2, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, Default: normalizerExpression(func(ctx context.Context, _ []model.Value) (model.Value, error) {
			row, err := transaction.Get(ctx, fixture.request)
			if err != nil {
				return model.Value{}, err
			}
			nested := storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(row.Values[0].IntVal - 10)})
			if err := transaction.ApplyMutations(ctx, []storage.Mutation{nested}); err != nil {
				return model.Value{}, err
			}
			return model.Int64Val(7), nil
		})},
		ColumnRule{Layout: storage.ColumnLayout{ID: 3, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, Generated: normalizerExpression(func(context.Context, []model.Value) (model.Value, error) { return model.Value{}, failure })},
	)
	balancePolicy := contractPolicy(t, fixture.request.Target, ColumnRule{Layout: fixture.request.Layout[0]})
	transaction = contractBegin(t, fixture.manager, contractOptions(t, policy, balancePolicy))
	frozen := contractView(t, transaction, fixture.request.Target, contractSpan(1, 2))
	mutation := storage.NewInsertMutation(target, contractKey(5), []model.ColumnID{1}, []model.Value{model.Int64Val(5)})
	if err := transaction.ApplyMutations(t.Context(), []storage.Mutation{mutation}); !errors.Is(err, failure) {
		t.Fatalf("nested default failure: %v", err)
	}
	contractValues(t, transaction, fixture.request, 100)
	contractValues(t, frozen, fixture.request, 100)
	absent := storage.ReadRequest{Target: target, Key: contractKey(5), ColumnIDs: []model.ColumnID{1, 2, 3}, Layout: policy.Layout().Columns}
	contractAbsent(t, transaction, absent)
	contractApply(t, transaction, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(95)}))
	contractCommit(t, transaction)
	reader := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	contractValues(t, reader, fixture.request, 95)
	contractAbsent(t, reader, absent)
}
