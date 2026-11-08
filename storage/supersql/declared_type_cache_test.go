package supersql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// declaredTypeTestSession opens one database and one pinned connection, so every
// statement in a test runs on the same session. The Catalog is returned so the
// catalog read probes are reachable.
func declaredTypeTestSession(t *testing.T) (*sql.DB, *Catalog, *sql.Conn) {
	t.Helper()
	pool, connector := openTestDB(t)
	conn, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pool, connector.database, conn
}

// TestDeclaredTypeRuntimeIsNotRebuiltPerStatement is the regression test for the
// per-statement catalog cost of a no-DDL statement. It asserts, with exact
// counters and no timing, that a no-DDL statement rebuilds zero declared-type
// runtimes and re-reads a bounded number of catalog rows after 40 tables and 100
// user enums exist.
//
// It opens its own connector instead of using openTestDB so that it can assert
// the close error of a session this long directly.
func TestDeclaredTypeRuntimeIsNotRebuiltPerStatement(t *testing.T) {
	ctx := t.Context()
	connector, err := OpenConnector(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	pool := sql.OpenDB(connector)
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		if err := pool.Close(); err != nil {
			t.Errorf("pool close after long session: %v", err)
		}
	})
	database := connector.database

	exec := func(statement string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%q: %v", statement, err)
		}
	}
	// declaredScans reads the session's exact declared-type rebuild counter
	// through the pinned driver connection.
	declaredScans := func() uint64 {
		t.Helper()
		var scans uint64
		if err := conn.Raw(func(driverConn any) error {
			scans = driverConn.(*connection).scope.txns.DeclaredTypeScans()
			return nil
		}); err != nil {
			t.Fatalf("raw: %v", err)
		}
		return scans
	}
	perStatement := func() (reads, rebuilds uint64) {
		t.Helper()
		exec("SELECT 1")
		beforeReads, beforeRebuilds := database.catalog.ReadCount(), declaredScans()
		const statements = 5
		for i := range statements {
			exec(fmt.Sprintf("SELECT %d", i))
		}
		return (database.catalog.ReadCount() - beforeReads) / statements,
			(declaredScans() - beforeRebuilds) / statements
	}

	emptyReads, _ := perStatement()
	for i := range 40 {
		exec(fmt.Sprintf("CREATE TABLE cache_table_%d (id INT PRIMARY KEY, name VARCHAR, n INT)", i))
	}
	for i := range 100 {
		exec(fmt.Sprintf("CREATE TYPE cache_enum_%d AS ENUM ('a', 'b')", i))
	}
	loadedReads, loadedRebuilds := perStatement()
	t.Logf("catalog reads per no-DDL statement: empty=%d loaded=%d; declared-type rebuilds per statement loaded=%d", emptyReads, loadedReads, loadedRebuilds)

	// The declared-type runtime is cached by catalog content, so no-DDL
	// statements must not rebuild the declared types even with 100 of them.
	if loadedRebuilds != 0 {
		t.Fatalf("no-DDL statements rebuilt declared types: %d per statement with 100 enums", loadedRebuilds)
	}
	// Every O(catalog) read a no-DDL statement performs is a pure function of the
	// catalog content it reads, so all of them are served from the session's
	// caches: the declared-type definition scan, the declared-type routine
	// discovery, and the catalog dependency discovery walk. A loaded catalog may
	// cost a bounded constant more than an empty one (a cached entry is keyed by
	// content, so the first statement after a change pays for it, and the probe
	// that names the content reads nothing but still opens a view). It must not
	// grow with the catalog.
	const allowance = 16
	if loadedReads > emptyReads+allowance {
		t.Fatalf("catalog reads per no-DDL statement grew with the catalog: %d with 40 tables + 100 enums, empty %d, allowance %d", loadedReads, emptyReads, allowance)
	}
}

