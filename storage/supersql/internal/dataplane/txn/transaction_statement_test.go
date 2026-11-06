package txn

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func statementBalance(fixture commitFixture, value int64) storage.Mutation {
	return storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(value)})
}

func TestManagedStatementRestoresEarlierStatement(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%v", canceled), func(t *testing.T) {
			fixture := newCommitFixture(t, nil)
			policy, inserted := contractTable(t)
			options := contractBalanceOptions(t, fixture)
			options.Resolver = contractResolver{normalizers: append(options.Resolver.(contractResolver).normalizers, policy)}
			transaction := contractBegin(t, fixture.manager, options)
			if err := transaction.Statement(t.Context(), func(ctx context.Context) error {
				return transaction.ApplyMutations(ctx, []storage.Mutation{statementBalance(fixture, 90)})
			}); err != nil {
				t.Fatal(err)
			}
			retained := contractView(t, transaction, fixture.request.Target, contractSpan(1, 2))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("later row failed")
			var discarded ReadView
			inserted.Key = contractKey(2)
			err := transaction.Statement(ctx, func(ctx context.Context) error {
				if err := transaction.ApplyMutations(ctx, []storage.Mutation{statementBalance(fixture, 80)}); err != nil {
					return err
				}
				row, err := transaction.Get(ctx, fixture.request)
				if err != nil {
					return err
				}
				if len(row.Values) != 1 || row.Values[0].IntVal != 80 {
					return fmt.Errorf("statement read = %v, want 80", row.Values)
				}
				discarded, err = transaction.ReadView(ctx, []ReadDependency{{Target: fixture.request.Target, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{contractSpan(1, 2)}}})
				if err != nil {
					return err
				}
				if err := transaction.ApplyMutations(ctx, []storage.Mutation{storage.NewInsertMutation(inserted.Target, inserted.Key, []model.ColumnID{1}, []model.Value{model.Int64Val(2)})}); err != nil {
					return err
				}
				if canceled {
					cancel()
					return nil
				}
				return failure
			})
			want := failure
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("statement error = %v, want %v", err, want)
			}
			if transaction.State() != Active {
				t.Fatalf("statement rollback state = %v, want Active", transaction.State())
			}
			if discarded == nil {
				t.Fatal("callback did not reach its read view")
			}
			if _, err := discarded.Get(t.Context(), fixture.request); !errors.Is(err, storage.ErrClosed) {
				t.Fatalf("discarded statement view = %v, want closed", err)
			}
			contractValues(t, retained, fixture.request, 90)
			contractValues(t, transaction, fixture.request, 90)
			contractAbsent(t, transaction, inserted)
			if len(transaction.targets) != 1 {
				t.Fatalf("retained target admissions = %d, want 1", len(transaction.targets))
			}
			contractCommit(t, transaction)
			current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
			contractValues(t, current, fixture.request, 90)
			contractAbsent(t, current, inserted)
		})
	}
}

