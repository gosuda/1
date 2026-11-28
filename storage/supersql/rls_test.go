package supersql

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestMultiTenantGUCAndRowLevelSecurity(t *testing.T) {
	ctx := t.Context()
	dir := testDirectory(t)
	testClock := newTestClock()

	db, err := Open(ctx, OpenOptions{Directory: dir, Clock: testClock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()

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
		if got := sqlStateOf(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sql, code, got, err)
		}
	}

	// 1. Setup multi-tenant schema, roles, and RLS policy.
	mustExec(`CREATE TABLE tenant_docs (
		id bigint PRIMARY KEY,
		tenant_id text NOT NULL,
		title text NOT NULL,
		active boolean NOT NULL DEFAULT true
	)`)
	mustExec(`INSERT INTO tenant_docs VALUES
		(1, 'tenant_a', 'A-doc-1', true),
		(2, 'tenant_a', 'A-doc-2', false),
		(3, 'tenant_b', 'B-doc-1', true)`)

	mustExec(`CREATE ROLE tenant_user`)
	mustExec(`CREATE ROLE bypass_admin BYPASSRLS`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_docs TO tenant_user`)
	mustExec(`GRANT SELECT ON tenant_docs TO bypass_admin`)
	mustExec(`ALTER TABLE tenant_docs ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY tenant_isolation ON tenant_docs
		FOR ALL TO tenant_user
		USING (tenant_id = current_setting('app.tenant_id', true))
		WITH CHECK (tenant_id = current_setting('app.tenant_id', true))`)

	// 2. Table owner ('supersql') bypasses RLS by default when FORCE is not set.
	out := mustExec(`SELECT count(*) FROM tenant_docs`)
	if out.Rows[0][0].IntVal != 3 {
		t.Fatalf("owner count = %d, want 3", out.Rows[0][0].IntVal)
	}

	// 3. Switch to tenant_user and verify role introspection and GUC isolation.
	mustExec(`SET ROLE tenant_user`)
	out = mustExec(`SHOW role`)
	if len(out.Rows) != 1 || out.Rows[0][0].StrVal != "tenant_user" {
		t.Fatalf("SHOW role = %+v, want tenant_user", out.Rows)
	}
	out = mustExec(`SELECT current_user, session_user, current_role`)
	if out.Rows[0][0].StrVal != "tenant_user" || out.Rows[0][1].StrVal != "embedded" || out.Rows[0][2].StrVal != "tenant_user" {
		t.Fatalf("role functions = %+v", out.Rows[0])
	}

	// Without app.tenant_id set, current_setting('app.tenant_id', true) is NULL, so 0 rows visible.
	out = mustExec(`SELECT id FROM tenant_docs ORDER BY id`)
	if len(out.Rows) != 0 {
		t.Fatalf("unset tenant_id rows = %d, want 0", len(out.Rows))
	}

	// Set app.tenant_id = 'tenant_a' inside a transaction using SET LOCAL.
	mustExec(`BEGIN`)
	mustExec(`SET LOCAL app.tenant_id = 'tenant_a'`)
	out = mustExec(`SHOW app.tenant_id`)
	if out.Rows[0][0].StrVal != "tenant_a" {
		t.Fatalf("SHOW app.tenant_id = %q, want tenant_a", out.Rows[0][0].StrVal)
	}
	out = mustExec(`SELECT id, title FROM tenant_docs ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 2 {
		t.Fatalf("tenant_a visible rows = %+v, want ids [1, 2]", out.Rows)
	}

	// INSERT for tenant_a succeeds; INSERT for tenant_b violates WITH CHECK (42501).
	mustExec(`INSERT INTO tenant_docs VALUES (4, 'tenant_a', 'A-doc-3', true)`)
	mustExec(`COMMIT`)

	mustExec(`SET app.tenant_id = 'tenant_a'`)
	requireErrCode(`INSERT INTO tenant_docs VALUES (5, 'tenant_b', 'B-hijack', true)`, "42501")

	// Cross-tenant UPDATE and DELETE are filtered by USING and checked by WITH CHECK.
	out = mustExec(`UPDATE tenant_docs SET title = 'hijacked' WHERE id = 3`)
	if out.RowsAffected != 0 {
		t.Fatalf("cross-tenant UPDATE affected %d rows, want 0", out.RowsAffected)
	}
	requireErrCode(`UPDATE tenant_docs SET tenant_id = 'tenant_b' WHERE id = 1`, "42501")
	out = mustExec(`DELETE FROM tenant_docs WHERE id = 3`)
	if out.RowsAffected != 0 {
		t.Fatalf("cross-tenant DELETE affected %d rows, want 0", out.RowsAffected)
	}

	// 4. Add a RESTRICTIVE policy requiring active = true.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE POLICY active_docs_only ON tenant_docs AS RESTRICTIVE
		FOR SELECT TO tenant_user
		USING (active = true)`)
	mustExec(`SET ROLE tenant_user`)
	out = mustExec(`SELECT id FROM tenant_docs ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 4 {
		t.Fatalf("restrictive policy rows = %+v, want ids [1, 4]", out.Rows)
	}

	// 5. BYPASSRLS role sees all rows regardless of tenant_id.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bypass_admin`)
	out = mustExec(`SELECT count(*) FROM tenant_docs`)
	if out.Rows[0][0].IntVal != 4 {
		t.Fatalf("bypass_admin count = %d, want 4", out.Rows[0][0].IntVal)
	}

	// 6. FORCE ROW LEVEL SECURITY applies RLS to non-superuser table owners,
	// while superusers always bypass RLS (PostgreSQL 18 semantics).
	mustExec(`RESET ROLE`)
	mustExec(`CREATE ROLE doc_owner`)
	mustExec(`ALTER TABLE tenant_docs OWNER TO doc_owner`)
	mustExec(`ALTER TABLE tenant_docs FORCE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY owner_tenant_policy ON tenant_docs
		FOR ALL TO doc_owner
		USING (tenant_id = current_setting('app.tenant_id', true))`)
	mustExec(`SET app.tenant_id = 'tenant_b'`)
	// Superuser still sees all 4 rows even under FORCE ROW LEVEL SECURITY.
	out = mustExec(`SELECT count(*) FROM tenant_docs`)
	if out.Rows[0][0].IntVal != 4 {
		t.Fatalf("superuser under FORCE RLS count = %d, want 4", out.Rows[0][0].IntVal)
	}
	// Non-superuser table owner IS subject to FORCE ROW LEVEL SECURITY.
	mustExec(`SET ROLE doc_owner`)
	out = mustExec(`SELECT id FROM tenant_docs`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 3 {
		t.Fatalf("forced owner rows = %+v, want [3]", out.Rows)
	}
	mustExec(`RESET ROLE`)

	// 7. Reopen database and verify RLS policies and roles persist across restarts.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db = nil
	db, err = Open(ctx, OpenOptions{Directory: dir, Clock: testClock})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	mustExec(`SET ROLE tenant_user`)
	mustExec(`SET app.tenant_id = 'tenant_a'`)
	out = mustExec(`SELECT id FROM tenant_docs ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 4 {
		t.Fatalf("after reopen tenant_a rows = %+v, want [1, 4]", out.Rows)
	}
}

func TestInformationSchemaSQLStandardsPopulated(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for _, table := range []string{"sql_features", "sql_parts", "sql_sizing"} {
		out, err := db.exec(ctx, "SELECT count(*) FROM information_schema."+table, 0)
		if err != nil {
			t.Fatalf("SELECT count(*) FROM information_schema.%s: %v", table, err)
		}
		if len(out.Rows) != 1 || out.Rows[0][0].IntVal <= 0 {
			t.Fatalf("information_schema.%s is empty (count = %+v)", table, out.Rows)
		}
	}
}

func TestRLSProductionCompleteness(t *testing.T) {
	ctx := t.Context()
	dir := testDirectory(t)
	testClock := newTestClock()

	db, err := Open(ctx, OpenOptions{Directory: dir, Clock: testClock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()

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
		if got := sqlStateOf(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sql, code, got, err)
		}
	}

	mustExec(`CREATE ROLE app_user`)
	mustExec(`CREATE ROLE admin_user BYPASSRLS`)

	// 1. DROP TABLE cleans up RLS state so a recreated table does not inherit old policies.
	mustExec(`CREATE TABLE ephemeral_docs (id bigint PRIMARY KEY, tenant_id text NOT NULL)`)
	mustExec(`ALTER TABLE ephemeral_docs ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY eph_pol ON ephemeral_docs FOR ALL TO app_user USING (tenant_id = 'only_a')`)
	mustExec(`DROP TABLE ephemeral_docs`)
	mustExec(`CREATE TABLE ephemeral_docs (id bigint PRIMARY KEY, tenant_id text NOT NULL)`)
	mustExec(`INSERT INTO ephemeral_docs VALUES (1, 'tenant_b')`)
	mustExec(`GRANT SELECT ON ephemeral_docs TO app_user`)
	mustExec(`SET ROLE app_user`)
	out := mustExec(`SELECT count(*) FROM ephemeral_docs`)
	if out.Rows[0][0].IntVal != 1 {
		t.Fatalf("recreated table count = %d, want 1 (stale RLS policy not cleared on DROP TABLE)", out.Rows[0][0].IntVal)
	}
	mustExec(`RESET ROLE`)
	mustExec(`DROP TABLE ephemeral_docs`)

	// 2. ALTER TABLE ... RENAME TO ... migrates RLS configuration and policies.
	mustExec(`CREATE TABLE org_secrets (id bigint PRIMARY KEY, tenant_id text NOT NULL, secret text NOT NULL)`)
	mustExec(`INSERT INTO org_secrets VALUES (1, 't1', 's1'), (2, 't2', 's2')`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON org_secrets TO app_user`)
	mustExec(`ALTER TABLE org_secrets ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY org_isolation ON org_secrets
		FOR ALL TO app_user
		USING (tenant_id = current_setting('app.tenant_id', true))
		WITH CHECK (tenant_id = current_setting('app.tenant_id', true))`)
	mustExec(`ALTER TABLE org_secrets RENAME TO tenant_secrets`)

	mustExec(`SET ROLE app_user`)
	mustExec(`SET app.tenant_id = 't1'`)
	out = mustExec(`SELECT id, secret FROM tenant_secrets ORDER BY id`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 1 {
		t.Fatalf("renamed table rows = %+v, want [(1, s1)]", out.Rows)
	}

	// 3. SubLink subqueries (WHERE EXISTS, IN, scalar SELECT subquery) enforce RLS.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE TABLE public_projects (id bigint PRIMARY KEY, secret_id bigint NOT NULL)`)
	mustExec(`INSERT INTO public_projects VALUES (10, 1), (20, 2)`)
	mustExec(`GRANT SELECT ON public_projects TO app_user`)

	mustExec(`SET ROLE app_user`)
	mustExec(`SET app.tenant_id = 't1'`)
	out = mustExec(`SELECT id FROM public_projects p WHERE EXISTS (SELECT 1 FROM tenant_secrets s WHERE s.id = p.secret_id) ORDER BY id`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 10 {
		t.Fatalf("WHERE EXISTS rows = %+v, want [10]", out.Rows)
	}
	out = mustExec(`SELECT (SELECT count(*) FROM tenant_secrets)`)
	if out.Rows[0][0].IntVal != 1 {
		t.Fatalf("scalar subquery count = %d, want 1", out.Rows[0][0].IntVal)
	}

	// 4. INSERT ... ON CONFLICT DO UPDATE enforces USING and WITH CHECK.
	// Updating own tenant's row via ON CONFLICT succeeds:
	mustExec(`INSERT INTO tenant_secrets VALUES (1, 't1', 's1-updated')
		ON CONFLICT (id) DO UPDATE SET secret = EXCLUDED.secret`)
	out = mustExec(`SELECT secret FROM tenant_secrets WHERE id = 1`)
	if out.Rows[0][0].StrVal != "s1-updated" {
		t.Fatalf("ON CONFLICT DO UPDATE own row = %q, want s1-updated", out.Rows[0][0].StrVal)
	}
	// Attempting to overwrite another tenant's row (id = 2, tenant_id = 't2') violates UPDATE/SELECT USING (42501):
	requireErrCode(`INSERT INTO tenant_secrets VALUES (2, 't1', 'hijacked')
		ON CONFLICT (id) DO UPDATE SET secret = EXCLUDED.secret`, "42501")
	// Attempting to change own row's tenant_id to 't2' via ON CONFLICT DO UPDATE violates WITH CHECK (42501):
	requireErrCode(`INSERT INTO tenant_secrets VALUES (1, 't1', 's1')
		ON CONFLICT (id) DO UPDATE SET tenant_id = 't2'`, "42501")

	// 5. Querying pg_policies and pg_roles when RLS is active.
	out = mustExec(`SELECT schemaname, tablename, policyname, permissive, cmd FROM pg_policies ORDER BY policyname`)
	if len(out.Rows) != 1 || out.Rows[0][1].StrVal != "tenant_secrets" || out.Rows[0][2].StrVal != "org_isolation" || out.Rows[0][3].StrVal != "PERMISSIVE" || out.Rows[0][4].StrVal != "ALL" {
		t.Fatalf("pg_policies rows = %+v", out.Rows)
	}
	out = mustExec(`SELECT rolname, rolbypassrls FROM pg_roles ORDER BY rolname`)
	if len(out.Rows) != 3 || out.Rows[0][0].StrVal != "admin_user" || out.Rows[0][1].IntVal == 0 || out.Rows[1][0].StrVal != "app_user" || out.Rows[1][1].IntVal != 0 {
		t.Fatalf("pg_roles rows = %+v", out.Rows)
	}
}

func TestRLSSecurityBypassMatrix(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

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
		if got := sqlStateOf(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sql, code, got, err)
		}
	}

	mustExec(`CREATE TABLE rls_items (
		id bigint PRIMARY KEY,
		tenant_id text NOT NULL,
		label text NOT NULL
	)`)
	mustExec(`INSERT INTO rls_items VALUES (1, 't1', 'one'), (2, 't2', 'two')`)
	mustExec(`CREATE TABLE public_lookup (id bigint PRIMARY KEY, name text NOT NULL)`)
	mustExec(`INSERT INTO public_lookup VALUES (1, 'lookup-1'), (2, 'lookup-2'), (3, 'lookup-3')`)

	mustExec(`CREATE ROLE tenant_role`)
	mustExec(`CREATE ROLE worker_role`)
	mustExec(`CREATE ROLE ungranted_role`)
	mustExec(`ALTER TABLE rls_items ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY rls_items_pol ON rls_items
		FOR ALL TO PUBLIC
		USING (tenant_id = current_setting('app.tenant_id', true))
		WITH CHECK (tenant_id = current_setting('app.tenant_id', true))`)

	// S3: Object-level GRANT/REVOKE works and enforces real ACL privileges.
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON rls_items TO tenant_role`)
	mustExec(`GRANT SELECT ON public_lookup TO tenant_role`)
	mustExec(`GRANT CREATE ON SCHEMA public TO tenant_role`)

	// S2: Role membership and privilege checks.
	mustExec(`GRANT tenant_role TO worker_role`)
	// Prepare a statement while superuser to verify EXECUTE evaluates RLS dynamically under the caller's role.
	mustExec(`PREPARE p_items AS SELECT id, label FROM rls_items ORDER BY id`)
	mustExec(`CREATE FUNCTION fn_sql_count() RETURNS bigint LANGUAGE sql AS 'SELECT count(*) FROM rls_items'`)
	mustExec(`CREATE FUNCTION fn_plpgsql_insert(p_id bigint, p_tenant text, p_label text) RETURNS bigint LANGUAGE plpgsql AS $$
	BEGIN
		INSERT INTO rls_items VALUES (p_id, p_tenant, p_label);
		RETURN (SELECT count(*) FROM rls_items);
	END;
	$$`)

	// Bind session_user to worker_role (non-superuser member of tenant_role, not ungranted_role or postgres).
	if err := db.embeddedScope().settings.SetSessionUser(ctx, "worker_role", false); err != nil {
		t.Fatalf("SetSessionUser(worker_role): %v", err)
	}
	requireErrCode(`SET ROLE postgres`, "42501")
	requireErrCode(`SET ROLE ungranted_role`, "42501")
	requireErrCode(`CREATE ROLE escalated SUPERUSER`, "42501")
	requireErrCode(`ALTER ROLE worker_role BYPASSRLS`, "42501")
	requireErrCode(`DROP ROLE ungranted_role`, "42501")
	requireErrCode(`ALTER TABLE rls_items DISABLE ROW LEVEL SECURITY`, "42501")
	requireErrCode(`DROP POLICY rls_items_pol ON rls_items`, "42501")
	requireErrCode(`CREATE POLICY hijack_pol ON rls_items USING (true)`, "42501")

	// worker_role has membership in tenant_role, so SET ROLE tenant_role succeeds.
	mustExec(`SET ROLE tenant_role`)
	mustExec(`SET app.tenant_id = 't1'`)

	// S4: Fail-closed on unresolved/non-existent relation when RLS is active.
	requireErrCode(`SELECT * FROM nonexistent_rls_table`, "42P01")

	// S6: Outer join preserves unmatched/RLS-filtered rows on the nullable side with NULL columns.
	out := mustExec(`SELECT l.id, r.label FROM public_lookup l LEFT JOIN rls_items r ON l.id = r.id ORDER BY l.id`)
	if len(out.Rows) != 3 {
		t.Fatalf("LEFT JOIN row count = %d, want 3 (rows: %+v)", len(out.Rows), out.Rows)
	}
	if out.Rows[0][0].IntVal != 1 || out.Rows[0][1].StrVal != "one" || !out.Rows[1][1].IsNull || !out.Rows[2][1].IsNull {
		t.Fatalf("LEFT JOIN rows = %+v, want [(1,'one'), (2,NULL), (3,NULL)]", out.Rows)
	}

	// S1(a): PostgreSQL 18 supports direct COPY rel TO STDOUT under RLS,
	// enforcing row_security = off (42501) and filtering visible rows when ON,
	// matching COPY (SELECT * FROM rel) TO STDOUT.
	var copyBuf bytes.Buffer
	copyOut, err := db.CopyTo(ctx, `COPY rls_items TO STDOUT`, &copyBuf, CopyOptions{MaxRecordBytes: 1 << 20})
	if err != nil {
		t.Fatalf("direct CopyTo under RLS: %v", err)
	}
	if copyOut.RowsAffected != 1 || strings.TrimSpace(copyBuf.String()) != "1\tt1\tone" {
		t.Fatalf("direct CopyTo under RLS = %d rows %q, want 1 row '1\\tt1\\tone'", copyOut.RowsAffected, copyBuf.String())
	}
	mustExec(`SET row_security = off`)
	copyBuf.Reset()
	if _, err := db.CopyTo(ctx, `COPY rls_items TO STDOUT`, &copyBuf, CopyOptions{MaxRecordBytes: 1 << 20}); sqlStateOf(err) != "42501" {
		t.Fatalf("direct CopyTo with row_security=off: expected 42501, got %v", err)
	}
	mustExec(`SET row_security = on`)
	copyBuf.Reset()
	copyOut, err = db.CopyTo(ctx, `COPY (SELECT * FROM rls_items) TO STDOUT`, &copyBuf, CopyOptions{MaxRecordBytes: 1 << 20})
	if err != nil {
		t.Fatalf("CopyTo query under RLS: %v", err)
	}
	if copyOut.RowsAffected != 1 || strings.TrimSpace(copyBuf.String()) != "1\tt1\tone" {
		t.Fatalf("CopyTo query under RLS = %d rows %q, want 1 row '1\\tt1\\tone'", copyOut.RowsAffected, copyBuf.String())
	}

	// S1(b): COPY FROM STDIN allows own tenant row and rejects cross-tenant row with 42501.
	copyIn, err := db.CopyFrom(ctx, `COPY rls_items FROM STDIN`, strings.NewReader("10\tt1\tten\n"), CopyOptions{MaxRecordBytes: 1 << 20})
	if err != nil || copyIn.RowsAffected != 1 {
		t.Fatalf("CopyFrom own tenant: out=%+v err=%v", copyIn, err)
	}
	_, err = db.CopyFrom(ctx, `COPY rls_items FROM STDIN`, strings.NewReader("20\tt2\thijack\n"), CopyOptions{MaxRecordBytes: 1 << 20})
	if got := sqlStateOf(err); got != "42501" {
		t.Fatalf("CopyFrom cross-tenant: expected 42501, got %q (%v)", got, err)
	}

	// S1(c): MERGE INTO enforces USING on target and WITH CHECK on inserted/updated rows.
	mustExec(`CREATE TABLE merge_src (id bigint PRIMARY KEY, tenant_id text NOT NULL, label text NOT NULL)`)
	mustExec(`INSERT INTO merge_src VALUES (1, 't1', 'one-merged'), (2, 't1', 'two-hijack')`)
	// Row id=2 belongs to t2 in rls_items, so under USING (tenant_id = 't1') it is not matched;
	// WHEN NOT MATCHED then tries to insert id=2 with tenant_id='t1' (which hits PK conflict 23505, proving id=2 was not matched for update).
	out = mustExec(`MERGE INTO rls_items t
		USING (SELECT id, tenant_id, label FROM merge_src WHERE id = 1) s
		ON t.id = s.id
		WHEN MATCHED THEN UPDATE SET label = s.label`)
	if out.RowsAffected != 1 {
		t.Fatalf("MERGE own row affected = %d, want 1", out.RowsAffected)
	}
	requireErrCode(`MERGE INTO rls_items t
		USING (SELECT 1 AS id, 't2' AS tenant_id, 'bad' AS label) s
		ON t.id = s.id
		WHEN MATCHED THEN UPDATE SET tenant_id = s.tenant_id`, "42501")
	requireErrCode(`MERGE INTO rls_items t
		USING (SELECT 99 AS id, 't2' AS tenant_id, 'bad' AS label) s
		ON t.id = s.id
		WHEN NOT MATCHED THEN INSERT (id, tenant_id, label) VALUES (s.id, s.tenant_id, s.label)`, "42501")

	// S1(d): PREPARE / EXECUTE evaluates RLS dynamically at EXECUTE time.
	out = mustExec(`EXECUTE p_items`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 10 {
		t.Fatalf("EXECUTE p_items for t1 = %+v, want ids [1, 10]", out.Rows)
	}
	mustExec(`SET app.tenant_id = 't2'`)
	out = mustExec(`EXECUTE p_items`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 2 {
		t.Fatalf("EXECUTE p_items for t2 = %+v, want id [2]", out.Rows)
	}
	mustExec(`SET app.tenant_id = 't1'`)

	// S1(e): DECLARE CURSOR / FETCH enforces RLS.
	mustExec(`BEGIN`)
	mustExec(`DECLARE c_items CURSOR FOR SELECT id FROM rls_items ORDER BY id`)
	out = mustExec(`FETCH ALL FROM c_items`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 10 {
		t.Fatalf("FETCH ALL cursor rows = %+v, want [1, 10]", out.Rows)
	}
	mustExec(`CLOSE c_items`)
	mustExec(`COMMIT`)

	// S1(f): CREATE TABLE AS / CREATE MATERIALIZED VIEW / REFRESH MATERIALIZED VIEW enforce RLS.
	mustExec(`CREATE TABLE ctas_copy AS SELECT id, tenant_id, label FROM rls_items ORDER BY id`)
	out = mustExec(`SELECT count(*) FROM ctas_copy`)
	if out.Rows[0][0].IntVal != 2 {
		t.Fatalf("CTAS row count = %d, want 2", out.Rows[0][0].IntVal)
	}
	mustExec(`CREATE MATERIALIZED VIEW mv_copy AS SELECT id, tenant_id, label FROM rls_items`)
	out = mustExec(`SELECT count(*) FROM mv_copy`)
	if out.Rows[0][0].IntVal != 2 {
		t.Fatalf("MATVIEW row count = %d, want 2", out.Rows[0][0].IntVal)
	}
	mustExec(`SET app.tenant_id = 't2'`)
	mustExec(`REFRESH MATERIALIZED VIEW mv_copy`)
	out = mustExec(`SELECT count(*) FROM mv_copy`)
	if out.Rows[0][0].IntVal != 1 {
		t.Fatalf("REFRESH MATVIEW row count for t2 = %d, want 1", out.Rows[0][0].IntVal)
	}
	mustExec(`SET app.tenant_id = 't1'`)

	// S1(g): SQL and PL/pgSQL functions enforce RLS on internal statements.
	out = mustExec(`SELECT fn_sql_count()`)
	if out.Rows[0][0].IntVal != 2 {
		t.Fatalf("fn_sql_count() for t1 = %d, want 2", out.Rows[0][0].IntVal)
	}
	out = mustExec(`SELECT fn_plpgsql_insert(11, 't1', 'eleven')`)
	if out.Rows[0][0].IntVal != 3 {
		t.Fatalf("fn_plpgsql_insert own tenant count = %d, want 3", out.Rows[0][0].IntVal)
	}
	requireErrCode(`SELECT fn_plpgsql_insert(21, 't2', 'hijack')`, "42501")
}

// An RLS-protected relation on the nullable side of an outer join hides its
// filtered rows without dropping the preserved side's unmatched rows, and an
// RLS-protected preserved side keeps only its visible rows.
func TestRLSOuterJoinSemantics(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	mustExec := func(sql string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sql, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sql, err)
		}
		return out
	}
	mustExec(`CREATE TABLE oj_items (id bigint PRIMARY KEY, tenant_id text NOT NULL, label text NOT NULL)`)
	mustExec(`INSERT INTO oj_items VALUES (1, 't1', 'one'), (2, 't2', 'two'), (4, 't1', 'four')`)
	mustExec(`CREATE TABLE oj_lookup (id bigint PRIMARY KEY)`)
	mustExec(`INSERT INTO oj_lookup VALUES (1), (2), (3)`)
	mustExec(`CREATE ROLE oj_tenant`)
	mustExec(`GRANT SELECT ON oj_items, oj_lookup TO oj_tenant`)
	mustExec(`ALTER TABLE oj_items ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY oj_pol ON oj_items FOR ALL TO PUBLIC USING (tenant_id = current_setting('app.tenant_id', true))`)
	mustExec(`SET ROLE oj_tenant`)
	mustExec(`SET app.tenant_id = 't1'`)

	render := func(rows [][]model.Value) string {
		var b strings.Builder
		for _, row := range rows {
			b.WriteByte('(')
			for i, v := range row {
				if i > 0 {
					b.WriteByte(',')
				}
				switch {
				case v.IsNull:
					b.WriteString("NULL")
				case v.StrVal != "":
					b.WriteString(v.StrVal)
				default:
					b.WriteString(itoa64(v.IntVal))
				}
			}
			b.WriteByte(')')
		}
		return b.String()
	}
	for query, want := range map[string]string{
		// Protected relation on the nullable side: id 2 is hidden, ids 2 and 3 stay NULL-extended.
		`SELECT l.id, r.label FROM oj_lookup l LEFT JOIN oj_items r ON l.id = r.id ORDER BY l.id`: "(1,one)(2,NULL)(3,NULL)",
		`SELECT l.id, r.label FROM oj_items r RIGHT JOIN oj_lookup l ON l.id = r.id ORDER BY l.id`: "(1,one)(2,NULL)(3,NULL)",
		// Protected relation on the preserved side: only visible rows are preserved.
		`SELECT r.id, l.id FROM oj_items r LEFT JOIN oj_lookup l ON l.id = r.id ORDER BY r.id`: "(1,1)(4,NULL)",
		// FULL JOIN preserves both sides, but never the hidden row.
		`SELECT l.id, r.id FROM oj_lookup l FULL JOIN oj_items r ON l.id = r.id ORDER BY l.id, r.id`: "(1,1)(2,NULL)(3,NULL)(NULL,4)",
	} {
		if got := render(mustExec(query).Rows); got != want {
			t.Fatalf("%s = %s, want %s", query, got, want)
		}
	}
}

func TestRLSTransactionalCatalogAndBackup(t *testing.T) {
	ctx := t.Context()
	dir := testDirectory(t)
	testClock := newTestClock()

	db, err := Open(ctx, OpenOptions{Directory: dir, Clock: testClock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()

	mustExec := func(sql string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sql, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sql, err)
		}
		return out
	}

	mustExec(`CREATE TABLE tx_docs (id bigint PRIMARY KEY, tenant_id text NOT NULL)`)
	mustExec(`INSERT INTO tx_docs VALUES (1, 't1'), (2, 't2')`)
	mustExec(`CREATE ROLE tx_user`)
	mustExec(`GRANT SELECT ON tx_docs TO tx_user`)
	mustExec(`ALTER TABLE tx_docs ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY committed_pol ON tx_docs FOR ALL TO tx_user USING (tenant_id = 't1')`)

	// Native MVCC catalog RLS stores roles and policies inside catalog
	// descriptors and WAL rather than a sidecar rls_meta.json file.
	if _, err := os.Stat(filepath.Join(dir, "rls_meta.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected legacy rls_meta.json sidecar in data directory: err=%v", err)
	}

	// 1. ROLLBACK discards uncommitted policy and role mutations.
	mustExec(`BEGIN`)
	mustExec(`DROP POLICY committed_pol ON tx_docs`)
	mustExec(`CREATE POLICY rolled_back_pol ON tx_docs FOR ALL TO tx_user USING (true)`)
	mustExec(`CREATE ROLE ephemeral_role`)
	mustExec(`ROLLBACK`)

	out := mustExec(`SELECT policyname FROM pg_policies WHERE tablename = 'tx_docs'`)
	if len(out.Rows) != 1 || out.Rows[0][0].StrVal != "committed_pol" {
		t.Fatalf("policies after ROLLBACK = %+v, want [committed_pol]", out.Rows)
	}
	out = mustExec(`SELECT rolname FROM pg_roles WHERE rolname = 'ephemeral_role'`)
	if len(out.Rows) != 0 {
		t.Fatalf("ephemeral_role survived ROLLBACK: %+v", out.Rows)
	}

	// 2. SAVEPOINT and ROLLBACK TO SAVEPOINT roll back partial RLS DDL within a transaction.
	mustExec(`BEGIN`)
	mustExec(`CREATE ROLE sp_kept_role`)
	mustExec(`SAVEPOINT sp1`)
	mustExec(`CREATE ROLE sp_discarded_role`)
	mustExec(`DROP POLICY committed_pol ON tx_docs`)
	mustExec(`ROLLBACK TO SAVEPOINT sp1`)
	mustExec(`COMMIT`)

	out = mustExec(`SELECT rolname FROM pg_roles WHERE rolname IN ('sp_kept_role', 'sp_discarded_role') ORDER BY rolname`)
	if len(out.Rows) != 1 || out.Rows[0][0].StrVal != "sp_kept_role" {
		t.Fatalf("roles after SAVEPOINT rollback = %+v, want [sp_kept_role]", out.Rows)
	}

	// 3. CreateHotBackup + RestoreCheckpoint preserves MVCC-backed RLS policies and roles.
	backupDir := filepath.Join(t.TempDir(), "rls_backup")
	if err := db.CreateHotBackup(ctx, backupDir); err != nil {
		t.Fatalf("CreateHotBackup: %v", err)
	}
	restoredDir := filepath.Join(t.TempDir(), "rls_restored")
	restored, err := RestoreCheckpoint(ctx, backupDir, OpenOptions{Directory: restoredDir, Clock: newTestClock()})
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	defer restored.Close()

	if _, err := restored.exec(ctx, `SET ROLE tx_user`, 0); err != nil {
		t.Fatalf("restored SET ROLE tx_user: %v", err)
	}
	out, err = restored.exec(ctx, `SELECT id FROM tx_docs ORDER BY id`, 0)
	if err != nil {
		t.Fatalf("restored SELECT tx_docs: %v", err)
	}
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 1 {
		t.Fatalf("restored RLS rows = %+v, want [1]", out.Rows)
	}
}

func TestReadWriteConcurrencyAndParallelism(t *testing.T) {
	ctx := t.Context()
	dir := testDirectory(t)
	testClock := newTestClock()

	// W2 & W5: Configure explicit Parallelism and memory budgets via OpenOptions.
	db, err := Open(ctx, OpenOptions{
		Directory:              dir,
		Clock:                  testClock,
		Parallelism:            4,
		MemoryBudgetBytes:      256 << 20,
		TransactionMemoryBytes: 16 << 20,
		TransactionSpillBytes:  32 << 20,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := db.exec(ctx, `CREATE TABLE kv_bench (id bigint PRIMARY KEY, bucket bigint NOT NULL, val bigint NOT NULL)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.exec(ctx, `INSERT INTO kv_bench SELECT g, g % 8, g * 10 FROM generate_series(1, 200) AS g`, 0); err != nil {
		t.Fatalf("INSERT generate_series: %v", err)
	}

	// W1: Autocommit SELECT runs in implicit ReadOnly mode while SHOW transaction_read_only remains "off".
	out, err := db.exec(ctx, `SHOW transaction_read_only`, 0)
	if err != nil || len(out.Rows) != 1 || out.Rows[0][0].StrVal != "off" {
		t.Fatalf("SHOW transaction_read_only = %+v, err=%v, want 'off'", out.Rows, err)
	}

	// W3 & W4: Disjoint point reads, updates, and range deletes across two concurrent ReadWrite transactions
	// do not block each other with full-table locks.
	s1, err := db.newSession(ctx)
	if err != nil {
		t.Fatalf("newSession s1: %v", err)
	}
	defer s1.close()
	s2, err := db.newSession(ctx)
	if err != nil {
		t.Fatalf("newSession s2: %v", err)
	}
	defer s2.close()

	for _, s := range []*sessionScope{s1, s2} {
		if _, err := db.execIn(ctx, s, `BEGIN`, 0); err != nil {
			t.Fatalf("BEGIN session: %v", err)
		}
	}
	// Session 1 reads id=1, updates id=2, and range-deletes id IN [10, 15).
	if _, err := db.execIn(ctx, s1, `SELECT val FROM kv_bench WHERE id = 1`, 0); err != nil {
		t.Fatalf("s1 SELECT id=1: %v", err)
	}
	if _, err := db.execIn(ctx, s1, `UPDATE kv_bench SET val = 999 WHERE id = 2`, 0); err != nil {
		t.Fatalf("s1 UPDATE id=2: %v", err)
	}
	if _, err := db.execIn(ctx, s1, `DELETE FROM kv_bench WHERE id >= 10 AND id < 15`, 0); err != nil {
		t.Fatalf("s1 DELETE range [10,15): %v", err)
	}

	// Session 2 concurrently reads id=100, updates id=101, and range-deletes id IN [110, 115) without blocking on Session 1!
	if _, err := db.execIn(ctx, s2, `SELECT val FROM kv_bench WHERE id = 100`, 0); err != nil {
		t.Fatalf("s2 SELECT id=100: %v", err)
	}
	if _, err := db.execIn(ctx, s2, `UPDATE kv_bench SET val = 888 WHERE id = 101`, 0); err != nil {
		t.Fatalf("s2 UPDATE id=101: %v", err)
	}
	if _, err := db.execIn(ctx, s2, `DELETE FROM kv_bench WHERE id >= 110 AND id < 115`, 0); err != nil {
		t.Fatalf("s2 DELETE range [110,115): %v", err)
	}

	for _, s := range []*sessionScope{s1, s2} {
		if _, err := db.execIn(ctx, s, `COMMIT`, 0); err != nil {
			t.Fatalf("COMMIT session: %v", err)
		}
	}

	// W1 & W2: Concurrent reader sessions and writer sessions run in parallel under Parallelism=4.
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for worker := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s, err := db.newSession(ctx)
			if err != nil {
				errCh <- err
				return
			}
			defer s.close()
			for iter := range 10 {
				if w == 0 {
					key := int64(1000 + iter)
					if _, err := db.execIn(ctx, s, "INSERT INTO kv_bench VALUES ("+itoa64(key)+", 1, 42)", 0); err != nil {
						errCh <- err
						return
					}
				} else {
					res, err := db.execIn(ctx, s, `SELECT bucket, sum(val) FROM kv_bench GROUP BY bucket ORDER BY bucket`, 0)
					if err != nil {
						errCh <- err
						return
					}
					if len(res.Rows) != 8 {
						errCh <- err
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent reader/writer worker error: %v", err)
		}
	}
}

func itoa64(v int64) string {
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// TestNativeRLSPenetrationMatrix verifies that every vulnerability class
// historically present in AST-rewriting RLS is structurally closed by native
// MVCC catalog roles/policies, single-point binder injection, per-branch
// Modify WITH CHECK enforcement, and ResetSession sanitization.
func TestNativeRLSPenetrationMatrix(t *testing.T) {
	ctx := t.Context()
	dir := testDirectory(t)
	db, err := Open(ctx, OpenOptions{Directory: dir, Clock: newTestClock()})
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
			t.Fatalf("%s: expected error %s, got nil", sqlText, wantCode)
		}
		var mErr *model.Error
		if !errors.As(err, &mErr) || mErr.Code != wantCode {
			t.Fatalf("%s: got error %v, want SQLSTATE %s", sqlText, err, wantCode)
		}
	}

	// Setup base schema, roles, tables, views, and statistics as superuser.
	mustExec(`CREATE ROLE pen_tenant`)
	mustExec(`CREATE ROLE pen_other`)
	mustExec(`CREATE TABLE secret (id bigint PRIMARY KEY, tenant_id text NOT NULL, payload text NOT NULL)`)
	mustExec(`INSERT INTO secret VALUES (1, 't1', 't1-secret-1'), (2, 't2', 't2-secret-2'), (3, 't1', 't1-secret-3')`)
	mustExec(`ALTER TABLE secret ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY secret_tenant_pol ON secret FOR ALL TO pen_tenant USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true))`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON secret TO pen_tenant`)
	mustExec(`CREATE STATISTICS secret_ext_stats (ndistinct, dependencies, mcv) ON tenant_id, payload FROM secret`)
	mustExec(`ANALYZE secret`)

	mustExec(`CREATE TABLE secret_alter (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, plain_col bigint NOT NULL, gen_col bigint GENERATED ALWAYS AS (plain_col * 2) STORED)`)
	mustExec(`CREATE TABLE leak (id bigint PRIMARY KEY, tenant_id text NOT NULL, payload text NOT NULL)`)
	mustExec(`CREATE TABLE scratch (id bigint PRIMARY KEY, val text)`)
	mustExec(`CREATE VIEW v_definer AS SELECT id, tenant_id, payload FROM secret`)
	mustExec(`CREATE VIEW v_invoker WITH (security_invoker = true) AS SELECT id, tenant_id, payload FROM secret`)
	mustExec(`CREATE SCHEMA s2`)
	mustExec(`CREATE FUNCTION trig_noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END; $$`)
	mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON leak, scratch TO pen_tenant`)
	mustExec(`GRANT SELECT ON v_definer, v_invoker TO pen_tenant`)
	mustExec(`GRANT USAGE ON SCHEMA s2 TO pen_tenant`)
	mustExec(`GRANT CREATE ON SCHEMA public TO pen_tenant`)

	// Verify superuser sees pg_stats and pg_statistic_ext_data rows for secret before switching role.
	out := mustExec(`SELECT count(*) FROM pg_stats WHERE tablename = 'secret'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal == 0 {
		t.Fatalf("superuser pg_stats count for secret = %+v, want > 0", out.Rows)
	}
	out = mustExec(`SELECT count(*) FROM pg_statistic_ext_data d JOIN pg_statistic_ext e ON e.oid = d.stxoid WHERE e.stxname = 'secret_ext_stats'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal == 0 {
		t.Fatalf("superuser pg_statistic_ext_data count for secret_ext_stats = %+v, want > 0", out.Rows)
	}

	// Switch to restricted tenant role.
	mustExec(`SET ROLE pen_tenant`)
	mustExec(`SET app.tenant_id = 't1'`)

	// 1. Role escalation attempts via set_config / SET / routine SET must fail with 42501.
	requireErrCode(`SELECT set_config('supersql.active_role', 'supersql', false)`, "42501")
	requireErrCode(`SELECT set_config('supersql.session_user', 'supersql', false)`, "42501")
	requireErrCode(`SELECT set_config('role', 'supersql', false)`, "42501")
	requireErrCode(`SELECT set_config('session_authorization', 'supersql', false)`, "42501")
	requireErrCode(`SET supersql.active_role = 'supersql'`, "42501")
	requireErrCode(`CREATE FUNCTION esc_fn1() RETURNS int LANGUAGE SQL SET supersql.active_role = 'supersql' AS 'SELECT 1'`, "42501")
	requireErrCode(`CREATE FUNCTION esc_fn2() RETURNS int LANGUAGE SQL SET role = 'supersql' AS 'SELECT 1'`, "42501")
	requireErrCode(`CREATE FUNCTION esc_fn3() RETURNS int LANGUAGE SQL SET supersql."session_user" = 'supersql' AS 'SELECT 1'`, "42501")
	mustExec(`CREATE FUNCTION benign_fn() RETURNS int LANGUAGE SQL AS 'SELECT 1'`)
	requireErrCode(`ALTER FUNCTION benign_fn() SET supersql.active_role = 'supersql'`, "42501")

	// 2. EXPLAIN ANALYZE SELECT/UPDATE/DELETE under restricted role enforces RLS.
	mustExec(`EXPLAIN ANALYZE SELECT * FROM secret`)
	mustExec(`EXPLAIN ANALYZE UPDATE secret SET payload = 'pwned' WHERE id = 2`)
	mustExec(`EXPLAIN ANALYZE DELETE FROM secret WHERE id = 2`)

	// 3. Writable CTE cannot delete hidden rows.
	out = mustExec(`WITH d AS (DELETE FROM secret WHERE id = 2 RETURNING id) SELECT count(*) FROM d`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 0 {
		t.Fatalf("writable CTE deleted hidden row: %+v", out.Rows)
	}

	// 4. DML WITH clause cannot leak hidden rows into another table.
	mustExec(`WITH s AS (SELECT id, tenant_id, payload FROM secret) INSERT INTO leak SELECT * FROM s`)
	out = mustExec(`SELECT id FROM leak ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 3 {
		t.Fatalf("DML WITH leaked rows: %+v, want [1, 3]", out.Rows)
	}

	// 5. RETURNING subquery cannot exfiltrate hidden rows.
	out = mustExec(`INSERT INTO scratch VALUES (1, 'x') RETURNING (SELECT count(*) FROM secret WHERE tenant_id = 't2')`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 0 {
		t.Fatalf("RETURNING subquery exfiltrated hidden rows: %+v", out.Rows)
	}

	// 6. Non-owner DDL bypass attempts must all fail with 42501.
	requireErrCode(`ALTER TABLE secret SET SCHEMA s2`, "42501")
	requireErrCode(`DROP SCHEMA s2 CASCADE`, "42501")
	requireErrCode(`CREATE TRIGGER hijack_trig AFTER INSERT ON secret FOR EACH ROW EXECUTE FUNCTION trig_noop()`, "42501")
	requireErrCode(`ALTER TABLE secret RENAME TO stolen`, "42501")
	requireErrCode(`DROP TABLE secret`, "42501")
	requireErrCode(`TRUNCATE secret`, "42501")
	requireErrCode(`ALTER TABLE secret ADD COLUMN extra text`, "42501")
	requireErrCode(`ALTER TABLE secret_alter ALTER COLUMN id SET INCREMENT BY 2`, "42501")
	requireErrCode(`ALTER TABLE secret_alter ALTER COLUMN plain_col ADD GENERATED ALWAYS AS IDENTITY`, "42501")
	requireErrCode(`ALTER TABLE secret_alter ALTER COLUMN gen_col SET EXPRESSION AS (plain_col * 3)`, "42501")
	requireErrCode(`ALTER TABLE secret_alter ALTER COLUMN gen_col DROP EXPRESSION`, "42501")

	// 7. security_invoker = true vs security_invoker = false views.
	out = mustExec(`SELECT id FROM v_invoker ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 3 {
		t.Fatalf("security_invoker=true view rows = %+v, want [1, 3]", out.Rows)
	}
	out = mustExec(`SELECT id FROM v_definer ORDER BY id`)
	if len(out.Rows) != 3 {
		t.Fatalf("security_invoker=false view rows = %+v, want 3 (owner bypass)", out.Rows)
	}

	// 8. Native catalog views pg_policies, pg_roles, and pg_stats RLS masking.
	out = mustExec(`SELECT policyname, cmd FROM pg_policies WHERE tablename = 'secret'`)
	if len(out.Rows) != 1 || out.Rows[0][0].StrVal != "secret_tenant_pol" || out.Rows[0][1].StrVal != "ALL" {
		t.Fatalf("pg_policies rows = %+v, want [(secret_tenant_pol, ALL)]", out.Rows)
	}
	out = mustExec(`SELECT rolname FROM pg_roles WHERE rolname IN ('pen_tenant', 'pen_other') ORDER BY rolname`)
	if len(out.Rows) != 2 || out.Rows[0][0].StrVal != "pen_other" || out.Rows[1][0].StrVal != "pen_tenant" {
		t.Fatalf("pg_roles rows = %+v, want [pen_other, pen_tenant]", out.Rows)
	}
	out = mustExec(`SELECT count(*) FROM pg_stats WHERE tablename = 'secret'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 0 {
		t.Fatalf("non-owner pg_stats count for RLS table = %+v, want 0 (masked)", out.Rows)
	}
	out = mustExec(`SELECT count(*) FROM pg_statistic_ext_data d JOIN pg_statistic_ext e ON e.oid = d.stxoid WHERE e.stxname = 'secret_ext_stats'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 0 {
		t.Fatalf("non-owner pg_statistic_ext_data count for RLS table = %+v, want 0 (masked)", out.Rows)
	}

	// 9. Owner ALTER TABLE ... SET SCHEMA preserves policies in destination schema,
	//    and non-owner schema CASCADE still cannot drop superuser-owned table.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE SCHEMA tenant_owned_schema AUTHORIZATION pen_tenant`)
	// Verify hidden row (id = 2) was untouched by EXPLAIN ANALYZE / writable CTE.
	out = mustExec(`SELECT payload FROM secret WHERE id = 2`)
	if len(out.Rows) != 1 || out.Rows[0][0].StrVal != "t2-secret-2" {
		t.Fatalf("hidden row id=2 was modified or deleted: %+v", out.Rows)
	}
	mustExec(`ALTER TABLE secret SET SCHEMA s2`)
	mustExec(`CREATE TABLE tenant_owned_schema.admin_owned (id bigint PRIMARY KEY)`)

	mustExec(`SET ROLE pen_tenant`)
	mustExec(`SET app.tenant_id = 't1'`)
	out = mustExec(`SELECT id FROM s2.secret ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 3 {
		t.Fatalf("s2.secret after SET SCHEMA rows = %+v, want [1, 3]", out.Rows)
	}
	requireErrCode(`DROP SCHEMA tenant_owned_schema CASCADE`, "42501")

	// 10. Per-branch INSERT ... ON CONFLICT DO UPDATE with distinct INSERT (col > 0)
	//     and UPDATE (col < 10) policies (must NOT be conjoined together!).
	mustExec(`RESET ROLE`)
	mustExec(`CREATE TABLE upsert_branch (id bigint PRIMARY KEY, col bigint NOT NULL)`)
	mustExec(`INSERT INTO upsert_branch VALUES (1, 5)`)
	mustExec(`ALTER TABLE upsert_branch ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY sel_all ON upsert_branch FOR SELECT TO pen_tenant USING (true)`)
	mustExec(`CREATE POLICY ins_pos ON upsert_branch FOR INSERT TO pen_tenant WITH CHECK (col > 0)`)
	mustExec(`CREATE POLICY upd_lt10 ON upsert_branch FOR UPDATE TO pen_tenant USING (true) WITH CHECK (col < 10)`)
	mustExec(`GRANT SELECT, INSERT, UPDATE ON upsert_branch TO pen_tenant`)

	mustExec(`SET ROLE pen_tenant`)
	// New row with col=20: satisfies INSERT (20 > 0) even though it violates UPDATE (20 < 10).
	mustExec(`INSERT INTO upsert_branch VALUES (2, 20) ON CONFLICT (id) DO UPDATE SET col = EXCLUDED.col`)
	// New row with col=-1: violates INSERT (-1 > 0) -> 42501.
	requireErrCode(`INSERT INTO upsert_branch VALUES (3, -1) ON CONFLICT (id) DO UPDATE SET col = EXCLUDED.col`, "42501")
	// Conflict on id=1 updating col=-5: satisfies UPDATE (-5 < 10) even though -5 > 0 is false.
	mustExec(`INSERT INTO upsert_branch VALUES (1, 99) ON CONFLICT (id) DO UPDATE SET col = -5`)
	// Conflict on id=1 updating col=15: violates UPDATE (15 < 10) -> 42501.
	requireErrCode(`INSERT INTO upsert_branch VALUES (1, 99) ON CONFLICT (id) DO UPDATE SET col = 15`, "42501")
	out = mustExec(`SELECT id, col FROM upsert_branch ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][1].IntVal != -5 || out.Rows[1][1].IntVal != 20 {
		t.Fatalf("upsert_branch rows = %+v, want [(1,-5), (2,20)]", out.Rows)
	}

	// 11. Concurrent RLS DDL (CREATE POLICY / CREATE ROLE via sql.DB) -> zero lost updates.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE TABLE conc_t (id bigint PRIMARY KEY, tenant_id text NOT NULL)`)
	mustExec(`ALTER TABLE conc_t ENABLE ROW LEVEL SECURITY`)
	connector := NewConnectorWithCatalog(db)
	sqlDB := sql.OpenDB(connector)
	defer sqlDB.Close()

	execWithRetry := func(query string) error {
		var lastErr error
		for attempt := range 64 {
			_, err := sqlDB.ExecContext(ctx, query)
			if err == nil {
				return nil
			}
			var mErr *model.Error
			if errors.As(err, &mErr) && (mErr.Code == "40001" || mErr.Code == "55P03") {
				lastErr = err
				time.Sleep(time.Duration(1+attempt%5) * 5 * time.Millisecond)
				continue
			}
			return err
		}
		return fmt.Errorf("exceeded retries for %s: last error: %w", query, lastErr)
	}

	const workers = 6
	var wg sync.WaitGroup
	errCh := make(chan error, workers*2)
	for i := range workers {
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			errCh <- execWithRetry(fmt.Sprintf(`CREATE ROLE conc_role_%d`, idx))
		}(i)
		go func(idx int) {
			defer wg.Done()
			errCh <- execWithRetry(fmt.Sprintf(`CREATE POLICY conc_pol_%d ON conc_t FOR SELECT TO PUBLIC USING (tenant_id = 't%d')`, idx, idx))
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent RLS DDL error: %v", err)
		}
	}
	out = mustExec(`SELECT count(*) FROM pg_policies WHERE tablename = 'conc_t'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != workers {
		t.Fatalf("concurrent CREATE POLICY count = %+v, want %d (lost update!)", out.Rows, workers)
	}
	out = mustExec(`SELECT count(*) FROM pg_roles WHERE rolname LIKE 'conc_role_%'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != workers {
		t.Fatalf("concurrent CREATE ROLE count = %+v, want %d (lost update!)", out.Rows, workers)
	}

	// 12. Pooled sql.DB ResetSession sanitizes active role, custom GUCs, temp
	//     tables, SQL prepared statements, held cursors, and advisory locks.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	conn1, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("sqlDB.Conn 1: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, `CREATE TEMP TABLE pool_leak_tmp (id bigint PRIMARY KEY)`); err != nil {
		t.Fatalf("conn1 CREATE TEMP TABLE: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, `INSERT INTO pool_leak_tmp VALUES (1)`); err != nil {
		t.Fatalf("conn1 INSERT temp: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, `PREPARE pool_leak_prep AS SELECT 99`); err != nil {
		t.Fatalf("conn1 PREPARE: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("conn1 BEGIN for WITH HOLD cursor: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, `DECLARE pool_leak_cur CURSOR WITH HOLD FOR SELECT 1`); err != nil {
		t.Fatalf("conn1 DECLARE CURSOR WITH HOLD: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("conn1 COMMIT for WITH HOLD cursor: %v", err)
	}
	var acquired bool
	if err := conn1.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(77777)`).Scan(&acquired); err != nil || !acquired {
		t.Fatalf("conn1 pg_try_advisory_lock: acquired=%v err=%v", acquired, err)
	}
	if _, err := conn1.ExecContext(ctx, `SET ROLE pen_tenant`); err != nil {
		t.Fatalf("conn1 SET ROLE: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, `SET app.tenant_id = 't1'`); err != nil {
		t.Fatalf("conn1 SET app.tenant_id: %v", err)
	}
	if err := conn1.Close(); err != nil {
		t.Fatalf("conn1 Close: %v", err)
	}

	// database/sql invokes driver.SessionResetter.ResetSession on the next
	// checkout from the pool, sanitizing all session-scoped state before reuse.
	conn2, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("sqlDB.Conn 2: %v", err)
	}
	defer conn2.Close()

	// Verify session-level advisory lock was released on ResetSession by
	// acquiring it from a distinct session (the embedded catalog session).
	out = mustExec(`SELECT pg_try_advisory_lock(77777)`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal == 0 {
		t.Fatalf("pooled connection leaked session advisory lock 77777: pg_try_advisory_lock = %+v", out.Rows)
	}
	mustExec(`SELECT pg_advisory_unlock(77777)`)
	var gotRole string
	if err := conn2.QueryRowContext(ctx, `SHOW role`).Scan(&gotRole); err != nil {
		t.Fatalf("conn2 SHOW role: %v", err)
	}
	if gotRole != "none" {
		t.Fatalf("pooled connection leaked role = %q, want %q", gotRole, "none")
	}
	var gotTenant sql.NullString
	if err := conn2.QueryRowContext(ctx, `SELECT current_setting('app.tenant_id', true)`).Scan(&gotTenant); err != nil {
		t.Fatalf("conn2 current_setting: %v", err)
	}
	if gotTenant.Valid && gotTenant.String != "" {
		t.Fatalf("pooled connection leaked app.tenant_id = %q, want NULL/empty", gotTenant.String)
	}
	var unlocked bool
	if err := conn2.QueryRowContext(ctx, `SELECT pg_advisory_unlock(77777)`).Scan(&unlocked); err != nil {
		t.Fatalf("conn2 pg_advisory_unlock: %v", err)
	}
	if unlocked {
		t.Fatalf("pooled connection still held session advisory lock 77777")
	}
	if _, err := conn2.ExecContext(ctx, `SELECT * FROM pool_leak_tmp`); err == nil || SQLState(err) != "42P01" {
		t.Fatalf("pooled connection leaked temp table: err=%v, want SQLSTATE 42P01", err)
	}
	if _, err := conn2.ExecContext(ctx, `EXECUTE pool_leak_prep`); err == nil || SQLState(err) != "26000" {
		t.Fatalf("pooled connection leaked SQL PREPARE statement: err=%v, want SQLSTATE 26000", err)
	}
	if _, err := conn2.ExecContext(ctx, `FETCH 1 FROM pool_leak_cur`); err == nil || SQLState(err) != "34000" {
		t.Fatalf("pooled connection leaked held cursor: err=%v, want SQLSTATE 34000", err)
	}
}

func TestRLSOnConflictExistenceOracleAndUsingCheck(t *testing.T) {
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

	mustExec(`CREATE ROLE conflict_user`)

	// Table 1: standard tenant isolation (FOR ALL USING + WITH CHECK).
	mustExec(`CREATE TABLE conflict_docs (
		id bigint PRIMARY KEY,
		tenant_id text NOT NULL,
		val text NOT NULL
	)`)
	mustExec(`INSERT INTO conflict_docs VALUES (1, 't1', 'own-1'), (2, 't2', 'other-2')`)
	mustExec(`ALTER TABLE conflict_docs ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY docs_pol ON conflict_docs
		FOR ALL TO conflict_user
		USING (tenant_id = current_setting('app.tenant_id', true))
		WITH CHECK (tenant_id = current_setting('app.tenant_id', true))`)
	mustExec(`GRANT SELECT, INSERT, UPDATE ON conflict_docs TO conflict_user`)

	// Table 2: separate SELECT USING (val <> 'hidden_sel') and UPDATE USING (true)
	// to verify DO UPDATE enforces SELECT USING as well as UPDATE USING.
	mustExec(`CREATE TABLE conflict_sel_using (
		id bigint PRIMARY KEY,
		val text NOT NULL
	)`)
	mustExec(`INSERT INTO conflict_sel_using VALUES (1, 'visible'), (2, 'hidden_sel')`)
	mustExec(`ALTER TABLE conflict_sel_using ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY sel_hide ON conflict_sel_using FOR SELECT TO conflict_user USING (val <> 'hidden_sel')`)
	mustExec(`CREATE POLICY ins_allow ON conflict_sel_using FOR INSERT TO conflict_user WITH CHECK (true)`)
	mustExec(`CREATE POLICY upd_allow ON conflict_sel_using FOR UPDATE TO conflict_user USING (true) WITH CHECK (true)`)
	mustExec(`GRANT SELECT, INSERT, UPDATE ON conflict_sel_using TO conflict_user`)

	mustExec(`SET ROLE conflict_user`)
	mustExec(`SET app.tenant_id = 't1'`)

	// 1. Existence oracle fix: INSERT ... ON CONFLICT DO NOTHING with a proposed
	// row violating INSERT WITH CHECK MUST fail with 42501 BOTH when the key does
	// NOT exist and when the key ALREADY exists.
	requireErrCode(`INSERT INTO conflict_docs VALUES (999, 't2', 'probe-missing') ON CONFLICT (id) DO NOTHING`, "42501")
	requireErrCode(`INSERT INTO conflict_docs VALUES (2, 't2', 'probe-existing-other') ON CONFLICT (id) DO NOTHING`, "42501")
	requireErrCode(`INSERT INTO conflict_docs VALUES (1, 't2', 'probe-existing-own') ON CONFLICT (id) DO NOTHING`, "42501")

	// Same for ON CONFLICT DO UPDATE (even with WHERE false): INSERT WITH CHECK
	// runs before conflict lookup, so violating proposed rows always fail with 42501.
	requireErrCode(`INSERT INTO conflict_docs VALUES (999, 't2', 'probe-missing') ON CONFLICT (id) DO UPDATE SET val = 'x' WHERE false`, "42501")
	requireErrCode(`INSERT INTO conflict_docs VALUES (2, 't2', 'probe-existing') ON CONFLICT (id) DO UPDATE SET val = 'x' WHERE false`, "42501")

	// A proposed row satisfying INSERT WITH CHECK on an existing key with DO NOTHING
	// still succeeds cleanly with 0 rows affected.
	out := mustExec(`INSERT INTO conflict_docs VALUES (2, 't1', 'noop') ON CONFLICT (id) DO NOTHING`)
	if out.RowsAffected != 0 {
		t.Fatalf("ON CONFLICT DO NOTHING allowed proposed row affected %d rows, want 0", out.RowsAffected)
	}

	// 2. DO UPDATE USING enforcement: targeting an existing row hidden by UPDATE/SELECT
	// USING policy MUST raise 42501 even when user supplies DO UPDATE ... WHERE false!
	err = requireErrCode(`INSERT INTO conflict_docs VALUES (2, 't1', 'hijack')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val WHERE false`, "42501")
	if !strings.Contains(err.Error(), "USING expression") {
		t.Fatalf("expected USING expression error message, got: %v", err)
	}

	// Also when UPDATE USING is true but SELECT USING hides the conflicting row:
	err = requireErrCode(`INSERT INTO conflict_sel_using VALUES (2, 'hijack')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val WHERE false`, "42501")
	if !strings.Contains(err.Error(), "USING expression") {
		t.Fatalf("expected USING expression error message for SELECT USING, got: %v", err)
	}

	// 3. When both UPDATE USING and SELECT USING allow the conflicting row, user's
	// DO UPDATE ... WHERE false skips the update cleanly (0 rows affected), and a
	// matching DO UPDATE updates the row normally.
	out = mustExec(`INSERT INTO conflict_docs VALUES (1, 't1', 'skipped')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val WHERE false`)
	if out.RowsAffected != 0 {
		t.Fatalf("ON CONFLICT DO UPDATE WHERE false on visible row affected %d rows, want 0", out.RowsAffected)
	}
	out = mustExec(`INSERT INTO conflict_docs VALUES (1, 't1', 'updated-1')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val WHERE conflict_docs.val = 'own-1'`)
	if out.RowsAffected != 1 {
		t.Fatalf("ON CONFLICT DO UPDATE on visible row affected %d rows, want 1", out.RowsAffected)
	}
	out = mustExec(`INSERT INTO conflict_sel_using VALUES (1, 'still-visible')
		ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val WHERE false`)
	if out.RowsAffected != 0 {
		t.Fatalf("conflict_sel_using WHERE false on visible row affected %d rows, want 0", out.RowsAffected)
	}

	// Verify hidden rows remain completely untouched as superuser.
	mustExec(`RESET ROLE`)
	out = mustExec(`SELECT id, tenant_id, val FROM conflict_docs ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][2].StrVal != "updated-1" || out.Rows[1][1].StrVal != "t2" || out.Rows[1][2].StrVal != "other-2" {
		t.Fatalf("conflict_docs final rows = %+v, want [(1,t1,updated-1), (2,t2,other-2)]", out.Rows)
	}
	out = mustExec(`SELECT id, val FROM conflict_sel_using ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][1].StrVal != "visible" || out.Rows[1][1].StrVal != "hidden_sel" {
		t.Fatalf("conflict_sel_using final rows = %+v, want [(1,visible), (2,hidden_sel)]", out.Rows)
	}
}