// TestDeclaredTypeRuntimeTracksCatalogWrites pins the other half of the cache
// contract: a type created in an open transaction is visible to later statements
// in it, a rolled-back type is not, and a rollback to a savepoint keeps the work
// that preceded it.
func TestDeclaredTypeRuntimeTracksCatalogWrites(t *testing.T) {
	ctx := t.Context()
	_, _, conn := declaredTypeTestSession(t)

	exec := func(statement string) error {
		t.Helper()
		_, err := conn.ExecContext(ctx, statement)
		return err
	}
	mustExec := func(statement string) {
		t.Helper()
		if err := exec(statement); err != nil {
			t.Fatalf("%q: %v", statement, err)
		}
	}

	mustExec("CREATE TYPE visible_enum AS ENUM ('x')")
	mustExec("SELECT 'x'::visible_enum::text")
	if err := exec("SELECT 'x'::missing_enum::text"); err == nil {
		t.Fatal("unknown type resolved before it was created")
	}

	mustExec("BEGIN")
	mustExec("CREATE TYPE in_txn_enum AS ENUM ('y')")
	mustExec("SELECT 'y'::in_txn_enum::text")
	mustExec("ROLLBACK")
	if err := exec("SELECT 'y'::in_txn_enum::text"); err == nil {
		t.Fatal("type created in a rolled-back transaction is still visible")
	}

	mustExec("BEGIN")
	mustExec("CREATE TYPE kept_enum AS ENUM ('k')")
	mustExec("SAVEPOINT before_discard")
	mustExec("CREATE TYPE discarded_enum AS ENUM ('d')")
	mustExec("ROLLBACK TO SAVEPOINT before_discard")
	mustExec("SELECT 'k'::kept_enum::text")
	// An error aborts the transaction (PostgreSQL semantics), so probe the
	// discarded type inside a savepoint that is rolled back to recover.
	mustExec("SAVEPOINT probe")
	if err := exec("SELECT 'd'::discarded_enum::text"); err == nil {
		t.Fatal("type created after a savepoint survived rollback to it")
	}
	mustExec("ROLLBACK TO SAVEPOINT probe")
	mustExec("COMMIT")
	mustExec("SELECT 'k'::kept_enum::text")
}

// TestDeclaredTypeRuntimeIsPerSession proves the cache never serves one session
// a stale definition set: DDL committed by one session is visible to another
// session's very next statement. It never holds a DDL transaction open across
// the other session, so no statement can block on a lock.
func TestDeclaredTypeRuntimeIsPerSession(t *testing.T) {
	ctx := t.Context()
	pool, _, first := declaredTypeTestSession(t)
	second, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("second conn: %v", err)
	}
	defer second.Close()

	// The second session warms its declared-type cache before the change.
	if _, err := second.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, err := first.ExecContext(ctx, "CREATE TYPE session_enum AS ENUM ('s')"); err != nil {
		t.Fatalf("CREATE TYPE: %v", err)
	}
	if _, err := second.ExecContext(ctx, "SELECT 's'::session_enum::text"); err != nil {
		t.Fatalf("committed type not visible to the other session's next statement: %v", err)
	}
	if _, err := first.ExecContext(ctx, "ALTER TYPE session_enum ADD VALUE 'two'"); err != nil {
		t.Fatalf("ALTER TYPE: %v", err)
	}
	if _, err := second.ExecContext(ctx, "SELECT 'two'::session_enum::text"); err != nil {
		t.Fatalf("ALTER visible to other session: %v", err)
	}
}

