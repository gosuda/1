package supersql

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

func queryIntSlice(t *testing.T, ctx context.Context, conn *sql.Conn, query string, args ...any) []int {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var got []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", query, err)
	}
	return got
}

func queryStmtIntSlice(t *testing.T, ctx context.Context, stmt *sql.Stmt, args ...any) []int {
	t.Helper()
	rows, err := stmt.QueryContext(ctx, args...)
	if err != nil {
		t.Fatalf("stmt query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("stmt scan: %v", err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("stmt rows: %v", err)
	}
	return got
}

func TestPhase5TempCommitPreparedShadowing(t *testing.T) {
	t.Run("CreateTempBeforePrepare", func(t *testing.T) {
		ctx := t.Context()
		_, _, conn := declaredTypeTestSession(t)
		mustExec := func(sqlText string) {
			t.Helper()
			if _, err := conn.ExecContext(ctx, sqlText); err != nil {
				t.Fatalf("%q: %v", sqlText, err)
			}
		}

		mustExec(`CREATE TABLE items (id int)`)
		mustExec(`INSERT INTO items VALUES (1)`)
		mustExec(`CREATE TEMP TABLE items (id int)`)
		mustExec(`INSERT INTO items VALUES (999)`)

		selStmt, err := conn.PrepareContext(ctx, `SELECT id FROM items ORDER BY id`)
		if err != nil {
			t.Fatalf("prepare SELECT: %v", err)
		}
		defer func() { _ = selStmt.Close() }()

		insStmt, err := conn.PrepareContext(ctx, `INSERT INTO items VALUES ($1)`)
		if err != nil {
			t.Fatalf("prepare INSERT: %v", err)
		}
		defer func() { _ = insStmt.Close() }()

		updAllStmt, err := conn.PrepareContext(ctx, `UPDATE items SET id = id + 1`)
		if err != nil {
			t.Fatalf("prepare UPDATE: %v", err)
		}
		defer func() { _ = updAllStmt.Close() }()

		delAllStmt, err := conn.PrepareContext(ctx, `DELETE FROM items`)
		if err != nil {
			t.Fatalf("prepare DELETE: %v", err)
		}
		defer func() { _ = delAllStmt.Close() }()

		// Run SELECT twice to exercise both miss and hit paths in resolveCachedPlan.
		for i := range 2 {
			if got := queryStmtIntSlice(t, ctx, selStmt); !slices.Equal(got, []int{999}) {
				t.Fatalf("SELECT pass %d: got %v, want [999]", i, got)
			}
		}

		// INSERT via prepared statement must target pg_temp.items.
		if _, err := insStmt.ExecContext(ctx, 1000); err != nil {
			t.Fatalf("prepared INSERT: %v", err)
		}
		if got := queryStmtIntSlice(t, ctx, selStmt); !slices.Equal(got, []int{999, 1000}) {
			t.Fatalf("after prepared INSERT: pg_temp.items = %v, want [999 1000]", got)
		}
		if got := queryIntSlice(t, ctx, conn, `SELECT id FROM public.items ORDER BY id`); !slices.Equal(got, []int{1}) {
			t.Fatalf("after prepared INSERT: public.items = %v, want [1]", got)
		}

		// UPDATE via prepared statement must target pg_temp.items.
		res, err := updAllStmt.ExecContext(ctx)
		if err != nil {
			t.Fatalf("prepared UPDATE: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Fatalf("prepared UPDATE rows affected = %d, want 2", n)
		}
		if got := queryStmtIntSlice(t, ctx, selStmt); !slices.Equal(got, []int{1000, 1001}) {
			t.Fatalf("after prepared UPDATE: pg_temp.items = %v, want [1000 1001]", got)
		}
		if got := queryIntSlice(t, ctx, conn, `SELECT id FROM public.items ORDER BY id`); !slices.Equal(got, []int{1}) {
			t.Fatalf("after prepared UPDATE: public.items = %v, want [1]", got)
		}

		// DELETE via prepared statement must target pg_temp.items and leave public.items untouched.
		res, err = delAllStmt.ExecContext(ctx)
		if err != nil {
			t.Fatalf("prepared DELETE: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Fatalf("prepared DELETE rows affected = %d, want 2", n)
		}
		if got := queryStmtIntSlice(t, ctx, selStmt); len(got) != 0 {
			t.Fatalf("after prepared DELETE: pg_temp.items = %v, want empty", got)
		}
		if got := queryIntSlice(t, ctx, conn, `SELECT id FROM public.items ORDER BY id`); !slices.Equal(got, []int{1}) {
			t.Fatalf("after prepared DELETE: public.items = %v, want [1]", got)
		}
	})

	t.Run("PrepareBeforeCreateAlterAndDropTemp", func(t *testing.T) {
		ctx := t.Context()
		_, _, conn := declaredTypeTestSession(t)
		mustExec := func(sqlText string) {
			t.Helper()
			if _, err := conn.ExecContext(ctx, sqlText); err != nil {
				t.Fatalf("%q: %v", sqlText, err)
			}
		}

		mustExec(`CREATE TABLE items (id int)`)
		mustExec(`INSERT INTO items VALUES (1)`)

		selStmt, err := conn.PrepareContext(ctx, `SELECT id FROM items ORDER BY id`)
		if err != nil {
			t.Fatalf("prepare SELECT: %v", err)
		}
		defer func() { _ = selStmt.Close() }()

		delStmt, err := conn.PrepareContext(ctx, `DELETE FROM items`)
		if err != nil {
			t.Fatalf("prepare DELETE: %v", err)
		}
		defer func() { _ = delStmt.Close() }()

		insStmt, err := conn.PrepareContext(ctx, `INSERT INTO items VALUES ($1)`)
		if err != nil {
			t.Fatalf("prepare INSERT: %v", err)
		}
		defer func() { _ = insStmt.Close() }()

		// Warm cache against public.items before creating any temp table.
		if got := queryStmtIntSlice(t, ctx, selStmt); !slices.Equal(got, []int{1}) {
			t.Fatalf("before CREATE TEMP TABLE: got %v, want [1]", got)
		}

		// Create shadowing temp table and re-execute the exact same *sql.Stmt instances.
		mustExec(`CREATE TEMP TABLE items (id int)`)
		mustExec(`INSERT INTO items VALUES (999)`)

		if got := queryStmtIntSlice(t, ctx, selStmt); !slices.Equal(got, []int{999}) {
			t.Fatalf("after CREATE TEMP TABLE: got %v, want [999]", got)
		}
		if _, err := insStmt.ExecContext(ctx, 1000); err != nil {
			t.Fatalf("prepared INSERT after CREATE TEMP TABLE: %v", err)
		}
		if got := queryStmtIntSlice(t, ctx, selStmt); !slices.Equal(got, []int{999, 1000}) {
			t.Fatalf("after INSERT into temp: got %v, want [999 1000]", got)
		}

		// Prepared DELETE must delete from pg_temp.items, leaving public.items intact.
		res, err := delStmt.ExecContext(ctx)
		if err != nil {
			t.Fatalf("prepared DELETE on temp: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Fatalf("prepared DELETE on temp rows affected = %d, want 2", n)
		}
		if got := queryIntSlice(t, ctx, conn, `SELECT id FROM public.items ORDER BY id`); !slices.Equal(got, []int{1}) {
			t.Fatalf("public.items after temp DELETE = %v, want [1]", got)
		}

		// Re-populate and alter temp table; cached SELECT plan must invalidate and re-bind cleanly.
		mustExec(`INSERT INTO items VALUES (777)`)
		mustExec(`ALTER TABLE items ADD COLUMN tag text DEFAULT 'tmp'`)
		if got := queryStmtIntSlice(t, ctx, selStmt); !slices.Equal(got, []int{777}) {
			t.Fatalf("after ALTER TEMP TABLE: got %v, want [777]", got)
		}

		// Drop temp table and verify the same prepared statements re-bind back to public.items.
		mustExec(`DROP TABLE items`)
		if got := queryStmtIntSlice(t, ctx, selStmt); !slices.Equal(got, []int{1}) {
			t.Fatalf("after DROP TEMP TABLE: got %v, want [1]", got)
		}
		if _, err := insStmt.ExecContext(ctx, 2); err != nil {
			t.Fatalf("prepared INSERT after DROP TEMP TABLE: %v", err)
		}
		if got := queryStmtIntSlice(t, ctx, selStmt); !slices.Equal(got, []int{1, 2}) {
			t.Fatalf("public.items after INSERT: got %v, want [1 2]", got)
		}
		res, err = delStmt.ExecContext(ctx)
		if err != nil {
			t.Fatalf("prepared DELETE on public: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Fatalf("prepared DELETE on public rows affected = %d, want 2", n)
		}
		if got := queryStmtIntSlice(t, ctx, selStmt); len(got) != 0 {
			t.Fatalf("public.items after DELETE: got %v, want empty", got)
		}
	})
}

func TestPhase5TempCommitDeferredConstraints(t *testing.T) {
	t.Run("Enum", func(t *testing.T) {
		ctx := t.Context()
		_, _, conn := declaredTypeTestSession(t)
		mustExec := func(sqlText string) {
			t.Helper()
			if _, err := conn.ExecContext(ctx, sqlText); err != nil {
				t.Fatalf("%q: %v", sqlText, err)
			}
		}

		mustExec(`CREATE TYPE status AS ENUM ('a', 'b')`)
		mustExec(`CREATE TABLE t (k status UNIQUE DEFERRABLE INITIALLY DEFERRED)`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO t VALUES ('a')`)
		mustExec(`COMMIT`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO t VALUES ('a')`)
		_, err := conn.ExecContext(ctx, `COMMIT`)
		if err == nil {
			t.Fatalf("expected duplicate deferred enum UNIQUE violation on COMMIT, got nil")
		}
		if got := SQLState(err); got != "23505" {
			t.Fatalf("COMMIT SQLSTATE = %q (err=%v), want 23505", got, err)
		}
	})

	t.Run("Domain", func(t *testing.T) {
		ctx := t.Context()
		_, _, conn := declaredTypeTestSession(t)
		mustExec := func(sqlText string) {
			t.Helper()
			if _, err := conn.ExecContext(ctx, sqlText); err != nil {
				t.Fatalf("%q: %v", sqlText, err)
			}
		}

		mustExec(`CREATE DOMAIN pos_int AS int CHECK (VALUE > 0)`)
		mustExec(`CREATE TABLE t_dom (k pos_int UNIQUE DEFERRABLE INITIALLY DEFERRED)`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO t_dom VALUES (10)`)
		mustExec(`COMMIT`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO t_dom VALUES (10)`)
		_, err := conn.ExecContext(ctx, `COMMIT`)
		if err == nil {
			t.Fatalf("expected duplicate deferred domain UNIQUE violation on COMMIT, got nil")
		}
		if got := SQLState(err); got != "23505" {
			t.Fatalf("COMMIT SQLSTATE = %q (err=%v), want 23505", got, err)
		}
	})

	t.Run("Composite", func(t *testing.T) {
		ctx := t.Context()
		_, _, conn := declaredTypeTestSession(t)
		mustExec := func(sqlText string) {
			t.Helper()
			if _, err := conn.ExecContext(ctx, sqlText); err != nil {
				t.Fatalf("%q: %v", sqlText, err)
			}
		}

		mustExec(`CREATE TYPE pair AS (x int, y text)`)
		mustExec(`CREATE TABLE t_comp (k pair UNIQUE DEFERRABLE INITIALLY DEFERRED)`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO t_comp VALUES (ROW(1, 'a')::pair)`)
		mustExec(`COMMIT`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO t_comp VALUES (ROW(1, 'a')::pair)`)
		_, err := conn.ExecContext(ctx, `COMMIT`)
		if err == nil {
			t.Fatalf("expected duplicate deferred composite UNIQUE violation on COMMIT, got nil")
		}
		if got := SQLState(err); got != "23505" {
			t.Fatalf("COMMIT SQLSTATE = %q (err=%v), want 23505", got, err)
		}
	})

	t.Run("CustomCollationDomain", func(t *testing.T) {
		ctx := t.Context()
		_, _, conn := declaredTypeTestSession(t)
		mustExec := func(sqlText string) {
			t.Helper()
			if _, err := conn.ExecContext(ctx, sqlText); err != nil {
				t.Fatalf("%q: %v", sqlText, err)
			}
		}

		mustExec(`CREATE COLLATION ci_en (provider = icu, locale = 'en-US-u-ks-level2', deterministic = false)`)
		mustExec(`CREATE DOMAIN ci_text AS text COLLATE ci_en`)
		mustExec(`CREATE TABLE t_coll (k ci_text UNIQUE DEFERRABLE INITIALLY DEFERRED)`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO t_coll VALUES ('Hello')`)
		mustExec(`COMMIT`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO t_coll VALUES ('hello')`)
		_, err := conn.ExecContext(ctx, `COMMIT`)
		if err == nil {
			t.Fatalf("expected duplicate deferred custom-collation UNIQUE violation on COMMIT, got nil")
		}
		if got := SQLState(err); got != "23505" {
			t.Fatalf("COMMIT SQLSTATE = %q (err=%v), want 23505", got, err)
		}
	})
}

func TestPhase5TempCommitOnCommitDeleteRows(t *testing.T) {
	t.Run("EnumIndex", func(t *testing.T) {
		ctx := t.Context()
		_, _, conn := declaredTypeTestSession(t)
		mustExec := func(sqlText string) {
			t.Helper()
			if _, err := conn.ExecContext(ctx, sqlText); err != nil {
				t.Fatalf("%q: %v", sqlText, err)
			}
		}

		mustExec(`CREATE TYPE status AS ENUM ('a', 'b')`)
		mustExec(`CREATE TEMP TABLE tmp_enum (id int PRIMARY KEY, s status) ON COMMIT DELETE ROWS`)
		mustExec(`CREATE INDEX tmp_enum_s ON tmp_enum (s)`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO tmp_enum VALUES (1, 'a')`)
		mustExec(`COMMIT`)

		var count int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM tmp_enum`).Scan(&count); err != nil {
			t.Fatalf("SELECT count(*): %v", err)
		}
		if count != 0 {
			t.Fatalf("count after COMMIT = %d, want 0", count)
		}

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO tmp_enum VALUES (2, 'b')`)
		var s string
		if err := conn.QueryRowContext(ctx, `SELECT s FROM tmp_enum`).Scan(&s); err != nil {
			t.Fatalf("SELECT s: %v", err)
		}
		if s != "b" {
			t.Fatalf("SELECT s = %q, want %q", s, "b")
		}
		mustExec(`COMMIT`)
	})

	t.Run("DomainCompositeAndCollation", func(t *testing.T) {
		ctx := t.Context()
		_, _, conn := declaredTypeTestSession(t)
		mustExec := func(sqlText string) {
			t.Helper()
			if _, err := conn.ExecContext(ctx, sqlText); err != nil {
				t.Fatalf("%q: %v", sqlText, err)
			}
		}

		mustExec(`CREATE DOMAIN pos_int AS int CHECK (VALUE > 0)`)
		mustExec(`CREATE TYPE pair AS (x int, y text)`)
		mustExec(`CREATE COLLATION ci_en (provider = icu, locale = 'en-US-u-ks-level2', deterministic = false)`)
		mustExec(`CREATE TEMP TABLE tmp_types (id int PRIMARY KEY, d pos_int, p pair, c text COLLATE ci_en) ON COMMIT DELETE ROWS`)
		mustExec(`CREATE INDEX tmp_types_d ON tmp_types (d)`)
		mustExec(`CREATE INDEX tmp_types_p ON tmp_types (p)`)
		mustExec(`CREATE INDEX tmp_types_c ON tmp_types (c)`)

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO tmp_types VALUES (1, 7, ROW(1, 'x')::pair, 'Alpha')`)
		mustExec(`COMMIT`)

		var count int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM tmp_types`).Scan(&count); err != nil {
			t.Fatalf("SELECT count(*): %v", err)
		}
		if count != 0 {
			t.Fatalf("count after COMMIT = %d, want 0", count)
		}

		mustExec(`BEGIN`)
		mustExec(`INSERT INTO tmp_types VALUES (2, 9, ROW(2, 'y')::pair, 'Beta')`)
		var id, d int
		var c string
		if err := conn.QueryRowContext(ctx, `SELECT id, d, c FROM tmp_types WHERE c = 'beta'`).Scan(&id, &d, &c); err != nil {
			t.Fatalf("SELECT id, d, c: %v", err)
		}
		if id != 2 || d != 9 || c != "Beta" {
			t.Fatalf("got (%d, %d, %q), want (2, 9, %q)", id, d, c, "Beta")
		}
		mustExec(`COMMIT`)
	})
}