func TestManagedStatementRejectsLifecycleReentry(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	mark, err := transaction.Savepoint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Statement(t.Context(), nil); !errors.Is(err, errTransactionStatement) {
		t.Fatalf("nil callback = %v", err)
	}
	var escaped context.Context
	err = transaction.Statement(t.Context(), func(ctx context.Context) error {
		escaped = ctx
		operations := []func() error{
			func() error { _, err := transaction.Commit(ctx); return err },
			func() error { _, err := transaction.Resolve(ctx); return err },
			func() error { return transaction.Rollback(ctx) },
			func() error { return transaction.Close(ctx) },
			func() error { _, err := transaction.Savepoint(ctx); return err },
			func() error { return transaction.RollbackTo(ctx, mark) },
			func() error { return transaction.Statement(ctx, func(context.Context) error { return nil }) },
		}
		for i, operation := range operations {
			if err := operation(); !errors.Is(err, errTransactionReentry) {
				return fmt.Errorf("lifecycle operation %d = %w, want reentry rejection", i, err)
			}
		}
		return transaction.ApplyMutations(ctx, []storage.Mutation{statementBalance(fixture, 90)})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.ApplyMutations(escaped, []storage.Mutation{statementBalance(fixture, 80)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired callback token = %v, want canceled", err)
	}
	contractCommit(t, transaction)
	current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	contractValues(t, current, fixture.request, 90)
}

func TestManagedStatementsSerializeRollbackBeforeNextCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
		entered, release := make(chan struct{}), make(chan struct{})
		first, second := make(chan error, 1), make(chan error, 1)
		failure := errors.New("first statement failed")
		go func() {
			first <- transaction.Statement(t.Context(), func(ctx context.Context) error {
				if err := transaction.ApplyMutations(ctx, []storage.Mutation{statementBalance(fixture, 90)}); err != nil {
					return err
				}
				close(entered)
				<-release
				return failure
			})
		}()
		<-entered
		secondEntered := make(chan struct{})
		go func() {
			second <- transaction.Statement(t.Context(), func(ctx context.Context) error {
				close(secondEntered)
				row, err := transaction.Get(ctx, fixture.request)
				if err != nil {
					return err
				}
				if len(row.Values) != 1 || row.Values[0].IntVal != 100 {
					return fmt.Errorf("second statement observed unrolled write: %v", row.Values)
				}
				return transaction.ApplyMutations(ctx, []storage.Mutation{statementBalance(fixture, 80)})
			})
		}()
		synctest.Wait()
		select {
		case <-secondEntered:
			t.Error("second callback entered before first statement finished")
		default:
		}
		close(release)
		if err := <-first; !errors.Is(err, failure) {
			t.Fatalf("first statement = %v", err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		contractCommit(t, transaction)
		current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, current, fixture.request, 80)
	})
}

func TestManagedStatementWoundAbortsBeforeRequesterProceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		options := contractBalanceOptions(t, fixture)
		older := contractBegin(t, fixture.manager, options)
		younger := contractBegin(t, fixture.manager, options)
		entered := make(chan struct{})
		done := make(chan error, 1)
		var view ReadView
		go func() {
			done <- younger.Statement(t.Context(), func(ctx context.Context) error {
				if err := younger.ApplyMutations(ctx, []storage.Mutation{statementBalance(fixture, 90)}); err != nil {
					return err
				}
				var err error
				view, err = younger.ReadView(ctx, []ReadDependency{{Target: fixture.request.Target, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{contractSpan(1, 2)}}})
				if err != nil {
					return err
				}
				close(entered)
				<-ctx.Done()
				return nil
			})
		}()
		<-entered
		contractApply(t, older, statementBalance(fixture, 80))
		if err := <-done; !errors.Is(err, errLockWounded) {
			t.Fatalf("wounded statement = %v", err)
		}
		if younger.State() != Aborted || younger.resources.Load() != 0 {
			t.Fatalf("wound retained ownership: state=%v resources=%d", younger.State(), younger.resources.Load())
		}
		if _, err := view.Get(t.Context(), fixture.request); !errors.Is(err, storage.ErrClosed) {
			t.Fatalf("wounded view = %v, want closed", err)
		}
		contractCommit(t, older)
		current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, current, fixture.request, 80)
	})
}

func TestManagedStatementPanicClosesTransactionBeforeRethrow(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	contractApply(t, transaction, statementBalance(fixture, 90))
	var view ReadView
	failure := errors.New("callback panic")
	func() {
		defer func() {
			if value := recover(); value != failure {
				t.Errorf("panic = %v, want original value", value)
			}
			if transaction.State() != Aborted || transaction.resources.Load() != 0 {
				t.Errorf("panic escaped before cleanup: state=%v resources=%d", transaction.State(), transaction.resources.Load())
			}
		}()
		_ = transaction.Statement(t.Context(), func(ctx context.Context) error {
			if err := transaction.ApplyMutations(ctx, []storage.Mutation{statementBalance(fixture, 80)}); err != nil {
				t.Fatal(err)
			}
			var err error
			view, err = transaction.ReadView(ctx, []ReadDependency{{Target: fixture.request.Target, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{contractSpan(1, 2)}}})
			if err != nil {
				t.Fatal(err)
			}
			panic(failure)
		})
	}()
	if _, err := transaction.Commit(t.Context()); !errors.Is(err, errTransactionInactive) {
		t.Fatalf("commit after recovered panic = %v", err)
	}
	if view == nil {
		t.Fatal("callback did not acquire view")
	}
	if _, err := view.Get(t.Context(), fixture.request); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("panic retained view: %v", err)
	}
	current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	contractValues(t, current, fixture.request, 100)
	writer := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	contractApply(t, writer, statementBalance(fixture, 70))
	contractCommit(t, writer)
}

func TestManagedSuccessfulStatementsDoNotRetainSavepoints(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	for range 4096 {
		if err := transaction.Statement(t.Context(), func(context.Context) error { return nil }); err != nil {
			t.Fatalf("empty statement exhausted transaction resources: %v", err)
		}
	}
	if len(transaction.marks) != 0 {
		t.Fatalf("statements retained savepoints: count=%d", len(transaction.marks))
	}
	if err := transaction.Statement(t.Context(), func(ctx context.Context) error {
		return transaction.ApplyMutations(ctx, []storage.Mutation{statementBalance(fixture, 80)})
	}); err != nil {
		t.Fatal(err)
	}
	contractCommit(t, transaction)
	current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	contractValues(t, current, fixture.request, 80)
}