// TestDeclaredTypeRuntimeDropAfterCachedState proves a cached definition set
// never outlives the catalog change that invalidates it: ALTER TYPE ADD VALUE
// and DROP TYPE issued after the definitions were cached are visible to the
// very next statement.
func TestDeclaredTypeRuntimeDropAfterCachedState(t *testing.T) {
	ctx := t.Context()
	_, _, conn := declaredTypeTestSession(t)

	exec := func(statement string) error {
		t.Helper()
		_, err := conn.ExecContext(ctx, statement)
		return err
	}
	mustExec := func(statement string) {
		t.Helper()
		if err := exec(statement); err != nil {
			t.Fatalf("%q: %v", statement, err)
		}
	}
	mustExec("CREATE TYPE mutable_enum AS ENUM ('one')")
	mustExec("SELECT 'one'::mutable_enum::text")
	// A no-DDL statement in between makes the next change hit cached state.
	mustExec("SELECT 1")

	mustExec("ALTER TYPE mutable_enum ADD VALUE 'two'")
	mustExec("SELECT 'two'::mutable_enum::text")

	mustExec("DROP TYPE mutable_enum")
	err := exec("SELECT 'one'::mutable_enum::text")
	if err == nil {
		t.Fatal("dropped type still resolves from cached definitions")
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != "42704" {
		t.Fatalf("reference to dropped type: got %v, want SQLSTATE 42704", err)
	}
}

// discoveryTestSession runs a session's statements through one pinned
// connection and fails the test on the first unexpected error.
func discoveryTestSession(t *testing.T, conn *sql.Conn) (exec func(string) error, mustExec func(string)) {
	t.Helper()
	ctx := t.Context()
	exec = func(statement string) error {
		t.Helper()
		_, err := conn.ExecContext(ctx, statement)
		return err
	}
	mustExec = func(statement string) {
		t.Helper()
		if err := exec(statement); err != nil {
			t.Fatalf("%q: %v", statement, err)
		}
	}
	return exec, mustExec
}

// TestCatalogDiscoveryTracksOpenTransactionWrites proves the cached dependency
// discovery set is invalidated by a statement's own transaction: DDL staged in an
// open transaction is addressable by later statements in it, and ROLLBACK and
// ROLLBACK TO SAVEPOINT discard it. A statement whose declaration missed the new
// object would fail its descriptor read as uncovered rather than silently
// succeed, so addressing the object is the observable check.
func TestCatalogDiscoveryTracksOpenTransactionWrites(t *testing.T) {
	_, _, conn := declaredTypeTestSession(t)
	exec, mustExec := discoveryTestSession(t, conn)

	mustExec("SELECT 1")
	mustExec("BEGIN")
	mustExec("CREATE TABLE staged_table (id INT PRIMARY KEY, name VARCHAR)")
	// The declaration of a later statement in the same transaction must cover the
	// table the transaction just staged, or this read fails as an uncovered
	// catalog read. (Inserting into a table created in the same transaction is a
	// separate, pre-existing limitation on the unchanged code path, so this test
	// only addresses the created table.)
	mustExec("SELECT name FROM staged_table WHERE id = 1")
	mustExec("ROLLBACK")
	if err := exec("SELECT * FROM staged_table"); err == nil {
		t.Fatal("table created in a rolled-back transaction is still addressable")
	}

	mustExec("BEGIN")
	mustExec("CREATE TABLE kept_table (id INT)")
	mustExec("SAVEPOINT before_discard")
	mustExec("CREATE TABLE discarded_table (id INT)")
	mustExec("SELECT * FROM discarded_table")
	mustExec("ROLLBACK TO SAVEPOINT before_discard")
	mustExec("SELECT * FROM kept_table")
	// An error aborts the transaction (PostgreSQL semantics), so probe the
	// discarded table inside a savepoint that is rolled back to recover.
	mustExec("SAVEPOINT probe")
	if err := exec("SELECT * FROM discarded_table"); err == nil {
		t.Fatal("table created after a savepoint survived rollback to it")
	}
	mustExec("ROLLBACK TO SAVEPOINT probe")
	mustExec("COMMIT")
	mustExec("SELECT * FROM kept_table")
}

// TestCatalogDiscoveryCreateQueryDrop walks one relation through its whole life
// in a single session, so each step reads a catalog the previous step rewrote.
func TestCatalogDiscoveryCreateQueryDrop(t *testing.T) {
	_, _, conn := declaredTypeTestSession(t)
	exec, mustExec := discoveryTestSession(t, conn)

	mustExec("SELECT 1")
	mustExec("CREATE TABLE lifecycle_table (id INT PRIMARY KEY, name VARCHAR)")
	mustExec("INSERT INTO lifecycle_table VALUES (1, 'one'), (2, 'two')")
	mustExec("SELECT name FROM lifecycle_table WHERE id = 2")
	mustExec("CREATE INDEX lifecycle_table_name ON lifecycle_table (name)")
	mustExec("SELECT id FROM lifecycle_table WHERE name = 'one'")
	mustExec("DROP TABLE lifecycle_table")
	if err := exec("SELECT * FROM lifecycle_table"); err == nil {
		t.Fatal("dropped table is still addressable")
	}
	// Recreating the name after a drop must not reuse the dropped declaration.
	mustExec("CREATE TABLE lifecycle_table (id INT, extra VARCHAR)")
	mustExec("INSERT INTO lifecycle_table VALUES (3, 'three')")
	mustExec("SELECT extra FROM lifecycle_table WHERE id = 3")
}

// TestCatalogDiscoveryTemporaryObjects proves the discovery cache never serves a
// suffix discovered without the session's temporary objects: a temporary
// relation is addressable by name while it exists and is gone after DROP, while a
// permanent relation of another name is unaffected.
func TestCatalogDiscoveryTemporaryObjects(t *testing.T) {
	_, _, conn := declaredTypeTestSession(t)
	exec, mustExec := discoveryTestSession(t, conn)

	mustExec("SELECT 1")
	mustExec("CREATE TEMP TABLE temp_discovery (id INT, name VARCHAR)")
	mustExec("INSERT INTO temp_discovery VALUES (1, 'a')")
	mustExec("SELECT name FROM temp_discovery WHERE id = 1")
	mustExec("CREATE TABLE permanent_discovery (id INT)")
	mustExec("INSERT INTO permanent_discovery VALUES (2)")
	mustExec("SELECT id FROM permanent_discovery")
	// The temporary relation shadows a permanent one of the same name.
	mustExec("CREATE TABLE shadowed_discovery (id INT)")
	mustExec("CREATE TEMP TABLE shadowed_discovery (id INT, marker VARCHAR)")
	mustExec("INSERT INTO shadowed_discovery VALUES (9, 'temp')")
	mustExec("SELECT marker FROM shadowed_discovery WHERE id = 9")
	mustExec("DROP TABLE shadowed_discovery")
	// The permanent relation is addressable again once the shadow is dropped.
	mustExec("SELECT id FROM shadowed_discovery")
	mustExec("DROP TABLE temp_discovery")
	if err := exec("SELECT * FROM temp_discovery"); err == nil {
		t.Fatal("dropped temporary relation is still addressable")
	}
}

// TestCatalogDiscoveryTracksCommittedCatalogChanges proves the discovery cache
// is keyed by catalog content and not by session: DDL committed by another
// session is seen by the first session's very next statement, in both
// directions (create and drop). The committing statement runs in its own
// goroutine with a bounded context, so a lock wait cannot hang the test; the
// first session holds no open transaction across it, so there is nothing to wait
// for.
func TestCatalogDiscoveryTracksCommittedCatalogChanges(t *testing.T) {
	ctx := t.Context()
	pool, _, reader := declaredTypeTestSession(t)
	writer, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("writer conn: %v", err)
	}
	defer writer.Close()
	_, mustRead := discoveryTestSession(t, reader)

	// Warm the reader's caches against the pre-change catalog.
	mustRead("SELECT 1")
	mustRead("SELECT 1")

	runWriter := func(statement string) {
		t.Helper()
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := writer.ExecContext(bounded, statement); err != nil {
			t.Fatalf("writer %q: %v", statement, err)
		}
	}
	runWriter("CREATE TABLE committed_table (id INT PRIMARY KEY, name VARCHAR)")
	// A stale discovery set would leave the new table's descriptor row outside
	// the statement's declaration, so this read is the check.
	mustRead("SELECT name FROM committed_table WHERE id = 1")
	mustRead("INSERT INTO committed_table VALUES (1, 'one')")
	mustRead("SELECT name FROM committed_table WHERE id = 1")

	runWriter("CREATE INDEX committed_table_name ON committed_table (name)")
	mustRead("SELECT id FROM committed_table WHERE name = 'one'")

	runWriter("DROP TABLE committed_table")
	if _, err := reader.ExecContext(ctx, "SELECT * FROM committed_table"); err == nil {
		t.Fatal("table dropped by another session is still addressable")
	}
}

