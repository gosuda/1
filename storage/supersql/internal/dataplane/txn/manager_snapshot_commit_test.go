package txn

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type pausedSnapshotCommit struct {
	storage.Backend
	entered chan struct{}
	release chan struct{}
}

func (b *pausedSnapshotCommit) ApplyMutations(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	close(b.entered)
	select {
	case <-b.release:
		return b.Backend.ApplyMutations(ctx, batch)
	case <-ctx.Done():
		return storage.ApplyResult{}, ctx.Err()
	}
}

func TestStrongSnapshotWaitsForInFlightCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked := &pausedSnapshotCommit{entered: make(chan struct{}), release: make(chan struct{})}
		fixture := newCommitFixture(t, func(backend storage.Backend) storage.Backend {
			blocked.Backend = backend
			return blocked
		})
		writer := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
		if err := writer.ApplyMutations(t.Context(), fixture.batch(2, 73).Mutations); err != nil {
			t.Fatal(err)
		}
		committed := make(chan error, 1)
		go func() {
			_, err := writer.Commit(t.Context())
			committed <- err
		}()
		<-blocked.entered
		type openedSnapshot struct {
			reader storage.SnapshotReader
			err    error
		}
		read := make(chan openedSnapshot, 1)
		go func() {
			reader, err := fixture.manager.StrongSnapshot(t.Context())
			read <- openedSnapshot{reader, err}
		}()
		synctest.Wait()
		var opened openedSnapshot
		var early bool
		select {
		case opened = <-read:
			early = true
			t.Errorf("strong read returned before the in-flight commit settled: %v", opened.err)
		default:
		}
		close(blocked.release)
		if err := <-committed; err != nil {
			t.Error(err)
		}
		if !early {
			opened = <-read
		}
		if opened.reader != nil {
			t.Cleanup(func() {
				if err := opened.reader.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		if opened.err != nil {
			t.Fatal(opened.err)
		}
		fixture.balance(t, opened.reader, 73)
	})
}

func TestTransactionReadsDoNotWaitForInFlightCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked := &pausedSnapshotCommit{entered: make(chan struct{}), release: make(chan struct{})}
		fixture := newCommitFixture(t, func(backend storage.Backend) storage.Backend {
			blocked.Backend = backend
			return blocked
		})
		otherKey := fixture.request
		otherKey.Key = contractKey(2)
		writer := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
		if err := writer.ApplyMutations(t.Context(), []storage.Mutation{
			storage.NewInsertMutation(otherKey.Target, otherKey.Key, otherKey.ColumnIDs, []model.Value{model.Int64Val(200)}),
		}); err != nil {
			t.Fatal(err)
		}
		committed := make(chan error, 1)
		go func() {
			_, err := writer.Commit(t.Context())
			committed <- err
		}()
		<-blocked.entered

		// Both ReadOnly and ReadWrite transactions reading disjoint keys must complete
		// without waiting on the in-flight commit holding the sequencer gate.
		roTx := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, roTx, fixture.request, 100)

		rwTx := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
		contractValues(t, rwTx, fixture.request, 100)
		if err := rwTx.Close(t.Context()); err != nil {
			t.Fatal(err)
		}

		close(blocked.release)
		if err := <-committed; err != nil {
			t.Fatal(err)
		}
		after := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, after, otherKey, 200)
	})
}
