package supersql

import (
	"strings"
	"testing"
)

func TestPhase6RLSMergeDefaultDeny(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	mustExec := func(sqlText string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sqlText, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sqlText, err)
		}
		return out
	}
	requireErrCode := func(sqlText, wantCode string) error {
		t.Helper()
		_, err := db.exec(ctx, sqlText, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sqlText, wantCode)
		}
		if got := sqlStateOf(err); got != wantCode {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sqlText, wantCode, got, err)
		}
		return err
	}

	mustExec(`CREATE ROLE merge_rls_user`)

	// 1. Table with FOR SELECT USING (true) ONLY (no FOR DELETE / FOR UPDATE policy).
	mustExec(`CREATE TABLE m_sel_only (id bigint PRIMARY KEY, val text NOT NULL)`)
	mustExec(`INSERT INTO m_sel_only VALUES (1, 'v1'), (2, 'v2')`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON m_sel_only TO merge_rls_user`)
	mustExec(`ALTER TABLE m_sel_only ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY sel_only ON m_sel_only FOR SELECT TO merge_rls_user USING (true)`)

	mustExec(`SET ROLE merge_rls_user`)

	// MERGE ... WHEN MATCHED THEN DELETE must default-deny (0 rows deleted).
	out := mustExec(`MERGE INTO m_sel_only t
		USING (VALUES (1), (2)) AS s(id)
		ON t.id = s.id
		WHEN MATCHED THEN DELETE`)
	if out.RowsAffected != 0 {
		t.Fatalf("MERGE DELETE without DELETE policy affected %d rows, want 0", out.RowsAffected)
	}
	// WHEN NOT MATCHED BY SOURCE THEN DELETE must also default-deny.
	out = mustExec(`MERGE INTO m_sel_only t
		USING (VALUES (99)) AS s(id)
		ON t.id = s.id
		WHEN NOT MATCHED BY SOURCE THEN DELETE`)
	if out.RowsAffected != 0 {
		t.Fatalf("MERGE NOT MATCHED BY SOURCE DELETE without DELETE policy affected %d rows, want 0", out.RowsAffected)
	}
	// MERGE ... WHEN MATCHED THEN UPDATE must default-deny (0 rows updated).
	out = mustExec(`MERGE INTO m_sel_only t
		USING (VALUES (1, 'hijack')) AS s(id, val)
		ON t.id = s.id
		WHEN MATCHED THEN UPDATE SET val = s.val`)
	if out.RowsAffected != 0 {
		t.Fatalf("MERGE UPDATE without UPDATE policy affected %d rows, want 0", out.RowsAffected)
	}
	// WHEN NOT MATCHED BY SOURCE THEN UPDATE must also default-deny.
	out = mustExec(`MERGE INTO m_sel_only t
		USING (VALUES (99)) AS s(id)
		ON t.id = s.id
		WHEN NOT MATCHED BY SOURCE THEN UPDATE SET val = 'hijack'`)
	if out.RowsAffected != 0 {
		t.Fatalf("MERGE NOT MATCHED BY SOURCE UPDATE without UPDATE policy affected %d rows, want 0", out.RowsAffected)
	}

	// Verify rows are completely untouched.
	out = mustExec(`SELECT id, val FROM m_sel_only ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][1].StrVal != "v1" || out.Rows[1][1].StrVal != "v2" {
		t.Fatalf("m_sel_only rows modified unexpectedly: %+v", out.Rows)
	}

	// Add INSERT policy (still no DELETE policy): a matched row whose DELETE is denied
	// must NOT be treated as NOT MATCHED and must NOT trigger WHEN NOT MATCHED THEN INSERT.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE POLICY ins_only ON m_sel_only FOR INSERT TO merge_rls_user WITH CHECK (true)`)
	mustExec(`SET ROLE merge_rls_user`)
	out = mustExec(`MERGE INTO m_sel_only t
		USING (VALUES (1, 'dup')) AS s(id, val)
		ON t.id = s.id
		WHEN MATCHED THEN DELETE
		WHEN NOT MATCHED THEN INSERT (id, val) VALUES (s.id, s.val)`)
	if out.RowsAffected != 0 {
		t.Fatalf("MERGE DELETE+INSERT on existing row without DELETE policy affected %d rows, want 0", out.RowsAffected)
	}

	// 2. MERGE with UPDATE policy ONLY (no DELETE policy):
	//    - MERGE with ONLY WHEN MATCHED THEN UPDATE succeeds (does not require DELETE policy).
	//    - MERGE with BOTH UPDATE and DELETE updates allowed rows and denies DELETE rows.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE TABLE m_upd_only (id bigint PRIMARY KEY, val text NOT NULL)`)
	mustExec(`INSERT INTO m_upd_only VALUES (1, 'a'), (2, 'b')`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON m_upd_only TO merge_rls_user`)
	mustExec(`ALTER TABLE m_upd_only ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY m_upd_sel ON m_upd_only FOR SELECT TO merge_rls_user USING (val <> 'hidden')`)
	mustExec(`CREATE POLICY m_upd_upd ON m_upd_only FOR UPDATE TO merge_rls_user USING (true) WITH CHECK (val <> 'bad')`)

	mustExec(`SET ROLE merge_rls_user`)
	out = mustExec(`MERGE INTO m_upd_only AS alias_t
		USING (VALUES (1, 'a1')) AS s(id, val)
		ON alias_t.id = s.id
		WHEN MATCHED THEN UPDATE SET val = s.val`)
	if out.RowsAffected != 1 {
		t.Fatalf("MERGE UPDATE-only with UPDATE policy affected %d rows, want 1", out.RowsAffected)
	}
	// Mixed UPDATE + DELETE: UPDATE succeeds for id=1, DELETE is denied for id=2.
	out = mustExec(`MERGE INTO m_upd_only t
		USING (VALUES (1, 'upd'), (2, 'del')) AS s(id, op)
		ON t.id = s.id
		WHEN MATCHED AND s.op = 'upd' THEN UPDATE SET val = 'a2'
		WHEN MATCHED AND s.op = 'del' THEN DELETE`)
	if out.RowsAffected != 1 {
		t.Fatalf("MERGE UPDATE+DELETE without DELETE policy affected %d rows, want 1", out.RowsAffected)
	}
	// UPDATE WITH CHECK violation raises 42501.
	requireErrCode(`MERGE INTO m_upd_only t
		USING (VALUES (1, 'bad')) AS s(id, val)
		ON t.id = s.id
		WHEN MATCHED THEN UPDATE SET val = s.val`, "42501")
	// UPDATE producing a row invisible to SELECT USING raises 42501 (PG 18).
	requireErrCode(`MERGE INTO m_upd_only t
		USING (VALUES (1, 'hidden')) AS s(id, val)
		ON t.id = s.id
		WHEN MATCHED THEN UPDATE SET val = s.val`, "42501")

	mustExec(`RESET ROLE`)
	out = mustExec(`SELECT id, val FROM m_upd_only ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][1].StrVal != "a2" || out.Rows[1][1].StrVal != "b" {
		t.Fatalf("m_upd_only final rows = %+v, want [(1,a2), (2,b)]", out.Rows)
	}

	// 3. MERGE with DELETE policy ONLY (no UPDATE policy) and a fallback DELETE clause:
	//    When a row matches the first WHEN MATCHED AND cond THEN UPDATE clause,
	//    the denied UPDATE must NOT fall through into the fallback DELETE clause.
	mustExec(`CREATE TABLE m_del_only (id bigint PRIMARY KEY, val text NOT NULL)`)
	mustExec(`INSERT INTO m_del_only VALUES (1, 'keep'), (2, 'drop')`)
	mustExec(`GRANT SELECT, UPDATE, DELETE ON m_del_only TO merge_rls_user`)
	mustExec(`ALTER TABLE m_del_only ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY m_del_sel ON m_del_only FOR SELECT TO merge_rls_user USING (true)`)
	mustExec(`CREATE POLICY m_del_del ON m_del_only FOR DELETE TO merge_rls_user USING (true)`)

	mustExec(`SET ROLE merge_rls_user`)
	out = mustExec(`MERGE INTO m_del_only t
		USING (VALUES (1), (2)) AS s(id)
		ON t.id = s.id
		WHEN MATCHED AND t.val = 'keep' THEN UPDATE SET val = 'changed'
		WHEN MATCHED THEN DELETE`)
	if out.RowsAffected != 1 {
		t.Fatalf("MERGE UPDATE(denied)+fallback DELETE affected %d rows, want 1", out.RowsAffected)
	}
	mustExec(`RESET ROLE`)
	out = mustExec(`SELECT id, val FROM m_del_only ORDER BY id`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 1 || out.Rows[0][1].StrVal != "keep" {
		t.Fatalf("m_del_only final rows = %+v, want [(1,keep)]", out.Rows)
	}

	// 4. MERGE with disjoint UPDATE and DELETE USING policies: neither masks the other.
	mustExec(`CREATE TABLE m_disjoint (id bigint PRIMARY KEY, cat text NOT NULL, val text NOT NULL)`)
	mustExec(`INSERT INTO m_disjoint VALUES (1, 'novel', 'n1'), (2, 'manga', 'm1'), (3, 'scifi', 's1')`)
	mustExec(`GRANT SELECT, UPDATE, DELETE ON m_disjoint TO merge_rls_user`)
	mustExec(`ALTER TABLE m_disjoint ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY md_sel ON m_disjoint FOR SELECT TO merge_rls_user USING (true)`)
	mustExec(`CREATE POLICY md_upd ON m_disjoint FOR UPDATE TO merge_rls_user USING (cat = 'novel') WITH CHECK (true)`)
	mustExec(`CREATE POLICY md_del ON m_disjoint FOR DELETE TO merge_rls_user USING (cat = 'manga')`)

	mustExec(`SET ROLE merge_rls_user`)
	out = mustExec(`MERGE INTO m_disjoint t
		USING (VALUES (1), (2), (3)) AS s(id)
		ON t.id = s.id
		WHEN MATCHED AND t.cat = 'novel' THEN UPDATE SET val = 'n1-upd'
		WHEN MATCHED AND t.cat = 'manga' THEN DELETE
		WHEN MATCHED THEN DELETE`)
	if out.RowsAffected != 2 {
		t.Fatalf("MERGE disjoint UPDATE+DELETE affected %d rows, want 2", out.RowsAffected)
	}
	mustExec(`RESET ROLE`)
	out = mustExec(`SELECT id, cat, val FROM m_disjoint ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[0][2].StrVal != "n1-upd" || out.Rows[1][0].IntVal != 3 || out.Rows[1][2].StrVal != "s1" {
		t.Fatalf("m_disjoint final rows = %+v, want [(1,novel,n1-upd), (3,scifi,s1)]", out.Rows)
	}
}

func TestPhase6RLSUpdateDeleteSelectDefaultDeny(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	mustExec := func(sqlText string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sqlText, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sqlText, err)
		}
		return out
	}
	requireErrCode := func(sqlText, wantCode string) {
		t.Helper()
		_, err := db.exec(ctx, sqlText, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sqlText, wantCode)
		}
		if got := sqlStateOf(err); got != wantCode {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sqlText, wantCode, got, err)
		}
	}

	mustExec(`CREATE ROLE dml_rls_user`)

	// 1. UPDATE policy ONLY (NO FOR SELECT policy).
	mustExec(`CREATE TABLE u_only (id bigint PRIMARY KEY, v bigint NOT NULL)`)
	mustExec(`INSERT INTO u_only VALUES (1, 10), (2, 20)`)
	mustExec(`GRANT SELECT, UPDATE ON u_only TO dml_rls_user`)
	mustExec(`ALTER TABLE u_only ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY u_allow ON u_only FOR UPDATE TO dml_rls_user USING (true) WITH CHECK (true)`)

	mustExec(`SET ROLE dml_rls_user`)

	// Reads target column in WHERE -> requires SELECT RLS -> default-deny (0 rows affected).
	out := mustExec(`UPDATE u_only SET v = 999 WHERE id = 1`)
	if out.RowsAffected != 0 {
		t.Fatalf("UPDATE WHERE without SELECT policy affected %d rows, want 0", out.RowsAffected)
	}
	// Reads target column in SET expression -> requires SELECT RLS -> default-deny (0 rows affected).
	out = mustExec(`UPDATE u_only SET v = v + 1`)
	if out.RowsAffected != 0 {
		t.Fatalf("UPDATE SET reading column without SELECT policy affected %d rows, want 0", out.RowsAffected)
	}
	// Reads target column in RETURNING -> requires SELECT RLS -> default-deny (0 rows affected / returned).
	out = mustExec(`UPDATE u_only SET v = 888 RETURNING id`)
	if out.RowsAffected != 0 || len(out.Rows) != 0 {
		t.Fatalf("UPDATE RETURNING without SELECT policy leaked rows: affected=%d rows=%+v", out.RowsAffected, out.Rows)
	}

	// Verify rows untouched before blind UPDATE.
	mustExec(`RESET ROLE`)
	out = mustExec(`SELECT id, v FROM u_only ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][1].IntVal != 10 || out.Rows[1][1].IntVal != 20 {
		t.Fatalf("u_only modified despite missing SELECT policy: %+v", out.Rows)
	}

	// Blind UPDATE (no WHERE, no RETURNING, no target column read) only needs UPDATE RLS (PG 18).
	mustExec(`SET ROLE dml_rls_user`)
	out = mustExec(`UPDATE u_only SET v = 42`)
	if out.RowsAffected != 2 {
		t.Fatalf("blind UPDATE without target column reads affected %d rows, want 2", out.RowsAffected)
	}
	mustExec(`RESET ROLE`)
	out = mustExec(`SELECT id, v FROM u_only ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][1].IntVal != 42 || out.Rows[1][1].IntVal != 42 {
		t.Fatalf("u_only after blind UPDATE = %+v, want [(1,42), (2,42)]", out.Rows)
	}

	// 2. DELETE policy ONLY (NO FOR SELECT policy).
	mustExec(`CREATE TABLE d_only (id bigint PRIMARY KEY, v bigint NOT NULL)`)
	mustExec(`INSERT INTO d_only VALUES (1, 10), (2, 20)`)
	mustExec(`GRANT SELECT, DELETE ON d_only TO dml_rls_user`)
	mustExec(`ALTER TABLE d_only ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY d_allow ON d_only FOR DELETE TO dml_rls_user USING (true)`)

	mustExec(`SET ROLE dml_rls_user`)

	// DELETE with WHERE reads target column -> requires SELECT RLS -> default-deny (0 rows affected).
	out = mustExec(`DELETE FROM d_only WHERE id = 1`)
	if out.RowsAffected != 0 {
		t.Fatalf("DELETE WHERE without SELECT policy affected %d rows, want 0", out.RowsAffected)
	}
	// DELETE with RETURNING reads target column -> requires SELECT RLS -> default-deny (0 rows affected / returned).
	out = mustExec(`DELETE FROM d_only RETURNING id`)
	if out.RowsAffected != 0 || len(out.Rows) != 0 {
		t.Fatalf("DELETE RETURNING without SELECT policy leaked rows: affected=%d rows=%+v", out.RowsAffected, out.Rows)
	}

	mustExec(`RESET ROLE`)
	out = mustExec(`SELECT count(*) FROM d_only`)
	if out.Rows[0][0].IntVal != 2 {
		t.Fatalf("d_only row count = %d, want 2", out.Rows[0][0].IntVal)
	}

	// Unqualified DELETE FROM d_only (no target column reads) deletes allowed rows per PG 18.
	mustExec(`SET ROLE dml_rls_user`)
	out = mustExec(`DELETE FROM d_only`)
	if out.RowsAffected != 2 {
		t.Fatalf("blind DELETE without target column reads affected %d rows, want 2", out.RowsAffected)
	}
	mustExec(`RESET ROLE`)
	out = mustExec(`SELECT count(*) FROM d_only`)
	if out.Rows[0][0].IntVal != 0 {
		t.Fatalf("d_only row count after blind DELETE = %d, want 0", out.Rows[0][0].IntVal)
	}

	// 3. SELECT policy ONLY (NO FOR UPDATE / FOR DELETE policy): both UPDATE and DELETE default-deny.
	mustExec(`CREATE TABLE s_only (id bigint PRIMARY KEY, v bigint NOT NULL)`)
	mustExec(`INSERT INTO s_only VALUES (1, 10)`)
	mustExec(`GRANT SELECT, UPDATE, DELETE ON s_only TO dml_rls_user`)
	mustExec(`ALTER TABLE s_only ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY s_allow ON s_only FOR SELECT TO dml_rls_user USING (true)`)

	mustExec(`SET ROLE dml_rls_user`)
	if out := mustExec(`UPDATE s_only SET v = 99`); out.RowsAffected != 0 {
		t.Fatalf("UPDATE without UPDATE policy affected %d rows, want 0", out.RowsAffected)
	}
	if out := mustExec(`UPDATE s_only SET v = 99 WHERE id = 1`); out.RowsAffected != 0 {
		t.Fatalf("UPDATE WHERE without UPDATE policy affected %d rows, want 0", out.RowsAffected)
	}
	if out := mustExec(`DELETE FROM s_only`); out.RowsAffected != 0 {
		t.Fatalf("DELETE without DELETE policy affected %d rows, want 0", out.RowsAffected)
	}
	if out := mustExec(`DELETE FROM s_only WHERE id = 1`); out.RowsAffected != 0 {
		t.Fatalf("DELETE WHERE without DELETE policy affected %d rows, want 0", out.RowsAffected)
	}

	// 4. UPDATE + RETURNING enforces SELECT policy as WITH CHECK on the updated row (PG 18).
	mustExec(`RESET ROLE`)
	mustExec(`CREATE TABLE upd_ret (id bigint PRIMARY KEY, a bigint NOT NULL)`)
	mustExec(`INSERT INTO upd_ret VALUES (1, 10)`)
	mustExec(`GRANT SELECT, UPDATE ON upd_ret TO dml_rls_user`)
	mustExec(`ALTER TABLE upd_ret ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY ur_sel ON upd_ret FOR SELECT TO dml_rls_user USING (a < 20)`)
	mustExec(`CREATE POLICY ur_upd ON upd_ret FOR UPDATE TO dml_rls_user USING (a < 20) WITH CHECK (true)`)

	mustExec(`SET ROLE dml_rls_user`)
	// Without RETURNING or target column reads, updating a to 30 succeeds.
	if out := mustExec(`UPDATE upd_ret SET a = 30`); out.RowsAffected != 1 {
		t.Fatalf("blind UPDATE upd_ret affected %d rows, want 1", out.RowsAffected)
	}
	mustExec(`RESET ROLE`)
	mustExec(`UPDATE upd_ret SET a = 10`)
	mustExec(`SET ROLE dml_rls_user`)
	// With RETURNING, new row a=30 violates SELECT USING (a < 20) -> 42501.
	requireErrCode(`UPDATE upd_ret SET a = 30 RETURNING id, a`, "42501")
}

func TestPhase6RLSOnConflictSelectDefaultDeny(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	mustExec := func(sqlText string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sqlText, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sqlText, err)
		}
		return out
	}
	requireErrCode := func(sqlText, wantCode string) error {
		t.Helper()
		_, err := db.exec(ctx, sqlText, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sqlText, wantCode)
		}
		if got := sqlStateOf(err); got != wantCode {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sqlText, wantCode, got, err)
		}
		return err
	}

	mustExec(`CREATE ROLE upsert_rls_user`)
	mustExec(`CREATE TABLE oc_no_sel (id bigint PRIMARY KEY, val text NOT NULL)`)
	mustExec(`INSERT INTO oc_no_sel VALUES (1, 'secret')`)
	mustExec(`GRANT SELECT, INSERT, UPDATE ON oc_no_sel TO upsert_rls_user`)
	mustExec(`ALTER TABLE oc_no_sel ENABLE ROW LEVEL SECURITY`)
	// INSERT and UPDATE policies exist, but NO SELECT policy!
	mustExec(`CREATE POLICY oc_ins ON oc_no_sel FOR INSERT TO upsert_rls_user WITH CHECK (true)`)
	mustExec(`CREATE POLICY oc_upd ON oc_no_sel FOR UPDATE TO upsert_rls_user USING (true) WITH CHECK (true)`)

	mustExec(`SET ROLE upsert_rls_user`)

	// ON CONFLICT DO UPDATE reading target column in WHERE must fail with 42501 (USING expression).
	err = requireErrCode(`INSERT INTO oc_no_sel VALUES (1, 'probe')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val WHERE oc_no_sel.val = 'secret'`, "42501")
	if !strings.Contains(err.Error(), "USING expression") {
		t.Fatalf("expected USING expression error, got: %v", err)
	}

	// Even with WHERE false, conflict lookup + DO UPDATE requires SELECT USING -> 42501.
	err = requireErrCode(`INSERT INTO oc_no_sel VALUES (1, 'probe')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val WHERE false`, "42501")
	if !strings.Contains(err.Error(), "USING expression") {
		t.Fatalf("expected USING expression error, got: %v", err)
	}

	// Without WHERE/RETURNING, arbiter + DO UPDATE still requires SELECT USING -> 42501 (PG 18).
	requireErrCode(`INSERT INTO oc_no_sel VALUES (1, 'probe')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val`, "42501")

	// With RETURNING on conflicting row -> 42501 (no row leaked).
	requireErrCode(`INSERT INTO oc_no_sel VALUES (1, 'probe')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val RETURNING id, val`, "42501")

	// INSERT ... RETURNING on non-conflicting row also requires SELECT USING -> 42501 (no row leaked).
	requireErrCode(`INSERT INTO oc_no_sel VALUES (99, 'new')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val RETURNING id`, "42501")

	// Verify row 1 was never modified and row 99 was never inserted.
	mustExec(`RESET ROLE`)
	out := mustExec(`SELECT id, val FROM oc_no_sel ORDER BY id`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 1 || out.Rows[0][1].StrVal != "secret" {
		t.Fatalf("oc_no_sel rows = %+v, want [(1,secret)]", out.Rows)
	}

	// Once a SELECT policy exists, visible rows can be updated via ON CONFLICT DO UPDATE,
	// while updating a row into an invisible state fails with 42501 (PG 18).
	mustExec(`CREATE POLICY oc_sel ON oc_no_sel FOR SELECT TO upsert_rls_user USING (val <> 'hidden')`)
	mustExec(`SET ROLE upsert_rls_user`)

	requireErrCode(`INSERT INTO oc_no_sel VALUES (1, 'hidden')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val`, "42501")
	out = mustExec(`INSERT INTO oc_no_sel VALUES (1, 'updated')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val RETURNING id, val`)
	if out.RowsAffected != 1 || len(out.Rows) != 1 || out.Rows[0][1].StrVal != "updated" {
		t.Fatalf("ON CONFLICT DO UPDATE with SELECT policy = affected=%d rows=%+v", out.RowsAffected, out.Rows)
	}
}