// TestCatalogDiscoveryHidesUncommittedTransactionDDL proves a transaction's
// staged DDL is not visible to another session. The reader sets a bounded
// lock_timeout so that a wait on the writer's catalog locks reports an error
// instead of blocking forever; the property under test is only that the reader
// never observes the uncommitted object.
func TestCatalogDiscoveryHidesUncommittedTransactionDDL(t *testing.T) {
	ctx := t.Context()
	pool, _, writer := declaredTypeTestSession(t)
	reader, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("reader conn: %v", err)
	}
	defer reader.Close()
	if _, err := reader.ExecContext(ctx, "SET lock_timeout = '2s'"); err != nil {
		t.Fatalf("SET lock_timeout: %v", err)
	}
	_, mustWrite := discoveryTestSession(t, writer)
	_, mustRead := discoveryTestSession(t, reader)

	mustWrite("BEGIN")
	mustWrite("CREATE TABLE uncommitted_table (id INT)")
	mustWrite("SELECT * FROM uncommitted_table")
	// The reader must not observe the uncommitted object. It either waits on the
	// writer's catalog locks (and reports the timeout) or resolves nothing; both
	// are non-observations.
	if _, err := reader.ExecContext(ctx, "SELECT * FROM uncommitted_table"); err == nil {
		t.Fatal("uncommitted DDL became visible to another session")
	}
	mustWrite("ROLLBACK")
	if _, err := reader.ExecContext(ctx, "SELECT * FROM uncommitted_table"); err == nil {
		t.Fatal("rolled-back DDL became visible to another session")
	}
	mustRead("SELECT 1")
}

