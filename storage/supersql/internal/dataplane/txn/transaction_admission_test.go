package txn

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestCreatedTargetAdmissionFollowsSavepointLifetime(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	policy, request := contractTable(t)
	transaction := contractBegin(t, fixture.manager, contractOptions(t))
	mutation := storage.Mutation{Target: request.Target, Op: storage.MutationInsert, Key: contractKey(1), ColumnIDs: []model.ColumnID{1}, Values: []model.Value{model.Int64Val(1)}}
	mark, err := transaction.Savepoint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	admit := func(ctx context.Context) error {
		if err := transaction.AdmitCreatedTarget(ctx, policy, 4096); err != nil {
			return err
		}
		return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
	}
	if err := transaction.Statement(t.Context(), admit); err != nil {
		t.Fatal(err)
	}
	request.Key = contractKey(1)
	contractValues(t, transaction, request, 1, 7, 9)
	if err := transaction.RollbackTo(t.Context(), mark); err != nil {
		t.Fatal(err)
	}
	if err := transaction.ApplyMutations(t.Context(), []storage.Mutation{mutation}); !errors.Is(err, storage.ErrInvalidLayout) {
		t.Fatalf("rolled-back admission accepted write: %v", err)
	}
	if err := transaction.Statement(t.Context(), admit); err != nil {
		t.Fatal(err)
	}
	contractValues(t, transaction, request, 1, 7, 9)
	contractCommit(t, transaction)
	reader := contractBegin(t, fixture.manager, contractOptions(t, policy))
	contractValues(t, reader, request, 1, 7, 9)
}

func TestRolledBackPolicyRetentionCanBeReadmitted(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	policy, request := contractTable(t)
	options := contractOptions(t)
	options.MemoryBytes = 8 << 20
	transaction := contractBegin(t, fixture.manager, options)
	mark, err := transaction.Savepoint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	mutation := storage.Mutation{Target: request.Target, Op: storage.MutationInsert, Key: contractKey(1), ColumnIDs: []model.ColumnID{1}, Values: []model.Value{model.Int64Val(1)}}
	for range 8 {
		if err := transaction.Statement(t.Context(), func(ctx context.Context) error {
			if err := transaction.AdmitCreatedTarget(ctx, policy, 512<<10); err != nil {
				return err
			}
			return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
		}); err != nil {
			t.Fatal(err)
		}
		request.Key = contractKey(1)
		contractValues(t, transaction, request, 1, 7, 9)
		if err := transaction.RollbackTo(t.Context(), mark); err != nil {
			t.Fatal(err)
		}
	}
}