type statementCloseFailure struct {
	storage.SnapshotReader
	failure error
}

func (reader statementCloseFailure) Close() error {
	return errors.Join(reader.SnapshotReader.Close(), reader.failure)
}

func TestManagedStatementCleanupFailureFencesOwnership(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic=%v", panics), func(t *testing.T) {
			fixture := newCommitFixture(t, nil)
			manager, err := NewManager(t.Context(), ManagerConfig{Backend: fixture.backend, Clock: fixture.clock, Authority: fixture.manager.authority, Recovered: fixture.manager.frontier, MaxOperations: 8})
			if err != nil {
				t.Fatal(err)
			}
			cleanupFailure := errors.New("snapshot close failed")
			t.Cleanup(func() {
				if err := manager.Close(); !errors.Is(err, cleanupFailure) {
					t.Errorf("manager cleanup lost failure: %v", err)
				}
			})
			transaction, err := manager.Begin(t.Context(), contractBalanceOptions(t, fixture))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := transaction.Close(t.Context()); !errors.Is(err, cleanupFailure) {
					t.Errorf("transaction cleanup lost failure: %v", err)
				}
			})
			failure := errors.New("statement failed")
			func() {
				defer func() {
					value := recover()
					wrongPanic := panics && value != failure
					unexpectedPanic := !panics && value != nil
					if wrongPanic || unexpectedPanic {
						t.Errorf("unexpected panic value: %v", value)
					}
				}()
				err = transaction.Statement(t.Context(), func(ctx context.Context) error {
					if err := transaction.ApplyMutations(ctx, []storage.Mutation{statementBalance(fixture, 90)}); err != nil {
						return err
					}
					view, err := transaction.ReadView(ctx, []ReadDependency{{Target: fixture.request.Target, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{contractSpan(1, 2)}}})
					if err != nil {
						return err
					}
					owned := view.(*transactionView)
					owned.reader = statementCloseFailure{SnapshotReader: owned.reader, failure: cleanupFailure}
					if panics {
						panic(failure)
					}
					return failure
				})
			}()
			if !panics && (!errors.Is(err, failure) || !errors.Is(err, cleanupFailure)) {
				t.Fatalf("statement lost error causes: %v", err)
			}
			if transaction.State() != Aborted {
				t.Fatalf("cleanup failure state = %v, want Aborted", transaction.State())
			}
			if _, err := manager.Begin(t.Context(), TransactionOptions{Mode: ReadOnly}); !errors.Is(err, cleanupFailure) {
				t.Fatalf("cleanup failure did not fence admission: %v", err)
			}
			current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
			contractValues(t, current, fixture.request, 100)
		})
	}
}

func TestManagedStatementRollbackRetainsOnlyEarlierLocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		options := contractBalanceOptions(t, fixture)
		transaction := contractBegin(t, fixture.manager, options)
		write := func(key int64) storage.Mutation {
			return storage.NewUpsertMutation(fixture.request.Target, contractKey(key), fixture.request.ColumnIDs, []model.Value{model.Int64Val(key * 10)})
		}
		contractValues(t, transaction, fixture.request, 100)
		contractApply(t, transaction, write(2))
		failure := errors.New("discard new locks")
		err := transaction.Statement(t.Context(), func(ctx context.Context) error {
			request := fixture.request
			request.Key = contractKey(3)
			if _, err := transaction.Get(ctx, request); !errors.Is(err, storage.ErrRowNotFound) {
				return fmt.Errorf("new point read = %v, want absent", err)
			}
			if err := transaction.ApplyMutations(ctx, []storage.Mutation{write(4)}); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		later := contractBegin(t, fixture.manager, options)
		contractApply(t, later, write(3), write(4))
		contractCommit(t, later)
		readCompetitor := contractBegin(t, fixture.manager, options)
		writeCompetitor := contractBegin(t, fixture.manager, options)
		readDone, writeDone := make(chan error, 1), make(chan error, 1)
		go func() { readDone <- readCompetitor.ApplyMutations(t.Context(), []storage.Mutation{write(1)}) }()
		go func() { writeDone <- writeCompetitor.ApplyMutations(t.Context(), []storage.Mutation{write(2)}) }()
		synctest.Wait()
		for _, pending := range []<-chan error{readDone, writeDone} {
			if len(pending) != 0 {
				t.Error("statement rollback released an earlier lock")
			}
		}
		if err := transaction.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-readDone; err != nil {
			t.Fatal(err)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		contractCommit(t, readCompetitor)
		contractCommit(t, writeCompetitor)
	})
}
