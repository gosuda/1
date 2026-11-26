package modify_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func prepareProvisionalSQL(t *testing.T, f *sessionExecutionFixture, dependencies []txn.ReadDependency, sql string) *session.Prepared {
	t.Helper()
	view, err := f.session.ReadView(t.Context(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		t.Fatal(errors.Join(err, view.Close()))
	}
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := session.Prepare(t.Context(), parsed.Stmts[0], snapshot, f.planning(sql), nil)
	if err = errors.Join(err, snapshot.Close(), view.Close()); err != nil {
		t.Fatal(err)
	}
	return prepared
}

func runProvisionalSQL(t *testing.T, f *sessionExecutionFixture, owner *session.Transactions, dependencies []txn.ReadDependency, prepared *session.Prepared) ([][]int64, error) {
	t.Helper()
	invocation := mergeInvocation(t, f)
	invocation.Dependencies = dependencies
	var result [][]int64
	_, err := owner.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		var err error
		result, err = collectWritableRows(ctx, rows)
		return err
	})
	return result, err
}

func TestPreparedProvisionalRejectsRecreatedCatalogIdentity(t *testing.T) {
	f, _, _, _, dependencies := sequenceDDLFixture(t)
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	if err := f.session.Savepoint(t.Context(), "before_create"); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`); err != nil {
		t.Fatal(err)
	}
	prepared := prepareProvisionalSQL(t, f, dependencies, `SELECT nextval('numbers')`)
	if err := f.session.RollbackTo(t.Context(), "before_create"); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`); err != nil {
		t.Fatal(err)
	}
	replacement := prepareProvisionalSQL(t, f, dependencies, `SELECT nextval('numbers')`)
	if !reflect.DeepEqual(prepared.Dependencies(), replacement.Dependencies()) {
		t.Fatal("reproduction did not reuse descriptor IDs and versions")
	}
	// No runtime is supplied: publication refusal must precede expression setup.
	_, err := prepared.Open(t.Context(), f.invocation(t).Environment)
	requireUniqueSQLState(t, err, "0A000")
	_, err = runProvisionalSQL(t, f, f.session, dependencies, prepared)
	requireUniqueSQLState(t, err, "0A000")
}

func TestPreparedEarlierProvisionalSurvivesLaterRollbackAndCommit(t *testing.T) {
	f, _, _, _, dependencies := sequenceDDLFixture(t)
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`); err != nil {
		t.Fatal(err)
	}
	if err := f.session.Savepoint(t.Context(), "after_create"); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE discarded`); err != nil {
		t.Fatal(err)
	}
	prepared := prepareProvisionalSQL(t, f, dependencies, `SELECT 'numbers'::regclass`)
	before, err := runProvisionalSQL(t, f, f.session, dependencies, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.session.RollbackTo(t.Context(), "after_create"); err != nil {
		t.Fatal(err)
	}
	after, err := runProvisionalSQL(t, f, f.session, dependencies, prepared)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("surviving binding: %v, %v; want %v", after, err, before)
	}
	if _, err := f.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	after, err = runProvisionalSQL(t, f, f.peer(t), dependencies, prepared)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("committed binding: %v, %v; want %v", after, err, before)
	}
}

func TestPreparedUnpublishedCatalogRejectsOtherSessionAndAbort(t *testing.T) {
	f, _, _, _, dependencies := sequenceDDLFixture(t)
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`); err != nil {
		t.Fatal(err)
	}
	prepared := prepareProvisionalSQL(t, f, dependencies, `SELECT nextval('numbers')`)
	peer := f.peer(t)
	_, err := runProvisionalSQL(t, f, peer, dependencies, prepared)
	requireUniqueSQLState(t, err, "0A000")
	if err := f.session.Rollback(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	_, err = runProvisionalSQL(t, f, peer, dependencies, prepared)
	requireUniqueSQLState(t, err, "0A000")
}

func TestPreparedProvisionalFencesUnknownCommitUntilResolution(t *testing.T) {
	var backend *sequencePublicationBackend
	f, _, _, _, dependencies := sequenceDDLFixtureWithBackend(t, func(base storage.Backend) storage.Backend {
		backend = &sequencePublicationBackend{Backend: base}
		return backend
	})
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSequenceDDL(t, f, dependencies, `CREATE SEQUENCE numbers`); err != nil {
		t.Fatal(err)
	}
	prepared := prepareProvisionalSQL(t, f, dependencies, `SELECT 'numbers'::regclass`)
	before, err := runProvisionalSQL(t, f, f.session, dependencies, prepared)
	if err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.failNext, backend.failResolve = true, true
	backend.mu.Unlock()
	_, err = f.session.Commit(t.Context(), false)
	if !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("commit decision = %v", err)
	}
	_, err = prepared.Open(t.Context(), f.invocation(t).Environment)
	requireUniqueSQLState(t, err, "0A000")
	peer := f.peer(t)
	_, err = runProvisionalSQL(t, f, peer, dependencies, prepared)
	requireUniqueSQLState(t, err, "0A000")
	backend.mu.Lock()
	backend.failResolve = false
	backend.mu.Unlock()
	resolution, err := f.session.Resolve(t.Context())
	if err != nil || resolution.State != storage.CommitCommitted {
		t.Fatalf("resolution = %+v, %v", resolution, err)
	}
	rows, err := runProvisionalSQL(t, f, peer, dependencies, prepared)
	if err != nil || !reflect.DeepEqual(rows, before) {
		t.Fatalf("resolved binding = %v, %v; want %v", rows, err, before)
	}
}