// TestCatalogDiscoveryAlternatesWithConcurrentCommits interleaves one session's
// no-DDL reads with another session's catalog commits for many rounds, so every
// read after the first is served from a cache that the intervening commit
// invalidated. The interleaving is strictly sequential: two sessions racing
// statement-for-statement on the same catalog lock the same rows, and this
// engine wounds the older lock waiter, so a truly concurrent variant would
// exercise lock fairness rather than the cache. What the interleaving pins is
// the property the concurrency argument rests on: a cached suffix is served only
// while the view that acquires its locks still reports the identity the suffix
// was discovered under (see catalogDiscoveryCache), so a commit that landed
// after the suffix was cached is never served from the cache.
func TestCatalogDiscoveryAlternatesWithConcurrentCommits(t *testing.T) {
	ctx := t.Context()
	pool, _, reader := declaredTypeTestSession(t)
	writer, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("writer conn: %v", err)
	}
	defer writer.Close()
	_, mustRead := discoveryTestSession(t, reader)
	_, mustWrite := discoveryTestSession(t, writer)

	mustRead("SELECT 1")
	for i := range 10 {
		mustWrite(fmt.Sprintf("CREATE TABLE alternating_%d (id INT PRIMARY KEY, n INT)", i))
		mustRead(fmt.Sprintf("SELECT n FROM alternating_%d WHERE id = %d", i, i))
		mustWrite(fmt.Sprintf("CREATE TYPE alternating_enum_%d AS ENUM ('a', 'b')", i))
		mustRead(fmt.Sprintf("SELECT 'a'::alternating_enum_%d::text", i))
		mustWrite(fmt.Sprintf("ALTER TYPE alternating_enum_%d ADD VALUE 'c'", i))
		mustRead(fmt.Sprintf("SELECT 'c'::alternating_enum_%d::text", i))
		mustWrite(fmt.Sprintf("DROP TABLE alternating_%d", i))
		mustRead("SELECT 1")
	}
	for i := range 10 {
		if _, err := reader.ExecContext(ctx, fmt.Sprintf("SELECT * FROM alternating_%d", i)); err == nil {
			t.Fatalf("dropped table alternating_%d is still addressable", i)
		}
	}
}
