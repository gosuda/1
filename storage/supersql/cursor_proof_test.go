package supersql

import (
	"fmt"
	"strings"
	"testing"
)

// TestPinnedParallelismAdmitsCursorBurst pins the prepareWorkers group floor:
// with Parallelism fixed to 4 (independent of runner GOMAXPROCS), a
// transaction holding 13 open cursors must still be admitted. Before the
// floor, maxGroups was max(16, parallelism*4) = 16 on 4 workers and the 9th
// portals_p2 DECLARE failed with 53200 on small runners while passing on
// larger ones.
func TestPinnedParallelismAdmitsCursorBurst(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: t.TempDir(), Clock: newTestClock(), Parallelism: 4})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	scope, err := db.newSession(ctx)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer scope.close()
	for _, stmt := range []string{
		`CREATE TABLE cur_probe(n int);`,
		`INSERT INTO cur_probe VALUES (1), (2), (3);`,
		`BEGIN;`,
	} {
		if _, err := db.execIn(ctx, scope, stmt, 10000); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	for i := 0; i < 13; i++ {
		stmt := fmt.Sprintf(`DECLARE c%d CURSOR FOR SELECT n FROM cur_probe ORDER BY n;`, i)
		if _, err := db.execIn(ctx, scope, stmt, 10000); err != nil {
			t.Fatalf("declare %d: %v", i, err)
		}
	}
	for i := 0; i < 13; i++ {
		stmt := fmt.Sprintf(`FETCH ALL FROM c%d;`, i)
		if _, err := db.execIn(ctx, scope, stmt, 10000); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if _, err := db.execIn(ctx, scope, `COMMIT;`, 10000); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCursorEvaluatesRowErrors pins that a cursor over populated rows
// evaluates per-row errors and that a failed cursor stays unusable. The
// transactions.txt ratchet records these FETCHes as passing only because the
// skipped setup COPY leaves tenk1 empty, so the cursor never sees a row.
func TestCursorEvaluatesRowErrors(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: t.TempDir(), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	scope, err := db.newSession(ctx)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer scope.close()
	for _, stmt := range []string{
		`CREATE TABLE div_probe(n int);`,
		`INSERT INTO div_probe VALUES (10), (20), (30);`,
		`BEGIN;`,
		`DECLARE dead CURSOR FOR SELECT n/0 FROM div_probe ORDER BY n;`,
		`DECLARE live CURSOR FOR SELECT n FROM div_probe ORDER BY n;`,
		`FETCH 10 FROM live;`,
		`SAVEPOINT s1;`,
	} {
		if _, err := db.execIn(ctx, scope, stmt, 10000); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	if _, err := db.execIn(ctx, scope, `FETCH 10 FROM dead;`, 10000); err == nil {
		t.Fatalf("expected division-by-zero on populated fetch, got nil")
	} else if got := regressionSQLState(err); got != "22012" {
		t.Fatalf("fetch state = %q, want 22012 (%v)", got, err)
	}
	for _, stmt := range []string{
		`ROLLBACK TO SAVEPOINT s1;`,
		`RELEASE SAVEPOINT s1;`,
		`SAVEPOINT s2;`,
	} {
		if _, err := db.execIn(ctx, scope, stmt, 10000); err != nil {
			t.Fatalf("recover %q: %v", stmt, err)
		}
	}
	// The failed cursor stays unusable: PostgreSQL marks the portal dead,
	// so the refetch is rejected with 55000 instead of re-running the
	// query. The pg184 reference (expected/transactions.out: the FETCH
	// after ROLLBACK TO answers `portal "c" cannot be run`) pins this:
	// the portal predates the savepoint yet stays dead to the world.
	if _, err := db.execIn(ctx, scope, `FETCH 10 FROM dead;`, 10000); err == nil {
		t.Fatalf("expected failed cursor to stay unusable, got nil")
	} else if got := regressionSQLState(err); got != "55000" {
		t.Fatalf("dead-cursor refetch state = %q, want 55000 (%v)", got, err)
	} else if want := `portal "dead" cannot be run`; !strings.Contains(err.Error(), want) {
		t.Fatalf("dead-cursor refetch message = %q, want substring %q", err.Error(), want)
	}
	if _, err := db.execIn(ctx, scope, `SELECT 1;`, 10000); regressionSQLState(err) != "25P02" {
		t.Fatalf("transaction after first dead-cursor refetch: state=%q (%v), want 25P02", regressionSQLState(err), err)
	}
	// The 55000 aborts the transaction like any other ERROR, so recover
	// inside the savepoint and refetch once more: the portal itself is
	// dead, so the rejection repeats instead of clearing.
	if _, err := db.execIn(ctx, scope, `ROLLBACK TO SAVEPOINT s2;`, 10000); err != nil {
		t.Fatalf("recover s2 after first refetch: %v", err)
	}
	if _, err := db.execIn(ctx, scope, `SELECT 1;`, 10000); err != nil {
		t.Fatalf("transaction after first refetch rollback: %v", err)
	}
	if _, err := db.execIn(ctx, scope, `FETCH 10 FROM dead;`, 10000); err == nil {
		t.Fatalf("expected failed cursor to stay unusable on second refetch, got nil")
	} else if got := regressionSQLState(err); got != "55000" {
		t.Fatalf("dead-cursor second refetch state = %q, want 55000 (%v)", got, err)
	}
	if _, err := db.execIn(ctx, scope, `SELECT 1;`, 10000); regressionSQLState(err) != "25P02" {
		t.Fatalf("transaction after second dead-cursor refetch: state=%q (%v), want 25P02", regressionSQLState(err), err)
	}
	if _, err := db.execIn(ctx, scope, `ROLLBACK TO SAVEPOINT s2;`, 10000); err != nil {
		t.Fatalf("recover s2 after second refetch: %v", err)
	}
	if _, err := db.execIn(ctx, scope, `SELECT 1;`, 10000); err != nil {
		t.Fatalf("transaction after second refetch rollback: %v", err)
	}
	for _, stmt := range []string{
		`ROLLBACK TO SAVEPOINT s2;`,
		`RELEASE SAVEPOINT s2;`,
	} {
		if _, err := db.execIn(ctx, scope, stmt, 10000); err != nil {
			t.Fatalf("recover %q: %v", stmt, err)
		}
	}
	if _, err := db.execIn(ctx, scope, `FETCH 10 FROM live;`, 10000); err != nil {
		t.Fatalf("healthy cursor fetch after recover: %v", err)
	}
	if _, err := db.execIn(ctx, scope, `ROLLBACK;`, 10000); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

// TestCursorDirectionErrorMarksPortalFailedMatchesPG18 verifies PostgreSQL 18
// PortalRunFetch semantics: MarkPortalActive runs before DoPortalRunFetch, so
// an invalid FETCH BACKWARD on a NO SCROLL cursor transitions the portal to
// PORTAL_FAILED both inside a savepoint and for a committed WITH HOLD cursor.
func TestCursorDirectionErrorMarksPortalFailedMatchesPG18(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: t.TempDir(), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	scope, err := db.newSession(ctx)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer scope.close()
	for _, stmt := range []string{
		`CREATE TABLE cur_dir_probe(n int);`,
		`INSERT INTO cur_dir_probe VALUES (1), (2), (3);`,
		`BEGIN;`,
		`DECLARE c NO SCROLL CURSOR FOR SELECT n FROM cur_dir_probe ORDER BY n;`,
		`SAVEPOINT s1;`,
	} {
		if _, err := db.execIn(ctx, scope, stmt, 10000); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	if _, err := db.execIn(ctx, scope, `FETCH BACKWARD 1 FROM c;`, 10000); regressionSQLState(err) != "55000" {
		t.Fatalf("invalid backward fetch state = %q (%v), want 55000", regressionSQLState(err), err)
	}
	if _, err := db.execIn(ctx, scope, `ROLLBACK TO SAVEPOINT s1;`, 10000); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
	if _, err := db.execIn(ctx, scope, `FETCH 1 FROM c;`, 10000); regressionSQLState(err) != "55000" || !strings.Contains(err.Error(), `portal "c" cannot be run`) {
		t.Fatalf("refetch after direction error = %v, want 55000 portal cannot be run", err)
	}
	for _, stmt := range []string{
		`ROLLBACK;`,
		`BEGIN;`,
		`DECLARE h NO SCROLL CURSOR WITH HOLD FOR SELECT n FROM cur_dir_probe ORDER BY n;`,
		`COMMIT;`,
	} {
		if _, err := db.execIn(ctx, scope, stmt, 10000); err != nil {
			t.Fatalf("setup holdable %q: %v", stmt, err)
		}
	}
	if _, err := db.execIn(ctx, scope, `FETCH BACKWARD 1 FROM h;`, 10000); regressionSQLState(err) != "55000" {
		t.Fatalf("holdable invalid backward fetch state = %q (%v), want 55000", regressionSQLState(err), err)
	}
	if _, err := db.execIn(ctx, scope, `FETCH 1 FROM h;`, 10000); regressionSQLState(err) != "55000" || !strings.Contains(err.Error(), `portal "h" cannot be run`) {
		t.Fatalf("holdable refetch after direction error = %v, want 55000 portal cannot be run", err)
	}
	if _, err := db.execIn(ctx, scope, `CLOSE h;`, 10000); err != nil {
		t.Fatalf("close holdable: %v", err)
	}
}
