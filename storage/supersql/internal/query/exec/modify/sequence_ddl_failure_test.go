package modify_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

var errSequencePublicationFault = errors.New("sequence publication durability fault")

type sequencePublicationBackend struct {
	storage.Backend
	mu                    sync.Mutex
	failNext, failResolve bool
}

func (b *sequencePublicationBackend) ApplyMutations(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	b.mu.Lock()
	fail := b.failNext
	b.failNext = false
	b.mu.Unlock()
	result, err := b.Backend.ApplyMutations(ctx, batch)
	if err == nil && fail {
		return storage.ApplyResult{}, errSequencePublicationFault
	}
	return result, err
}
func (b *sequencePublicationBackend) ResolveCommit(ctx context.Context, id model.TxnID) (storage.CommitResolution, error) {
	b.mu.Lock()
	fail := b.failResolve
	b.mu.Unlock()
	if fail {
		return storage.CommitResolution{}, errSequencePublicationFault
	}
	return b.Backend.ResolveCommit(ctx, id)
}

func TestSequencePublicationCleanupRemainsOwnedAfterClose(t *testing.T) {
	var backend *sequencePublicationBackend
	f, store, _, _, dependencies := sequenceDDLFixtureWithBackend(t, func(base storage.Backend) storage.Backend {
		backend = &sequencePublicationBackend{Backend: base}
		return backend
	})
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE earlier`); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.failNext = true
	backend.failResolve = true
	backend.mu.Unlock()
	_, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`)
	var pending *schema.PendingSequenceError
	if !errors.As(err, &pending) {
		t.Fatalf("uncertain allocation=%v", err)
	}
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err == nil {
		t.Fatal("admitted command while publication cleanup was unresolved")
	}
	if err := f.session.Close(); err == nil {
		t.Fatal("closed session discarded unresolved allocation ownership")
	}
	backend.mu.Lock()
	backend.failResolve = false
	backend.mu.Unlock()
	if err := f.session.ResolveSequenceDDL(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.session.Close(); err != nil {
		t.Fatal(err)
	}
	view, err := f.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	count := 0
	if err := store.Scan(t.Context(), view, func(schema.SequenceState) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unpublished incarnations retained after resolution: %d", count)
	}
}

func TestSequenceOwnershipChangeDiscardsOnlyCallerLease(t *testing.T) {
	f, _, service, facade, dependencies := sequenceDDLFixture(t)
	other, err := session.NewSequenceSession(session.SequenceSessionOptions{Allocations: service, MaxSequences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := other.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers CACHE 3`); err != nil {
		t.Fatal(err)
	}
	requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{1}})
	requireSequenceDDLRows(t, f, other, dependencies, `SELECT nextval('numbers')`, [][]int64{{4}})
	if _, err := executeSequenceDDL(t, f, dependencies, `ALTER SEQUENCE numbers OWNED BY NONE`); err != nil {
		t.Fatal(err)
	}
	requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{7}})
	requireSequenceDDLRows(t, f, other, dependencies, `SELECT nextval('numbers')`, [][]int64{{5}})
	if _, err := executeSequenceDDL(t, f, dependencies, `ALTER SEQUENCE numbers START WITH 90`); err != nil {
		t.Fatal(err)
	}
	requireSequenceDDLRows(t, f, facade, dependencies, `SELECT currval('numbers'),nextval('numbers')`, [][]int64{{7, 10}})
	requireSequenceDDLRows(t, f, other, dependencies, `SELECT currval('numbers'),nextval('numbers')`, [][]int64{{5, 13}})
}

func TestFailedSequenceDDLOrdersCacheEffects(t *testing.T) {
	for _, scenario := range []struct {
		name, sql, state string
		next             int64
	}{
		{"ownership after cache discard", `ALTER SEQUENCE numbers OWNED BY missing_table.id`, "42P01", 4},
		{"invalid parameters before ownership", `ALTER SEQUENCE numbers INCREMENT 0 OWNED BY missing_table.id`, "22023", 2},
		{"current value before ownership", `ALTER SEQUENCE numbers MAXVALUE 2 OWNED BY missing_table.id`, "22023", 2},
		{"restart before ownership", `ALTER SEQUENCE numbers MAXVALUE 2 RESTART 1 OWNED BY missing_table.id`, "42P01", 4},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, _, _, facade, dependencies := sequenceDDLFixture(t)
			if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers CACHE 3`); err != nil {
				t.Fatal(err)
			}
			requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{1}})
			_, err := executeSequenceDDL(t, f, dependencies, scenario.sql)
			requireUniqueSQLState(t, err, scenario.state)
			requireSequenceDDLRows(t, f, facade, dependencies, `SELECT currval('numbers'),nextval('numbers')`, [][]int64{{1, scenario.next}})
		})
	}
}

func TestSequenceAlterWaitsBeforeCopyingAllocationState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _, _, facade, dependencies := sequenceDDLFixture(t)
		created, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`)
		if err != nil {
			t.Fatal(err)
		}
		requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{1}})
		reader, err := f.store.manager.Begin(t.Context(), f.transactionOptions)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := reader.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
		view, err := reader.ReadView(t.Context(), []txn.ReadDependency{f.catalog.DescriptorDependency(created.Descriptor.ID())})
		if err != nil {
			t.Fatal(err)
		}
		if err := view.Close(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := executeSequenceDDL(t, f, dependencies, `ALTER SEQUENCE numbers START WITH 90`)
			done <- err
		}()
		synctest.Wait()
		metadata, _ := created.Descriptor.Sequence()
		ref := session.SequenceReference{Object: created.Descriptor.ID(), Identity: schema.SequenceIncarnation(metadata.Identity), Incarnation: schema.SequenceIncarnation(metadata.Incarnation)}
		if value, err := facade.Next(t.Context(), ref); err != nil || value != 2 {
			t.Fatalf("reader allocation = %d, %v", value, err)
		}
		if err := reader.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		requireSequenceDDLRows(t, f, facade, dependencies, `SELECT nextval('numbers')`, [][]int64{{3}})
	})
}
