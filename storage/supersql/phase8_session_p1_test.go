package supersql

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func phase8Exec(t *testing.T, ctx context.Context, conn *sql.Conn, query string, args ...any) {
	t.Helper()
	if _, err := conn.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func queryInt(t *testing.T, ctx context.Context, conn *sql.Conn, query string, args ...any) int {
	t.Helper()
	var v int
	if err := conn.QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

func TestPhase8AutocommitSelectMisclassificationP1M1(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	phase8Exec(t, ctx, conn, `CREATE TABLE write_log (tag text, val int)`)
	phase8Exec(t, ctx, conn, `CREATE TABLE items (id int PRIMARY KEY, name text)`)
	phase8Exec(t, ctx, conn, `INSERT INTO items VALUES (1, 'Alpha'), (2, 'Beta')`)
	phase8Exec(t, ctx, conn, `CREATE SEQUENCE p8_seq`)

	// Writing helper function.
	phase8Exec(t, ctx, conn, `
		CREATE FUNCTION p8_write_val(t text, v int) RETURNS int
		LANGUAGE sql AS $$
			INSERT INTO write_log VALUES (t, v);
			SELECT v;
		$$
	`)

	// 1. View calling a writing/volatile function.
	phase8Exec(t, ctx, conn, `CREATE VIEW v_write AS SELECT p8_write_val('view_fn', 42) AS x`)
	if got := queryInt(t, ctx, conn, `SELECT x FROM v_write`); got != 42 {
		t.Fatalf("SELECT x FROM v_write = %d, want 42", got)
	}
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM write_log WHERE tag = 'view_fn' AND val = 42`); got != 1 {
		t.Fatalf("write_log count for view_fn = %d, want 1", got)
	}

	// 2. View containing FOR UPDATE locking clause.
	phase8Exec(t, ctx, conn, `CREATE VIEW v_lock AS SELECT id FROM items ORDER BY id FOR UPDATE`)
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM v_lock`); got != 2 {
		t.Fatalf("SELECT count(*) FROM v_lock = %d, want 2", got)
	}

	// 3. View calling volatile sequence nextval().
	phase8Exec(t, ctx, conn, `CREATE VIEW v_seq AS SELECT nextval('p8_seq') AS n`)
	if got := queryInt(t, ctx, conn, `SELECT n FROM v_seq`); got != 1 {
		t.Fatalf("SELECT n FROM v_seq = %d, want 1", got)
	}

	// 4a. User-defined function shadowing allowlisted built-in name (lower) under default search_path.
	phase8Exec(t, ctx, conn, `
		CREATE FUNCTION public.lower(v int) RETURNS int
		LANGUAGE sql AS $$
			INSERT INTO write_log VALUES ('shadow_lower', v);
			SELECT v * 3;
		$$
	`)
	if got := queryInt(t, ctx, conn, `SELECT lower(7)`); got != 21 {
		t.Fatalf("SELECT lower(7) = %d, want 21", got)
	}
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM write_log WHERE tag = 'shadow_lower' AND val = 7`); got != 1 {
		t.Fatalf("write_log count for shadow_lower = %d, want 1", got)
	}

	// 4b. User-defined function shadowing allowlisted built-in name (round) when public precedes pg_catalog.
	phase8Exec(t, ctx, conn, `
		CREATE FUNCTION public.round(v int) RETURNS int
		LANGUAGE sql AS $$
			INSERT INTO write_log VALUES ('shadow_round', v);
			SELECT v * 2;
		$$
	`)
	phase8Exec(t, ctx, conn, `SET search_path = public, pg_catalog`)
	if got := queryInt(t, ctx, conn, `SELECT round(7)`); got != 14 {
		t.Fatalf("SELECT round(7) = %d, want 14", got)
	}
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM write_log WHERE tag = 'shadow_round' AND val = 7`); got != 1 {
		t.Fatalf("write_log count for shadow_round = %d, want 1", got)
	}

	// 5. User-defined aggregate invoking a writing state transition function
	//    (also shadowing allowlisted built-in name "sum" for text).
	phase8Exec(t, ctx, conn, `
		CREATE FUNCTION p8_agg_sfunc(state int, val text) RETURNS int
		LANGUAGE sql AS $$
			INSERT INTO write_log VALUES ('user_agg', length(val));
			SELECT state + length(val);
		$$
	`)
	phase8Exec(t, ctx, conn, `CREATE AGGREGATE public.sum(text) (SFUNC = p8_agg_sfunc, STYPE = int, INITCOND = '0')`)
	if got := queryInt(t, ctx, conn, `SELECT sum(name) FROM items`); got != 9 {
		t.Fatalf("SELECT sum(name) FROM items = %d, want 9", got)
	}
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM write_log WHERE tag = 'user_agg'`); got != 2 {
		t.Fatalf("write_log count for user_agg = %d, want 2", got)
	}

	// 6. User-defined cast invoking a writing routine.
	phase8Exec(t, ctx, conn, `CREATE TYPE p8_box AS (v int)`)
	phase8Exec(t, ctx, conn, `
		CREATE FUNCTION p8_box_to_int(b p8_box) RETURNS int
		LANGUAGE sql STRICT AS $$
			INSERT INTO write_log VALUES ('user_cast', (b).v);
			SELECT (b).v;
		$$
	`)
	phase8Exec(t, ctx, conn, `CREATE CAST (p8_box AS int) WITH FUNCTION p8_box_to_int(p8_box)`)
	if got := queryInt(t, ctx, conn, `SELECT (ROW(9)::p8_box)::int`); got != 9 {
		t.Fatalf("SELECT (ROW(9)::p8_box)::int = %d, want 9", got)
	}
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM write_log WHERE tag = 'user_cast' AND val = 9`); got != 1 {
		t.Fatalf("write_log count for user_cast = %d, want 1", got)
	}

	// 7. Domain CHECK constraint invoking a writing routine.
	phase8Exec(t, ctx, conn, `
		CREATE FUNCTION p8_domain_check(v int) RETURNS boolean
		LANGUAGE sql AS $$
			INSERT INTO write_log VALUES ('domain_check', v);
			SELECT v > 0;
		$$
	`)
	phase8Exec(t, ctx, conn, `CREATE DOMAIN p8_logged_int AS int CHECK (p8_domain_check(VALUE))`)
	if got := queryInt(t, ctx, conn, `SELECT 11::p8_logged_int`); got != 11 {
		t.Fatalf("SELECT 11::p8_logged_int = %d, want 11", got)
	}
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM write_log WHERE tag = 'domain_check' AND val = 11`); got != 1 {
		t.Fatalf("write_log count for domain_check = %d, want 1", got)
	}

	// 8. Row-level security USING policy invoking a writing routine on a plain
	//    SELECT whose unexpanded AST has no user function calls at all.
	phase8Exec(t, ctx, conn, `CREATE TABLE t_rls (id int PRIMARY KEY)`)
	phase8Exec(t, ctx, conn, `INSERT INTO t_rls VALUES (10)`)
	phase8Exec(t, ctx, conn, `
		CREATE FUNCTION p8_rls_check(v int) RETURNS boolean
		LANGUAGE sql AS $$
			INSERT INTO write_log VALUES ('rls_using', v);
			SELECT true;
		$$
	`)
	phase8Exec(t, ctx, conn, `CREATE POLICY p8_pol ON t_rls FOR SELECT USING (p8_rls_check(id))`)
	phase8Exec(t, ctx, conn, `ALTER TABLE t_rls ENABLE ROW LEVEL SECURITY`)
	phase8Exec(t, ctx, conn, `ALTER TABLE t_rls FORCE ROW LEVEL SECURITY`)
	if got := queryInt(t, ctx, conn, `SELECT id FROM t_rls`); got != 10 {
		t.Fatalf("SELECT id FROM t_rls = %d, want 10", got)
	}
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM write_log WHERE tag = 'rls_using' AND val = 10`); got != 1 {
		t.Fatalf("write_log count for rls_using = %d, want 1", got)
	}

	// 9. Pure built-in read-only autocommit SELECT still works cleanly.
	var lowerName string
	if err := conn.QueryRowContext(ctx, `SELECT lower(name) FROM items WHERE id = 1`).Scan(&lowerName); err != nil {
		t.Fatalf("pure readonly SELECT: %v", err)
	}
	if lowerName != "alpha" {
		t.Fatalf("lower(name) = %q, want alpha", lowerName)
	}
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM items`); got != 2 {
		t.Fatalf("SELECT count(*) FROM items = %d, want 2", got)
	}

	// 10. Explicit BEGIN READ ONLY MUST STILL REJECT writes/locks with SQLSTATE 25006.
	readOnlyCases := []struct {
		name  string
		query string
	}{
		{"view_fn", `SELECT x FROM v_write`},
		{"view_lock", `SELECT id FROM v_lock`},
		{"view_seq", `SELECT n FROM v_seq`},
		{"shadow_lower", `SELECT lower(7)`},
		{"shadow_round", `SELECT round(7)`},
		{"user_agg", `SELECT sum(name) FROM items`},
		{"user_cast", `SELECT (ROW(9)::p8_box)::int`},
		{"domain_check", `SELECT 11::p8_logged_int`},
		{"rls_using", `SELECT id FROM t_rls`},
	}
	for _, tc := range readOnlyCases {
		t.Run("BeginReadOnly/"+tc.name, func(t *testing.T) {
			tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatalf("BeginTx ReadOnly: %v", err)
			}
			var discard any
			err = tx.QueryRowContext(ctx, tc.query).Scan(&discard)
			if got := SQLState(err); got != "25006" {
				t.Fatalf("BEGIN READ ONLY %s: SQLSTATE = %q (err=%v), want 25006", tc.query, got, err)
			}
			if rbErr := tx.Rollback(); rbErr != nil {
				t.Fatalf("Rollback: %v", rbErr)
			}
		})
	}

	// 11. SET default_transaction_read_only = on MUST STILL REJECT autocommit writes/locks with 25006.
	phase8Exec(t, ctx, conn, `SET default_transaction_read_only = on`)
	for _, tc := range readOnlyCases {
		t.Run("DefaultTxReadOnly/"+tc.name, func(t *testing.T) {
			var discard any
			err := conn.QueryRowContext(ctx, tc.query).Scan(&discard)
			if got := SQLState(err); got != "25006" {
				t.Fatalf("default_transaction_read_only=on %s: SQLSTATE = %q (err=%v), want 25006", tc.query, got, err)
			}
		})
	}
	// Pure read-only SELECT still succeeds with default_transaction_read_only = on.
	if got := queryInt(t, ctx, conn, `SELECT count(*) FROM items`); got != 2 {
		t.Fatalf("SELECT count(*) with default_transaction_read_only=on = %d, want 2", got)
	}
	phase8Exec(t, ctx, conn, `SET default_transaction_read_only = off`)
	// After restoring default_transaction_read_only = off, autocommit succeeds again.
	if got := queryInt(t, ctx, conn, `SELECT round(8)`); got != 16 {
		t.Fatalf("SELECT round(8) after restoring read-write = %d, want 16", got)
	}
}

func TestPhase8NotifyBoundsAndSavepointP1M2(t *testing.T) {
	ctx := t.Context()
	db, connector := openTestDB(t)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 1. Single autocommit query staging > 8 MiB of 8000-byte payloads must fail
	//    with SQLSTATE 54000 instead of exhausting memory, and leave session usable.
	_, err = conn.ExecContext(ctx, `SELECT pg_notify('p8_oom', repeat('x', 8000)) FROM generate_series(1, 2000)`)
	if got := SQLState(err); got != "54000" {
		t.Fatalf("large pg_notify batch SQLSTATE = %q (err=%v), want 54000", got, err)
	}
	if got := queryInt(t, ctx, conn, `SELECT 1`); got != 1 {
		t.Fatalf("session not usable after 54000: got %d", got)
	}

	// 2. Single autocommit query staging > 16384 tiny notifications hits entry count limit.
	_, err = conn.ExecContext(ctx, `SELECT pg_notify('p8_cnt', '') FROM generate_series(1, 17000)`)
	if got := SQLState(err); got != "54000" {
		t.Fatalf("high-count pg_notify batch SQLSTATE = %q (err=%v), want 54000", got, err)
	}

	// 3. Savepoint rollback accurately restores byte budget so subsequent pg_notify
	//    calls are not falsely rejected, and deduplication on commit is preserved.
	sub := connector.Catalog().Subscribe("p8_sp")
	defer sub.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT pg_notify('p8_sp', 'kept-before')`); err != nil {
		t.Fatalf("pg_notify kept-before: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT sp1`); err != nil {
		t.Fatalf("SAVEPOINT sp1: %v", err)
	}
	// Stage ~6 MiB (750 * 8000 = 6,000,000 bytes) after sp1.
	if _, err := tx.ExecContext(ctx, `SELECT pg_notify('p8_sp', repeat('d', 8000)) FROM generate_series(1, 750)`); err != nil {
		t.Fatalf("stage 6 MiB after sp1: %v", err)
	}
	// Roll back to sp1: must release the 6 MiB from byte accounting.
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT sp1`); err != nil {
		t.Fatalf("ROLLBACK TO SAVEPOINT sp1: %v", err)
	}
	// Stage another ~6 MiB (would exceed 8 MiB if sp1 rollback failed to restore bytes).
	if _, err := tx.ExecContext(ctx, `SELECT pg_notify('p8_sp', repeat('k', 8000)) FROM generate_series(1, 750)`); err != nil {
		t.Fatalf("stage 6 MiB after rollback to sp1: %v", err)
	}
	// Duplicate of kept-before should still deduplicate on commit.
	if _, err := tx.ExecContext(ctx, `SELECT pg_notify('p8_sp', 'kept-before')`); err != nil {
		t.Fatalf("pg_notify dup: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n1, err := sub.Wait(waitCtx)
	if err != nil || n1.Payload != "kept-before" {
		t.Fatalf("n1 = (%+v, %v), want kept-before", n1, err)
	}
	n2, err := sub.Wait(waitCtx)
	wantKept := strings.Repeat("k", 8000)
	if err != nil || n2.Payload != wantKept {
		t.Fatalf("n2 = (len=%d, err=%v), want 8000 'k's", len(n2.Payload), err)
	}
	select {
	case extra := <-sub.C():
		t.Fatalf("unexpected extra notification: len=%d", len(extra.Payload))
	case <-time.After(30 * time.Millisecond):
	}
}
