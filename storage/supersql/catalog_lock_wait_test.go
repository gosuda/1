package supersql

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// catalogLockBound bounds every statement that may wait on another session's
// catalog locks. A regression then reports a SQLSTATE instead of hanging the suite.
const catalogLockBound = 5 * time.Second

// catalogLockExec runs one statement under catalogLockBound and returns its
// SQLSTATE (empty on success) and elapsed time. A non-SQL error fails the test.
func catalogLockExec(t *testing.T, conn *sql.Conn, query string) (string, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), catalogLockBound)
	defer cancel()
	start := time.Now()
	_, err := conn.ExecContext(ctx, query)
	elapsed := time.Since(start)
	if err == nil {
		return "", elapsed
	}
	var modelErr *model.Error
	if !errors.As(err, &modelErr) {
		t.Fatalf("%q: non-SQL error: %v", query, err)
	}
	return modelErr.Code, elapsed
}

// catalogLockMust runs a statement that must succeed without waiting.
func catalogLockMust(t *testing.T, conn *sql.Conn, query string) {
	t.Helper()
	if code, _ := catalogLockExec(t, conn, query); code != "" {
		t.Fatalf("%q: SQLSTATE %s", query, code)
	}
}

// requireCatalogLockWait requires a blocked statement to fail with want before
// the bound, so a timeout that never fires cannot pass as a slow success.
func requireCatalogLockWait(t *testing.T, label, query string, code string, elapsed time.Duration, want string) {
	t.Helper()
	if code != want || elapsed >= catalogLockBound {
		t.Fatalf("%s: %q SQLSTATE %q after %s, want %s before %s", label, query, code, elapsed, want, catalogLockBound)
	}
}

// TestCatalogLockWaitBoundedByLockTimeout pins the forward direction: an
// autocommit read is an implicit read-only snapshot read and never waits on
// another session's uncommitted catalog DDL (the uncommitted type is simply
// invisible), while statements that take catalog locks (explicit transaction
// blocks, writes) wait, lock_timeout bounds that wait with 55P03, and the same
// statement succeeds once the DDL ends.
func TestCatalogLockWaitBoundedByLockTimeout(t *testing.T) {
	pool, _ := openTestDB(t)
	writer, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	catalogLockMust(t, reader, "SET lock_timeout = '1s'")
	catalogLockMust(t, writer, "CREATE TABLE base (id bigint PRIMARY KEY)")
	catalogLockMust(t, writer, "BEGIN")
	catalogLockMust(t, writer, "CREATE TYPE t AS ENUM ('s')")
	for query, want := range map[string]string{"SELECT 1": "", "SELECT 's'::t::text": "42704"} {
		if code, elapsed := catalogLockExec(t, reader, query); code != want || elapsed >= time.Second {
			t.Fatalf("autocommit read behind uncommitted DDL: %q SQLSTATE %q after %s, want %q without waiting", query, code, elapsed, want)
		}
	}
	catalogLockMust(t, reader, "BEGIN")
	code, elapsed := catalogLockExec(t, reader, "SELECT 1")
	requireCatalogLockWait(t, "transaction block behind uncommitted DDL", "SELECT 1", code, elapsed, "55P03")
	catalogLockMust(t, reader, "ROLLBACK")
	code, elapsed = catalogLockExec(t, reader, "INSERT INTO base VALUES (1)")
	requireCatalogLockWait(t, "write behind uncommitted DDL", "INSERT INTO base VALUES (1)", code, elapsed, "55P03")
	catalogLockMust(t, writer, "ROLLBACK")
	catalogLockMust(t, reader, "INSERT INTO base VALUES (1)")
	catalogLockMust(t, reader, "BEGIN")
	catalogLockMust(t, reader, "SELECT 1")
	catalogLockMust(t, reader, "COMMIT")
}

// TestCatalogLockWaitBoundedByStatementTimeout pins the reverse direction with
// the statement-level bound: an idle transaction's catalog locks make another
// session's DDL wait, and statement_timeout bounds that DDL with 57014.
func TestCatalogLockWaitBoundedByStatementTimeout(t *testing.T) {
	pool, _ := openTestDB(t)
	idle, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	ddl, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer ddl.Close()

	catalogLockMust(t, ddl, "SET statement_timeout = '1s'")
	catalogLockMust(t, idle, "BEGIN")
	catalogLockMust(t, idle, "SELECT 1")
	const query = "CREATE TYPE u AS ENUM ('s')"
	code, elapsed := catalogLockExec(t, ddl, query)
	requireCatalogLockWait(t, "behind idle transaction", query, code, elapsed, "57014")
	catalogLockMust(t, idle, "ROLLBACK")
}

// TestCatalogLockWaitDDLBoundedByLockTimeout is the regression for lock_timeout
// on plain-Exec DDL: DDL that waits on an idle transaction's catalog locks fails
// with 55P03 under lock_timeout, and the same DDL succeeds once that transaction ends.
func TestCatalogLockWaitDDLBoundedByLockTimeout(t *testing.T) {
	pool, _ := openTestDB(t)
	idle, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	ddl, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer ddl.Close()

	catalogLockMust(t, ddl, "SET lock_timeout = '1s'")
	catalogLockMust(t, idle, "BEGIN")
	catalogLockMust(t, idle, "SELECT 1")
	const query = "CREATE TYPE u AS ENUM ('s')"
	code, elapsed := catalogLockExec(t, ddl, query)
	requireCatalogLockWait(t, "DDL behind idle transaction", query, code, elapsed, "55P03")
	catalogLockMust(t, idle, "ROLLBACK")
	catalogLockMust(t, ddl, query)
	catalogLockMust(t, ddl, "SELECT 's'::u::text")
}
