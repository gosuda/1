package supersql

import (
	"fmt"
	"testing"
	"time"
)

// TestEmbeddedPipelineSmoke exercises the public Exec surface end to end: DDL
// with defaults, DML with RETURNING, an explicit transaction inside one
// multi-statement call, CTE/window/array predicates, prepared statements,
// views, read-only transaction enforcement, AS OF reads, and schema changes.
func TestEmbeddedPipelineSmoke(t *testing.T) {
	ctx := t.Context()
	testClock := newTestClock()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: testClock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	exec := func(sql string) Outcome {
		t.Helper()
		outcome, err := database.Exec(ctx, sql, 0)
		if err != nil {
			t.Fatalf("%q => %v", sql, err)
		}
		return outcome
	}
	count := func(sql string) int64 {
		t.Helper()
		outcome := exec(sql)
		if len(outcome.Rows) != 1 || len(outcome.Rows[0]) != 1 {
			t.Fatalf("%q => %d rows", sql, len(outcome.Rows))
		}
		return outcome.Rows[0][0].IntVal
	}

	exec(`CREATE TABLE t (id bigint PRIMARY KEY, v text DEFAULT 'x', n numeric)`)

	// RETURNING surfaces the inserted keys as rows.
	inserted := exec(`INSERT INTO t (id, n) VALUES (1, 10), (2, 20), (3, 30) RETURNING id`)
	if len(inserted.Rows) != 3 || inserted.Rows[0][0].IntVal != 1 {
		t.Fatalf("INSERT RETURNING rows = %+v", inserted.Rows)
	}
	// The default applies; UPDATE RETURNING returns the new value.
	updated := exec(`UPDATE t SET n = n + 1 WHERE id = 1 RETURNING id, n`)
	if len(updated.Rows) != 1 || updated.Rows[0][0].IntVal != 1 {
		t.Fatalf("UPDATE RETURNING rows = %+v", updated.Rows)
	}
	exec(`DELETE FROM t WHERE id = 3`)

	selected := exec(`SELECT id, v, n::bigint FROM t ORDER BY id`)
	if len(selected.Rows) != 2 || selected.Rows[0][0].IntVal != 1 || selected.Rows[0][1].StrVal != "x" || selected.Rows[0][2].IntVal != 11 || selected.Rows[1][0].IntVal != 2 || selected.Rows[1][2].IntVal != 20 {
		t.Fatalf("SELECT rows = %+v", selected.Rows)
	}

	// One multi-statement call keeps BEGIN/COMMIT in a single session scope.
	exec(`BEGIN; INSERT INTO t VALUES (4, 'y', 40); COMMIT`)
	if got := count(`SELECT count(*) FROM t`); got != 3 {
		t.Fatalf("count after commit = %d, want 3", got)
	}

	if got := count(`WITH c AS (SELECT id FROM t) SELECT count(*) FROM c`); got != 3 {
		t.Fatalf("CTE count = %d, want 3", got)
	}
	window := exec(`SELECT (sum(n) OVER ())::bigint FROM t ORDER BY id`)
	if len(window.Rows) != 3 || window.Rows[0][0].IntVal != 71 {
		t.Fatalf("window rows = %+v, want three rows summing 71", window.Rows)
	}
	anyRow := exec(`SELECT n::bigint FROM t WHERE n = ANY (ARRAY[10, 20])`)
	if len(anyRow.Rows) != 1 || anyRow.Rows[0][0].IntVal != 20 {
		t.Fatalf("ANY rows = %+v, want [20]", anyRow.Rows)
	}

	exec(`CREATE INDEX idx_n ON t (n)`)
	if plan := exec(`EXPLAIN SELECT * FROM t`); len(plan.Rows) == 0 {
		t.Fatal("EXPLAIN returned no rows")
	}

	exec(`PREPARE p1 AS SELECT n::bigint FROM t WHERE id = $1`)
	if got := exec(`EXECUTE p1(2)`); len(got.Rows) != 1 || got.Rows[0][0].IntVal != 20 {
		t.Fatalf("EXECUTE rows = %+v, want [20]", got.Rows)
	}

	exec(`CREATE VIEW tv AS SELECT id FROM t`)
	if got := count(`SELECT count(*) FROM tv`); got != 3 {
		t.Fatalf("view count = %d, want 3", got)
	}

	// A read-only transaction rejects writes with SQLSTATE 25006. Exec shares
	// one session, so the aborted transaction needs an explicit ROLLBACK.
	if _, err := database.Exec(ctx, `BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY; INSERT INTO t VALUES (5, 'z', 50); COMMIT`, 0); err == nil {
		t.Fatal("write inside READ ONLY transaction succeeded; want 25006")
	}
	exec(`ROLLBACK`)

	// AS OF at the latest certified instant sees the committed state.
	reading, err := testClock.Read(ctx)
	if err != nil {
		t.Fatalf("clock read: %v", err)
	}
	instant := time.Unix(0, int64(reading.Latest)).UTC()
	if got := count(fmt.Sprintf(`SELECT count(*) FROM t AS OF SYSTEM TIME '%s'::timestamptz`, instant.Format("2006-01-02 15:04:05.999999999-07:00"))); got != 3 {
		t.Fatalf("AS OF count = %d, want 3", got)
	}

	// ADD COLUMN backfills the stored default.
	exec(`ALTER TABLE t ADD COLUMN extra int DEFAULT 5`)
	if got := count(`SELECT count(*) FROM t WHERE extra = 5`); got != 3 {
		t.Fatalf("rows with default = %d, want 3", got)
	}
	exec(`TRUNCATE t`)
	if got := count(`SELECT count(*) FROM t`); got != 0 {
		t.Fatalf("count after TRUNCATE = %d, want 0", got)
	}
}
