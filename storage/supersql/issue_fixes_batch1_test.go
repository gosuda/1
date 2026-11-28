package supersql

import (
	"testing"
)

func TestBatch1CoreIssueFixes(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: t.TempDir(), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	scope, err := db.newSession(ctx)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(func() { _ = scope.close() })

	mustExec := func(t *testing.T, sql string) {
		t.Helper()
		if _, err := db.execIn(ctx, scope, sql, 10000); err != nil {
			t.Fatalf("exec %q failed: %v", sql, err)
		}
	}
	mustFailState := func(t *testing.T, sql, wantState string) {
		t.Helper()
		_, err := db.execIn(ctx, scope, sql, 10000)
		if err == nil {
			t.Fatalf("exec %q succeeded, want SQLSTATE %s", sql, wantState)
		}
		if got := regressionSQLState(err); got != wantState {
			t.Fatalf("exec %q: state=%q (%v), want %q", sql, got, err, wantState)
		}
	}

	// Issue #41: trivial multi-way UNION must not hit 53200 reserve query bytes.
	t.Run("Issue41_TrivialUnionMemoryBudget", func(t *testing.T) {
		res, err := db.execIn(ctx, scope, `SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 ORDER BY 1`, 10000)
		if err != nil {
			t.Fatalf("5-way UNION failed: %v", err)
		}
		if len(res.Rows) != 5 {
			t.Fatalf("5-way UNION returned %d rows, want 5", len(res.Rows))
		}
	})

	// Issue #43: TRUNCATE ... RESTART IDENTITY and in-tx DML after uncommitted TRUNCATE.
	t.Run("Issue43_TruncateRestartIdentityAndInTxDML", func(t *testing.T) {
		mustExec(t, `CREATE TABLE trunc_seq (id serial primary key, v text)`)
		mustExec(t, `INSERT INTO trunc_seq (v) VALUES ('a'), ('b')`)
		mustExec(t, `TRUNCATE trunc_seq RESTART IDENTITY`)
		mustExec(t, `INSERT INTO trunc_seq (v) VALUES ('c')`)
		res, err := db.execIn(ctx, scope, `SELECT id, v FROM trunc_seq`, 10000)
		if err != nil || len(res.Rows) != 1 || res.Rows[0][0].IntVal != 1 {
			t.Fatalf("after TRUNCATE RESTART IDENTITY: rows=%+v err=%v", res.Rows, err)
		}
		// Uncommitted TRUNCATE followed by INSERT in the same transaction.
		mustExec(t, `BEGIN`)
		mustExec(t, `TRUNCATE trunc_seq RESTART IDENTITY`)
		mustExec(t, `INSERT INTO trunc_seq (v) VALUES ('in_tx')`)
		mustExec(t, `COMMIT`)
		res, err = db.execIn(ctx, scope, `SELECT id, v FROM trunc_seq`, 10000)
		if err != nil || len(res.Rows) != 1 || res.Rows[0][1].StrVal != "in_tx" {
			t.Fatalf("after in-tx TRUNCATE+INSERT: rows=%+v err=%v", res.Rows, err)
		}
	})

	// Issue #27 / #13: EXPLAIN DECLARE CURSOR no-leak, pg_temp explicit schema, auto-promoted temp view.
	t.Run("Issue27_SessionLifecycleAndTempObjects", func(t *testing.T) {
		mustExec(t, `CREATE TABLE explain_cur_t (a int)`)
		mustExec(t, `BEGIN`)
		mustExec(t, `EXPLAIN DECLARE c_exp CURSOR FOR SELECT a FROM explain_cur_t`)
		mustExec(t, `DECLARE c_exp CURSOR FOR SELECT a FROM explain_cur_t`)
		mustExec(t, `CLOSE c_exp`)
		mustExec(t, `COMMIT`)

		mustExec(t, `CREATE TABLE pg_temp.explicit_tmp (x int)`)
		mustExec(t, `INSERT INTO pg_temp.explicit_tmp VALUES (42)`)
		mustExec(t, `CREATE VIEW auto_tmp_v AS SELECT x FROM explicit_tmp`)
		res, err := db.execIn(ctx, scope, `SELECT x FROM auto_tmp_v`, 10000)
		if err != nil || len(res.Rows) != 1 || res.Rows[0][0].IntVal != 42 {
			t.Fatalf("auto-promoted temp view: rows=%+v err=%v", res.Rows, err)
		}
		mustFailState(t, `CREATE VIEW public.bad_perm_v AS SELECT x FROM explicit_tmp`, "42P16")
	})

	// Issue #28 / #11: Collated index + subsequent index, SQL-body BEGIN ATOMIC, multi-action ALTER TABLE.
	t.Run("Issue28_CollatedIndexSQLBodyAndMultiAlter", func(t *testing.T) {
		mustExec(t, `CREATE TABLE ic_test (key int, fruit text)`)
		mustExec(t, `CREATE UNIQUE INDEX both_index_key ON ic_test (key, fruit COLLATE "C" text_pattern_ops)`)
		mustExec(t, `CREATE UNIQUE INDEX fruit_lower_idx ON ic_test (lower(fruit))`)

		mustExec(t, `CREATE FUNCTION sql_atomic_add(a int, b int) RETURNS int BEGIN ATOMIC RETURN a + b; END`)
		res, err := db.execIn(ctx, scope, `SELECT sql_atomic_add(3, 4)`, 10000)
		if err != nil || len(res.Rows) != 1 || res.Rows[0][0].IntVal != 7 {
			t.Fatalf("BEGIN ATOMIC function: rows=%+v err=%v", res.Rows, err)
		}

		mustExec(t, `CREATE TABLE multi_alt (a int)`)
		mustExec(t, `ALTER TABLE multi_alt ADD COLUMN b int, ADD CONSTRAINT multi_alt_b_pos CHECK (b > 0)`)
		mustFailState(t, `INSERT INTO multi_alt VALUES (1, -5)`, "23514")
		mustExec(t, `INSERT INTO multi_alt VALUES (1, 5)`)
	})

	// Issue #9: ALTER TABLE <keyless> ADD CHECK, inheritance propagation, column-based naming, inherited drop protection.
	t.Run("Issue9_KeylessAddCheckAndInheritance", func(t *testing.T) {
		mustExec(t, `CREATE TABLE ac (aa text)`)
		mustExec(t, `ALTER TABLE ac ADD CONSTRAINT ac_check CHECK (aa IS NOT NULL)`)
		mustExec(t, `CREATE TABLE bc (bb text) INHERITS (ac)`)
		mustFailState(t, `INSERT INTO ac (aa) VALUES (NULL)`, "23514")
		mustFailState(t, `INSERT INTO bc (aa) VALUES (NULL)`, "23514")
		mustFailState(t, `ALTER TABLE bc DROP CONSTRAINT ac_check`, "42P16")
		mustExec(t, `ALTER TABLE ac DROP CONSTRAINT ac_check`)
		// Unnamed CHECK should propagate to existing child bc with default name ac_aa_check.
		mustExec(t, `ALTER TABLE ac ADD CHECK (aa IS NOT NULL)`)
		mustFailState(t, `INSERT INTO ac (aa) VALUES (NULL)`, "23514")
		mustFailState(t, `INSERT INTO bc (aa) VALUES (NULL)`, "23514")
		mustFailState(t, `ALTER TABLE bc DROP CONSTRAINT ac_aa_check`, "42P16")
		mustExec(t, `ALTER TABLE ac DROP CONSTRAINT ac_aa_check`)
		mustExec(t, `INSERT INTO bc (aa) VALUES (NULL)`)
	})

	// Issue #30: Pseudo-type column rejection (42P16), NOT ENFORCED column attribute, ALTER CONSTRAINT ENFORCED validation.
	t.Run("Issue30_PseudoTypesAndEnforcedConstraints", func(t *testing.T) {
		mustFailState(t, `CREATE TABLE unknowntab (u unknown)`, "42P16")
		mustFailState(t, `CREATE DOMAIN unknowndom AS unknown`, "42P16")

		mustExec(t, `CREATE TABLE fk_parent (id int primary key)`)
		mustExec(t, `CREATE TABLE fk_child (pid int REFERENCES fk_parent(id) NOT ENFORCED)`)
		mustExec(t, `INSERT INTO fk_child VALUES (999)`)
		mustFailState(t, `ALTER TABLE fk_child ALTER CONSTRAINT fk_child_pid_fkey ENFORCED`, "23503")
		mustExec(t, `DELETE FROM fk_child`)
		mustExec(t, `ALTER TABLE fk_child ALTER CONSTRAINT fk_child_pid_fkey ENFORCED`)
		mustFailState(t, `INSERT INTO fk_child VALUES (999)`, "23503")
	})
}
