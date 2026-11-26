package supersql

import "testing"

func openPhase3RBACDB(t *testing.T) (func(string) outcome, func(string, string)) {
	t.Helper()
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	mustExec := func(sql string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sql, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sql, err)
		}
		return out
	}

	requireErrCode := func(sql, code string) {
		t.Helper()
		_, err := db.exec(ctx, sql, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sql, code)
		}
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sql, code, got, err)
		}
	}

	return mustExec, requireErrCode
}

func TestPhase3RBACUpdatableViewPrivilegesAndNestedRLS(t *testing.T) {
	mustExec, requireErrCode := openPhase3RBACDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA admin`)
	mustExec(`CREATE TABLE admin.accounts (id int PRIMARY KEY, owner text NOT NULL, balance int NOT NULL)`)
	mustExec(`INSERT INTO admin.accounts VALUES (1, 'admin', 1000), (2, 'alice', 200)`)
	mustExec(`GRANT USAGE ON SCHEMA admin TO alice`)
	mustExec(`GRANT SELECT ON admin.accounts TO alice`)

	// 1. Updatable view privilege escalation attack (single and nested temp views).
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE TEMP VIEW v AS SELECT * FROM admin.accounts`)
	mustExec(`CREATE TEMP VIEW v2 AS SELECT * FROM v`)

	if out := mustExec(`SELECT count(*) FROM v`); out.Rows[0][0].IntVal != 2 {
		t.Fatalf("SELECT v count = %d, want 2", out.Rows[0][0].IntVal)
	}
	if out := mustExec(`SELECT count(*) FROM v2`); out.Rows[0][0].IntVal != 2 {
		t.Fatalf("SELECT v2 count = %d, want 2", out.Rows[0][0].IntVal)
	}

	// alice only has SELECT on admin.accounts: all DML through v and v2 must fail with 42501.
	for _, stmt := range []string{
		`INSERT INTO v VALUES (3, 'alice', 9999)`,
		`INSERT INTO v VALUES (1, 'alice', 9999) ON CONFLICT (id) DO UPDATE SET balance = 9999`,
		`UPDATE v SET balance = 9999`,
		`UPDATE v SET balance = 9999 WHERE id = 1`,
		`DELETE FROM v`,
		`DELETE FROM v WHERE id = 1`,
		`MERGE INTO v USING (VALUES (1, 'alice', 9999), (3, 'alice', 500)) AS s(id, owner, balance) ON v.id = s.id WHEN MATCHED THEN UPDATE SET balance = s.balance WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.owner, s.balance)`,
		`MERGE INTO v USING (VALUES (1)) AS s(id) ON v.id = s.id WHEN MATCHED THEN DELETE`,
		`INSERT INTO v2 VALUES (3, 'alice', 9999)`,
		`INSERT INTO v2 VALUES (1, 'alice', 9999) ON CONFLICT (id) DO UPDATE SET balance = 9999`,
		`UPDATE v2 SET balance = 9999`,
		`UPDATE v2 SET balance = 9999 WHERE id = 1`,
		`DELETE FROM v2`,
		`DELETE FROM v2 WHERE id = 1`,
		`MERGE INTO v2 USING (VALUES (1, 'alice', 9999), (3, 'alice', 500)) AS s(id, owner, balance) ON v2.id = s.id WHEN MATCHED THEN UPDATE SET balance = s.balance WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.owner, s.balance)`,
	} {
		requireErrCode(stmt, "42501")
	}

	// Grant INSERT only (without UPDATE): plain INSERT works, ON CONFLICT DO UPDATE still fails with 42501.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT INSERT ON admin.accounts TO alice`)
	mustExec(`SET ROLE alice`)
	mustExec(`INSERT INTO v2 VALUES (3, 'alice', 300)`)
	requireErrCode(`INSERT INTO v2 VALUES (1, 'alice', 9999) ON CONFLICT (id) DO UPDATE SET balance = 9999`, "42501")
	requireErrCode(`UPDATE v2 SET balance = 1`, "42501")
	requireErrCode(`DELETE FROM v2 WHERE id = 3`, "42501")

	// 2. Admin-owned definer view granted to bob works even though bob has no privileges on admin.accounts.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE VIEW public.admin_definer_v AS SELECT * FROM admin.accounts`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.admin_definer_v TO bob`)

	mustExec(`SET ROLE bob`)
	mustExec(`INSERT INTO public.admin_definer_v VALUES (10, 'bob', 50)`)
	mustExec(`UPDATE public.admin_definer_v SET balance = 75 WHERE id = 10`)
	mustExec(`MERGE INTO public.admin_definer_v USING (VALUES (10, 'bob', 80), (11, 'bob', 90)) AS s(id, owner, balance) ON admin_definer_v.id = s.id WHEN MATCHED THEN UPDATE SET balance = s.balance WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.owner, s.balance)`)
	mustExec(`DELETE FROM public.admin_definer_v WHERE id = 11`)
	if out := mustExec(`SELECT balance FROM public.admin_definer_v WHERE id = 10`); out.Rows[0][0].IntVal != 80 {
		t.Fatalf("definer view balance = %d, want 80", out.Rows[0][0].IntVal)
	}

	// 3. security_invoker = true view checks caller privileges on the underlying table.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE VIEW public.invoker_v WITH (security_invoker = true) AS SELECT * FROM admin.accounts`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.invoker_v TO bob`)
	mustExec(`GRANT USAGE ON SCHEMA admin TO bob`)
	mustExec(`GRANT SELECT ON admin.accounts TO bob`)

	mustExec(`SET ROLE bob`)
	if out := mustExec(`SELECT count(*) FROM public.invoker_v`); out.Rows[0][0].IntVal != 4 {
		t.Fatalf("invoker view SELECT count = %d, want 4", out.Rows[0][0].IntVal)
	}
	requireErrCode(`INSERT INTO public.invoker_v VALUES (20, 'bob', 1)`, "42501")
	requireErrCode(`UPDATE public.invoker_v SET balance = 2 WHERE id = 10`, "42501")
	requireErrCode(`DELETE FROM public.invoker_v WHERE id = 10`, "42501")
	requireErrCode(`MERGE INTO public.invoker_v USING (VALUES (10)) AS s(id) ON invoker_v.id = s.id WHEN MATCHED THEN DELETE`, "42501")

	mustExec(`RESET ROLE`)
	mustExec(`GRANT INSERT, UPDATE, DELETE ON admin.accounts TO bob`)
	mustExec(`SET ROLE bob`)
	mustExec(`INSERT INTO public.invoker_v VALUES (20, 'bob', 1)`)
	mustExec(`UPDATE public.invoker_v SET balance = 2 WHERE id = 20`)
	mustExec(`MERGE INTO public.invoker_v USING (VALUES (20, 'bob', 3)) AS s(id, owner, balance) ON invoker_v.id = s.id WHEN MATCHED THEN UPDATE SET balance = s.balance`)
	mustExec(`DELETE FROM public.invoker_v WHERE id = 20`)

	// 4. Nested view RLS: applyModifyRLS walks all view layers from outer to inner.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT CREATE ON SCHEMA public TO bob`)
	mustExec(`CREATE TABLE public.rls_docs (id int PRIMARY KEY, tenant text NOT NULL, body text)`)
	mustExec(`ALTER TABLE public.rls_docs ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY p_tenant ON public.rls_docs FOR ALL USING (tenant = current_user) WITH CHECK (tenant = current_user)`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.rls_docs TO alice, bob`)

	// Inner view owned by bob (definer = bob).
	mustExec(`SET ROLE bob`)
	mustExec(`CREATE VIEW public.v1_bob AS SELECT * FROM public.rls_docs`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.v1_bob TO alice`)

	// Outer view v2_admin (definer = superuser) -> v1_bob (definer = bob) -> rls_docs.
	// Effective role against rls_docs MUST be bob (NOT superuser).
	mustExec(`RESET ROLE`)
	mustExec(`CREATE VIEW public.v2_admin AS SELECT * FROM public.v1_bob`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.v2_admin TO alice`)

	// Outer view v2_invoker (security_invoker = true) -> v1_bob (definer = bob) -> rls_docs.
	// Effective role against rls_docs MUST be bob (NOT alice).
	mustExec(`CREATE VIEW public.v2_invoker WITH (security_invoker = true) AS SELECT * FROM public.v1_bob`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.v2_invoker TO alice`)

	// Inner view v1_inv (security_invoker = true) under outer view v2_bob_over_inv (definer = bob).
	// Effective role against rls_docs MUST be bob (NOT alice).
	mustExec(`CREATE VIEW public.v1_inv WITH (security_invoker = true) AS SELECT * FROM public.rls_docs`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.v1_inv TO bob`)
	mustExec(`SET ROLE bob`)
	mustExec(`CREATE VIEW public.v2_bob_over_inv AS SELECT * FROM public.v1_inv`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON public.v2_bob_over_inv TO alice`)

	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	// v2_admin: effective role is bob, so tenant='alice' violates WITH CHECK while tenant='bob' succeeds.
	requireErrCode(`INSERT INTO public.v2_admin VALUES (1, 'alice', 'denied')`, "42501")
	mustExec(`INSERT INTO public.v2_admin VALUES (2, 'bob', 'allowed')`)
	// v2_invoker: effective role is bob, so tenant='alice' violates WITH CHECK while tenant='bob' succeeds.
	requireErrCode(`INSERT INTO public.v2_invoker VALUES (3, 'alice', 'denied')`, "42501")
	mustExec(`INSERT INTO public.v2_invoker VALUES (4, 'bob', 'allowed')`)
	// v2_bob_over_inv: effective role is bob, so tenant='alice' violates WITH CHECK while tenant='bob' succeeds.
	requireErrCode(`INSERT INTO public.v2_bob_over_inv VALUES (5, 'alice', 'denied')`, "42501")
	mustExec(`INSERT INTO public.v2_bob_over_inv VALUES (6, 'bob', 'allowed')`)
	// UPDATE through nested view also enforces WITH CHECK under effective role bob.
	requireErrCode(`UPDATE public.v2_admin SET tenant = 'alice' WHERE id = 2`, "42501")
	mustExec(`UPDATE public.v2_admin SET body = 'updated' WHERE id = 2`)
}

func TestPhase3RBACInheritanceAndPartitionOwnership(t *testing.T) {
	mustExec, requireErrCode := openPhase3RBACDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA secret`)
	mustExec(`CREATE SCHEMA work`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA work TO alice, bob`)

	mustExec(`CREATE TABLE secret.base (id int, val text)`)
	mustExec(`CREATE TABLE secret.part_parent (id int, val text) PARTITION BY RANGE (id)`)
	mustExec(`CREATE TABLE secret.bob_hidden (id int, val text)`)
	mustExec(`ALTER TABLE secret.bob_hidden OWNER TO bob`)

	mustExec(`SET ROLE bob`)
	mustExec(`CREATE TABLE work.bob_base (id int, val text)`)
	mustExec(`CREATE TABLE work.bob_part (id int, val text) PARTITION BY RANGE (id)`)
	mustExec(`GRANT ALL ON TABLE work.bob_base, work.bob_part TO alice`)

	// Non-owner fails with 42501 without schema USAGE and also with schema USAGE / ALL table privileges.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TABLE work.c1 () INHERITS (secret.base)`, "42501")
	requireErrCode(`CREATE TABLE work.p1 PARTITION OF secret.part_parent FOR VALUES FROM (0) TO (10)`, "42501")
	requireErrCode(`CREATE TABLE work.c2 () INHERITS (work.bob_base)`, "42501")
	requireErrCode(`CREATE TABLE work.p2 PARTITION OF work.bob_part FOR VALUES FROM (0) TO (10)`, "42501")

	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SCHEMA secret TO alice`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TABLE work.c1 () INHERITS (secret.base)`, "42501")
	requireErrCode(`CREATE TABLE work.p1 PARTITION OF secret.part_parent FOR VALUES FROM (0) TO (10)`, "42501")

	// Owner lacking USAGE on parent's schema fails with 42501; succeeds once USAGE is granted.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`CREATE TABLE work.bob_hidden_child () INHERITS (secret.bob_hidden)`, "42501")
	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SCHEMA secret TO bob`)
	mustExec(`SET ROLE bob`)
	mustExec(`CREATE TABLE work.bob_hidden_child () INHERITS (secret.bob_hidden)`)

	// Owner succeeds for both INHERITS and PARTITION OF.
	mustExec(`CREATE TABLE work.bob_child () INHERITS (work.bob_base)`)
	mustExec(`CREATE TABLE work.bob_p1 PARTITION OF work.bob_part FOR VALUES FROM (0) TO (10)`)
}

func TestPhase3RBACRoutineLifecycleAuthorization(t *testing.T) {
	mustExec, requireErrCode := openPhase3RBACDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA s1`)
	mustExec(`CREATE SCHEMA s2`)
	mustExec(`CREATE SCHEMA s3`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s1 TO alice`)
	mustExec(`GRANT USAGE ON SCHEMA s1 TO bob`)
	mustExec(`GRANT USAGE ON SCHEMA s2 TO alice`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s3 TO alice`)

	mustExec(`SET ROLE alice`)
	mustExec(`CREATE FUNCTION s1.fn_alice(x int) RETURNS int LANGUAGE sql AS 'SELECT x + 1'`)
	mustExec(`CREATE PROCEDURE s1.proc_alice() LANGUAGE plpgsql AS $$ BEGIN END; $$`)

	// Non-owner fails 42501 on DROP, RENAME, and SET SCHEMA.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`DROP FUNCTION s1.fn_alice(int)`, "42501")
	requireErrCode(`DROP PROCEDURE s1.proc_alice()`, "42501")
	requireErrCode(`ALTER FUNCTION s1.fn_alice(int) RENAME TO fn_stolen`, "42501")
	requireErrCode(`ALTER PROCEDURE s1.proc_alice() RENAME TO proc_stolen`, "42501")
	requireErrCode(`ALTER FUNCTION s1.fn_alice(int) SET SCHEMA s3`, "42501")
	requireErrCode(`ALTER PROCEDURE s1.proc_alice() SET SCHEMA s3`, "42501")

	// Owner fails 42501 on SET SCHEMA without destination schema CREATE; succeeds with CREATE.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`ALTER FUNCTION s1.fn_alice(int) SET SCHEMA s2`, "42501")
	requireErrCode(`ALTER PROCEDURE s1.proc_alice() SET SCHEMA s2`, "42501")

	mustExec(`ALTER FUNCTION s1.fn_alice(int) RENAME TO fn_alice2`)
	mustExec(`ALTER FUNCTION s1.fn_alice2(int) SET SCHEMA s3`)
	mustExec(`DROP FUNCTION s3.fn_alice2(int)`)

	mustExec(`ALTER PROCEDURE s1.proc_alice() RENAME TO proc_alice2`)
	mustExec(`ALTER PROCEDURE s1.proc_alice2() SET SCHEMA s3`)
	mustExec(`DROP PROCEDURE s3.proc_alice2()`)
}

func TestPhase3RBACForeignKeyReferencesPrivilege(t *testing.T) {
	mustExec, requireErrCode := openPhase3RBACDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE SCHEMA ref_s`)
	mustExec(`CREATE SCHEMA app_s`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA app_s TO alice`)

	mustExec(`CREATE TABLE ref_s.parents (id int PRIMARY KEY)`)

	mustExec(`SET ROLE alice`)
	mustExec(`CREATE TABLE app_s.child_alter (id int PRIMARY KEY, parent_id int)`)

	// Denied without schema USAGE or REFERENCES.
	requireErrCode(`CREATE TABLE app_s.c1 (id int PRIMARY KEY, pid int REFERENCES ref_s.parents(id))`, "42501")
	requireErrCode(`ALTER TABLE app_s.child_alter ADD CONSTRAINT fk1 FOREIGN KEY (parent_id) REFERENCES ref_s.parents(id)`, "42501")
	requireErrCode(`ALTER TABLE app_s.child_alter ADD COLUMN pid2 int REFERENCES ref_s.parents(id)`, "42501")

	// Schema USAGE + SELECT alone is not enough (REFERENCES is required).
	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SCHEMA ref_s TO alice`)
	mustExec(`GRANT SELECT ON TABLE ref_s.parents TO alice`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TABLE app_s.c1 (id int PRIMARY KEY, pid int REFERENCES ref_s.parents(id))`, "42501")
	requireErrCode(`ALTER TABLE app_s.child_alter ADD CONSTRAINT fk1 FOREIGN KEY (parent_id) REFERENCES ref_s.parents(id)`, "42501")
	requireErrCode(`ALTER TABLE app_s.child_alter ADD COLUMN pid2 int REFERENCES ref_s.parents(id)`, "42501")

	// REFERENCES without schema USAGE is still denied.
	mustExec(`RESET ROLE`)
	mustExec(`REVOKE USAGE ON SCHEMA ref_s FROM alice`)
	mustExec(`GRANT REFERENCES ON TABLE ref_s.parents TO alice`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TABLE app_s.c1 (id int PRIMARY KEY, pid int REFERENCES ref_s.parents(id))`, "42501")
	requireErrCode(`ALTER TABLE app_s.child_alter ADD CONSTRAINT fk1 FOREIGN KEY (parent_id) REFERENCES ref_s.parents(id)`, "42501")

	// Both schema USAGE and REFERENCES granted -> succeeds for CREATE TABLE and ALTER TABLE.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SCHEMA ref_s TO alice`)
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE TABLE app_s.c1 (id int PRIMARY KEY, pid int REFERENCES ref_s.parents(id))`)
	mustExec(`ALTER TABLE app_s.child_alter ADD CONSTRAINT fk1 FOREIGN KEY (parent_id) REFERENCES ref_s.parents(id)`)
	mustExec(`ALTER TABLE app_s.child_alter ADD COLUMN pid2 int REFERENCES ref_s.parents(id)`)

	// Self-referencing FK on owned table works.
	mustExec(`CREATE TABLE app_s.self_fk (id int PRIMARY KEY, parent_id int REFERENCES app_s.self_fk(id))`)
}
