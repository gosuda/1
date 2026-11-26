package supersql

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestGrantRevokeTableViewSequenceSchemaFunction(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
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
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sql, code, got, err)
		}
	}

	// Setup roles and hierarchy: member_role -> group_role -> base_role.
	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE ROLE base_role`)
	mustExec(`CREATE ROLE group_role`)
	mustExec(`CREATE ROLE member_role`)
	mustExec(`GRANT base_role TO group_role`)
	mustExec(`GRANT group_role TO member_role`)

	// Setup table, view, materialized view, sequence, schema, function, procedure.
	mustExec(`CREATE TABLE items (id int PRIMARY KEY, val int NOT NULL)`)
	mustExec(`INSERT INTO items VALUES (1, 10), (2, 20)`)
	mustExec(`CREATE VIEW items_view AS SELECT id, val FROM items`)
	mustExec(`CREATE VIEW items_invoker WITH (security_invoker = true) AS SELECT id, val FROM items`)
	mustExec(`CREATE MATERIALIZED VIEW items_mv AS SELECT id, val FROM items`)
	mustExec(`CREATE SEQUENCE items_seq START 100`)
	mustExec(`CREATE SCHEMA app`)
	mustExec(`CREATE TABLE app.t1 (id int PRIMARY KEY, name text)`)
	mustExec(`CREATE TABLE app.t2 (id int PRIMARY KEY, name text)`)
	mustExec(`INSERT INTO app.t1 VALUES (1, 'a')`)
	mustExec(`INSERT INTO app.t2 VALUES (2, 'b')`)
	mustExec(`CREATE SEQUENCE app.s1 START 1`)
	mustExec(`CREATE FUNCTION app.double_val(x int) RETURNS int LANGUAGE sql AS 'SELECT x * 2'`)
	mustExec(`CREATE PROCEDURE app.noop_proc() LANGUAGE plpgsql AS $$ BEGIN END; $$`)

	// Missing grantee role -> 42704.
	requireErrCode(`GRANT SELECT ON items TO nonexistent_role`, "42704")

	// 1. Default denial on tables, views, matviews, sequences, and non-public schemas.
	mustExec(`SET ROLE alice`)
	requireErrCode(`SELECT * FROM items`, "42501")
	requireErrCode(`INSERT INTO items VALUES (3, 30)`, "42501")
	requireErrCode(`UPDATE items SET val = 99`, "42501")
	requireErrCode(`DELETE FROM items`, "42501")
	requireErrCode(`TRUNCATE items`, "42501")
	requireErrCode(`SELECT * FROM items_view`, "42501")
	requireErrCode(`SELECT * FROM items_mv`, "42501")
	requireErrCode(`SELECT nextval('items_seq')`, "42501")
	requireErrCode(`SELECT setval('items_seq', 200)`, "42501")
	requireErrCode(`SELECT * FROM app.t1`, "42501")
	requireErrCode(`CREATE TABLE app.alice_tbl (id int PRIMARY KEY)`, "42501")
	requireErrCode(`CREATE TABLE public.alice_tbl (id int PRIMARY KEY)`, "42501")
	requireErrCode(`GRANT SELECT ON items TO bob`, "42501")

	// COPY TO / COPY FROM denied without SELECT / INSERT.
	var copyBuf bytes.Buffer
	if _, err := db.CopyTo(ctx, `COPY items TO STDOUT`, &copyBuf, CopyOptions{MaxRecordBytes: 1 << 20}); SQLState(err) != "42501" {
		t.Fatalf("COPY TO without SELECT = %v, want 42501", err)
	}
	if _, err := db.CopyFrom(ctx, `COPY items FROM STDIN`, strings.NewReader("3\t30\n"), CopyOptions{MaxRecordBytes: 1 << 20}); SQLState(err) != "42501" {
		t.Fatalf("COPY FROM without INSERT = %v, want 42501", err)
	}

	// 2. Granular table privileges + WITH GRANT OPTION + UPDATE without SELECT.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT UPDATE ON items TO alice`)
	mustExec(`SET ROLE alice`)
	// UPDATE without WHERE / RETURNING / column read succeeds with UPDATE alone.
	mustExec(`UPDATE items SET val = 55`)
	// UPDATE with WHERE or RHS column read requires SELECT as well.
	requireErrCode(`UPDATE items SET val = 66 WHERE id = 1`, "42501")
	requireErrCode(`UPDATE items SET val = val + 1`, "42501")

	mustExec(`RESET ROLE`)
	mustExec(`GRANT SELECT, INSERT ON items TO alice WITH GRANT OPTION`)
	mustExec(`SET ROLE alice`)
	out := mustExec(`SELECT count(*) FROM items`)
	if out.Rows[0][0].IntVal != 2 {
		t.Fatalf("alice SELECT count = %d, want 2", out.Rows[0][0].IntVal)
	}
	mustExec(`UPDATE items SET val = val + 1 WHERE id = 1`)
	mustExec(`INSERT INTO items VALUES (3, 30)`)
	// COPY TO and COPY FROM now succeed for alice.
	copyBuf.Reset()
	if res, err := db.CopyTo(ctx, `COPY items TO STDOUT`, &copyBuf, CopyOptions{MaxRecordBytes: 1 << 20}); err != nil || res.RowsAffected != 3 {
		t.Fatalf("COPY TO with SELECT: res=%+v err=%v", res, err)
	}
	if res, err := db.CopyFrom(ctx, `COPY items FROM STDIN`, strings.NewReader("4\t40\n"), CopyOptions{MaxRecordBytes: 1 << 20}); err != nil || res.RowsAffected != 1 {
		t.Fatalf("COPY FROM with INSERT: res=%+v err=%v", res, err)
	}
	// alice has GRANT OPTION on SELECT so she can grant SELECT to bob, but NOT DELETE.
	mustExec(`GRANT SELECT ON items TO bob`)
	requireErrCode(`GRANT DELETE ON items TO bob`, "42501")

	// bob can now SELECT items.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	out = mustExec(`SELECT count(*) FROM items`)
	if out.Rows[0][0].IntVal != 4 {
		t.Fatalf("bob SELECT count = %d, want 4", out.Rows[0][0].IntVal)
	}

	// REVOKE GRANT OPTION FOR SELECT prevents alice from granting further while keeping her SELECT.
	mustExec(`RESET ROLE`)
	mustExec(`REVOKE GRANT OPTION FOR SELECT ON items FROM alice`)
	mustExec(`SET ROLE alice`)
	mustExec(`SELECT count(*) FROM items`)
	requireErrCode(`GRANT SELECT ON items TO member_role`, "42501")

	// REVOKE SELECT immediately denies access.
	mustExec(`RESET ROLE`)
	mustExec(`REVOKE SELECT ON items FROM bob`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`SELECT * FROM items`, "42501")

	// 3. View (owner semantics vs security_invoker) & Materialized View privileges.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT SELECT ON items_view, items_invoker, items_mv TO bob`)
	mustExec(`SET ROLE bob`)
	// bob has NO SELECT on items, yet items_view runs under owner (supersql) -> succeeds!
	out = mustExec(`SELECT count(*) FROM items_view`)
	if out.Rows[0][0].IntVal != 4 {
		t.Fatalf("bob items_view count = %d, want 4", out.Rows[0][0].IntVal)
	}
	// items_invoker runs under invoker (bob) -> denied on underlying items!
	requireErrCode(`SELECT * FROM items_invoker`, "42501")
	out = mustExec(`SELECT count(*) FROM items_mv`)
	if out.Rows[0][0].IntVal != 2 {
		t.Fatalf("bob items_mv count = %d, want 2", out.Rows[0][0].IntVal)
	}

	// 4. TRUNCATE privilege.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE TABLE trunc_tbl (id int PRIMARY KEY)`)
	mustExec(`INSERT INTO trunc_tbl VALUES (1)`)
	mustExec(`GRANT TRUNCATE ON trunc_tbl TO bob`)
	mustExec(`SET ROLE bob`)
	mustExec(`TRUNCATE trunc_tbl`)

	// 5. Sequence privileges (USAGE, SELECT, UPDATE).
	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SEQUENCE items_seq TO bob`)
	mustExec(`SET ROLE bob`)
	out = mustExec(`SELECT nextval('items_seq')`)
	if out.Rows[0][0].IntVal != 100 {
		t.Fatalf("nextval = %d, want 100", out.Rows[0][0].IntVal)
	}
	out = mustExec(`SELECT currval('items_seq')`)
	if out.Rows[0][0].IntVal != 100 {
		t.Fatalf("currval = %d, want 100", out.Rows[0][0].IntVal)
	}
	// setval requires UPDATE, not USAGE alone.
	requireErrCode(`SELECT setval('items_seq', 500)`, "42501")
	mustExec(`RESET ROLE`)
	mustExec(`GRANT UPDATE ON SEQUENCE items_seq TO bob`)
	mustExec(`SET ROLE bob`)
	mustExec(`SELECT setval('items_seq', 500)`)
	mustExec(`RESET ROLE`)
	mustExec(`REVOKE ALL PRIVILEGES ON SEQUENCE items_seq FROM bob`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`SELECT nextval('items_seq')`, "42501")

	// 6. Schema privileges + ALL TABLES/SEQUENCES/FUNCTIONS IN SCHEMA + transitive role inheritance.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SCHEMA app TO base_role`)
	mustExec(`GRANT SELECT ON ALL TABLES IN SCHEMA app TO base_role`)
	mustExec(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA app TO base_role`)
	// member_role inherits base_role transitively via group_role.
	mustExec(`SET ROLE member_role`)
	out = mustExec(`SELECT name FROM app.t1`)
	if out.Rows[0][0].StrVal != "a" {
		t.Fatalf("member_role app.t1 = %+v", out.Rows)
	}
	out = mustExec(`SELECT name FROM app.t2`)
	if out.Rows[0][0].StrVal != "b" {
		t.Fatalf("member_role app.t2 = %+v", out.Rows)
	}
	out = mustExec(`SELECT nextval('app.s1')`)
	if out.Rows[0][0].IntVal != 1 {
		t.Fatalf("member_role app.s1 = %d, want 1", out.Rows[0][0].IntVal)
	}
	// Functions have EXECUTE granted to PUBLIC by default (requires schema USAGE).
	out = mustExec(`SELECT app.double_val(21)`)
	if out.Rows[0][0].IntVal != 42 {
		t.Fatalf("member_role app.double_val(21) = %d, want 42", out.Rows[0][0].IntVal)
	}
	mustExec(`CALL app.noop_proc()`)

	// REVOKE EXECUTE FROM PUBLIC denies execution until explicitly granted.
	mustExec(`RESET ROLE`)
	mustExec(`REVOKE EXECUTE ON FUNCTION app.double_val(int) FROM PUBLIC`)
	mustExec(`REVOKE EXECUTE ON PROCEDURE app.noop_proc() FROM PUBLIC`)
	mustExec(`SET ROLE member_role`)
	requireErrCode(`SELECT app.double_val(5)`, "42501")
	requireErrCode(`CALL app.noop_proc()`, "42501")
	mustExec(`RESET ROLE`)
	mustExec(`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO base_role`)
	mustExec(`GRANT EXECUTE ON PROCEDURE app.noop_proc() TO base_role`)
	mustExec(`SET ROLE member_role`)
	out = mustExec(`SELECT app.double_val(5)`)
	if out.Rows[0][0].IntVal != 10 {
		t.Fatalf("app.double_val(5) after grant = %d, want 10", out.Rows[0][0].IntVal)
	}
	mustExec(`CALL app.noop_proc()`)

	// CREATE on schema allows creating objects in that schema.
	requireErrCode(`CREATE TABLE app.member_tbl (id int PRIMARY KEY)`, "42501")
	mustExec(`RESET ROLE`)
	mustExec(`GRANT CREATE ON SCHEMA app TO member_role`)
	mustExec(`SET ROLE member_role`)
	mustExec(`CREATE TABLE app.member_tbl (id int PRIMARY KEY)`)
	mustExec(`INSERT INTO app.member_tbl VALUES (1)`)

	// GRANT ... TO PUBLIC allows any role.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT SELECT ON items TO PUBLIC`)
	mustExec(`SET ROLE bob`)
	mustExec(`SELECT count(*) FROM items`)
	mustExec(`RESET ROLE`)
	mustExec(`REVOKE SELECT ON items FROM PUBLIC`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`SELECT count(*) FROM items`, "42501")

	// 7. Transactional ROLLBACK reverts GRANT and REVOKE.
	mustExec(`RESET ROLE`)
	mustExec(`BEGIN`)
	mustExec(`GRANT SELECT ON items TO bob`)
	mustExec(`ROLLBACK`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`SELECT count(*) FROM items`, "42501")

	mustExec(`RESET ROLE`)
	mustExec(`GRANT SELECT ON items TO bob`)
	mustExec(`BEGIN`)
	mustExec(`REVOKE SELECT ON items FROM bob`)
	mustExec(`ROLLBACK`)
	mustExec(`SET ROLE bob`)
	mustExec(`SELECT count(*) FROM items`)
}

func TestOwnerTransferAllObjects(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
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
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sql, code, got, err)
		}
	}

	mustExec(`CREATE ROLE owner_a`)
	mustExec(`CREATE ROLE owner_b`)
	mustExec(`CREATE ROLE bystander`)

	// CREATE SCHEMA ... AUTHORIZATION sets owner_a as owner.
	mustExec(`CREATE SCHEMA owned_sch AUTHORIZATION owner_a`)
	mustExec(`SET ROLE owner_a`)
	mustExec(`CREATE TABLE owned_sch.t (id int PRIMARY KEY, v int)`)
	mustExec(`INSERT INTO owned_sch.t VALUES (1, 100)`)
	mustExec(`CREATE VIEW owned_sch.v AS SELECT id, v FROM owned_sch.t`)
	mustExec(`CREATE MATERIALIZED VIEW owned_sch.mv AS SELECT id, v FROM owned_sch.t`)
	mustExec(`CREATE SEQUENCE owned_sch.seq START 10`)
	mustExec(`CREATE FUNCTION owned_sch.fn(x int) RETURNS int LANGUAGE sql AS 'SELECT x + 1'`)
	mustExec(`CREATE PROCEDURE owned_sch.pr() LANGUAGE plpgsql AS $$ BEGIN END; $$`)

	// Nonexistent target role -> 42704.
	requireErrCode(`ALTER TABLE owned_sch.t OWNER TO ghost_role`, "42704")
	// Non-superuser owner_a cannot transfer to owner_b without membership in owner_b -> 42501.
	requireErrCode(`ALTER TABLE owned_sch.t OWNER TO owner_b`, "42501")

	// Bystander cannot transfer ownership -> 42501.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bystander`)
	requireErrCode(`ALTER SCHEMA owned_sch OWNER TO bystander`, "42501")
	requireErrCode(`ALTER FUNCTION owned_sch.fn(int) OWNER TO bystander`, "42501")

	// Superuser transfers ownership of schema, table, view, matview, sequence, function, procedure to owner_b.
	mustExec(`RESET ROLE`)
	mustExec(`ALTER SCHEMA owned_sch OWNER TO owner_b`)
	mustExec(`ALTER TABLE owned_sch.t OWNER TO owner_b`)
	mustExec(`ALTER VIEW owned_sch.v OWNER TO owner_b`)
	mustExec(`ALTER MATERIALIZED VIEW owned_sch.mv OWNER TO owner_b`)
	mustExec(`ALTER SEQUENCE owned_sch.seq OWNER TO owner_b`)
	mustExec(`ALTER FUNCTION owned_sch.fn(int) OWNER TO owner_b`)
	mustExec(`ALTER PROCEDURE owned_sch.pr() OWNER TO owner_b`)

	// owner_a lost ownership -> denied!
	mustExec(`SET ROLE owner_a`)
	requireErrCode(`SELECT * FROM owned_sch.t`, "42501")
	requireErrCode(`SELECT * FROM owned_sch.v`, "42501")
	requireErrCode(`SELECT * FROM owned_sch.mv`, "42501")
	requireErrCode(`SELECT nextval('owned_sch.seq')`, "42501")

	// owner_b now has full owner privileges on every transferred object!
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE owner_b`)
	out := mustExec(`SELECT v FROM owned_sch.t`)
	if out.Rows[0][0].IntVal != 100 {
		t.Fatalf("owner_b table v = %d, want 100", out.Rows[0][0].IntVal)
	}
	mustExec(`SELECT * FROM owned_sch.v`)
	mustExec(`SELECT * FROM owned_sch.mv`)
	out = mustExec(`SELECT nextval('owned_sch.seq')`)
	if out.Rows[0][0].IntVal != 10 {
		t.Fatalf("owner_b seq = %d, want 10", out.Rows[0][0].IntVal)
	}
	out = mustExec(`SELECT owned_sch.fn(9)`)
	if out.Rows[0][0].IntVal != 10 {
		t.Fatalf("owner_b fn = %d, want 10", out.Rows[0][0].IntVal)
	}
	mustExec(`CALL owned_sch.pr()`)
	mustExec(`CREATE TABLE owned_sch.b_table (id int PRIMARY KEY)`)

	// When owner_b is a member of owner_a, owner_b can self-transfer back to owner_a.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT owner_a TO owner_b`)
	mustExec(`SET ROLE owner_b`)
	mustExec(`ALTER TABLE owned_sch.b_table OWNER TO owner_a`)
}

func TestSecurityDefinerAndRLSInteraction(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
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
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sql, code, got, err)
		}
	}

	mustExec(`CREATE ROLE low_priv`)
	mustExec(`CREATE ROLE data_owner`)
	mustExec(`CREATE SCHEMA vault AUTHORIZATION data_owner`)
	mustExec(`GRANT USAGE ON SCHEMA vault TO low_priv`)

	// data_owner creates a private table with RLS enabled and no direct GRANT to low_priv.
	mustExec(`SET ROLE data_owner`)
	mustExec(`CREATE TABLE vault.secrets (id int PRIMARY KEY, tenant text NOT NULL, body text NOT NULL)`)
	mustExec(`INSERT INTO vault.secrets VALUES (1, 't1', 'secret-t1'), (2, 't2', 'secret-t2')`)
	mustExec(`ALTER TABLE vault.secrets ENABLE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY secrets_t1 ON vault.secrets FOR ALL TO low_priv USING (tenant = 't1') WITH CHECK (tenant = 't1')`)

	// SECURITY INVOKER function fails for low_priv (no direct privilege on vault.secrets).
	mustExec(`CREATE FUNCTION vault.invoker_count() RETURNS bigint LANGUAGE sql SECURITY INVOKER AS 'SELECT count(*) FROM vault.secrets'`)

	// SECURITY DEFINER SQL function runs as data_owner (table owner -> full privileges + bypasses RLS).
	mustExec(`CREATE FUNCTION vault.definer_count() RETURNS bigint LANGUAGE sql SECURITY DEFINER AS 'SELECT count(*) FROM vault.secrets'`)

	// SECURITY DEFINER PL/pgSQL function reads & inserts rows and reports current_user inside body.
	mustExec(`CREATE FUNCTION vault.definer_insert_and_who(p_id int, p_tenant text, p_body text) RETURNS text LANGUAGE plpgsql SECURITY DEFINER AS $$
	DECLARE
		who text;
	BEGIN
		INSERT INTO vault.secrets VALUES (p_id, p_tenant, p_body);
		SELECT current_user INTO who;
		RETURN who;
	END;
	$$`)

	// SECURITY DEFINER function that raises an error must still restore caller's role!
	mustExec(`CREATE FUNCTION vault.definer_boom() RETURNS int LANGUAGE plpgsql SECURITY DEFINER AS $$
	BEGIN
		RAISE EXCEPTION 'boom inside definer';
	END;
	$$`)

	// Nested SECURITY DEFINER -> SECURITY INVOKER inherits definer's role cleanly.
	mustExec(`CREATE FUNCTION vault.nested_definer() RETURNS text LANGUAGE plpgsql SECURITY DEFINER AS $$
	DECLARE
		inner_count bigint;
		who text;
	BEGIN
		SELECT vault.invoker_count() INTO inner_count;
		SELECT current_user INTO who;
		RETURN who || ':' || inner_count::text;
	END;
	$$`)

	// Verify pg_proc.prosecdef reflects SECURITY DEFINER vs SECURITY INVOKER.
	mustExec(`RESET ROLE`)
	out := mustExec(`SELECT proname, prosecdef FROM pg_proc WHERE proname IN ('invoker_count', 'definer_count') ORDER BY proname`)
	if len(out.Rows) != 2 || out.Rows[0][0].StrVal != "definer_count" || out.Rows[0][1].IntVal == 0 || out.Rows[1][0].StrVal != "invoker_count" || out.Rows[1][1].IntVal != 0 {
		t.Fatalf("pg_proc prosecdef = %+v, want definer_count=true, invoker_count=false", out.Rows)
	}

	// Switch to low_priv (no direct grants on vault.secrets).
	mustExec(`SET ROLE low_priv`)
	requireErrCode(`SELECT * FROM vault.secrets`, "42501")
	requireErrCode(`SELECT vault.invoker_count()`, "42501")

	// Calling SECURITY DEFINER functions succeeds and sees all rows as data_owner!
	out = mustExec(`SELECT vault.definer_count()`)
	if out.Rows[0][0].IntVal != 2 {
		t.Fatalf("definer_count = %d, want 2", out.Rows[0][0].IntVal)
	}
	out = mustExec(`SELECT vault.definer_insert_and_who(3, 't2', 'secret-t2-added')`)
	if out.Rows[0][0].StrVal != "data_owner" {
		t.Fatalf("current_user inside SECURITY DEFINER = %q, want data_owner", out.Rows[0][0].StrVal)
	}
	// Caller's role is restored to low_priv immediately after call.
	out = mustExec(`SELECT current_user`)
	if out.Rows[0][0].StrVal != "low_priv" {
		t.Fatalf("current_user after SECURITY DEFINER = %q, want low_priv", out.Rows[0][0].StrVal)
	}
	out = mustExec(`SHOW role`)
	if out.Rows[0][0].StrVal != "low_priv" {
		t.Fatalf("SHOW role after SECURITY DEFINER = %q, want low_priv", out.Rows[0][0].StrVal)
	}

	// Nested SECURITY DEFINER calling SECURITY INVOKER executes inner as data_owner and restores low_priv.
	out = mustExec(`SELECT vault.nested_definer()`)
	if out.Rows[0][0].StrVal != "data_owner:3" {
		t.Fatalf("nested_definer = %q, want data_owner:3", out.Rows[0][0].StrVal)
	}
	out = mustExec(`SELECT current_user`)
	if out.Rows[0][0].StrVal != "low_priv" {
		t.Fatalf("current_user after nested_definer = %q, want low_priv", out.Rows[0][0].StrVal)
	}

	// Error inside SECURITY DEFINER still restores caller role!
	requireErrCode(`SELECT vault.definer_boom()`, "P0001")
	out = mustExec(`SELECT current_user`)
	if out.Rows[0][0].StrVal != "low_priv" {
		t.Fatalf("current_user after definer_boom = %q, want low_priv", out.Rows[0][0].StrVal)
	}
	requireErrCode(`SELECT * FROM vault.secrets`, "42501")

	// ALTER FUNCTION ... SECURITY INVOKER removes definer escalation.
	mustExec(`RESET ROLE`)
	mustExec(`ALTER FUNCTION vault.definer_count() SECURITY INVOKER`)
	mustExec(`SET ROLE low_priv`)
	requireErrCode(`SELECT vault.definer_count()`, "42501")

	// Grant SELECT on vault.secrets to low_priv: now direct SELECT sees only RLS-permitted tenant 't1' (1 row),
	// while ALTER FUNCTION back to SECURITY DEFINER sees all 3 rows!
	mustExec(`RESET ROLE`)
	mustExec(`GRANT SELECT ON vault.secrets TO low_priv`)
	mustExec(`ALTER FUNCTION vault.definer_count() SECURITY DEFINER`)
	mustExec(`SET ROLE low_priv`)
	out = mustExec(`SELECT count(*) FROM vault.secrets`)
	if out.Rows[0][0].IntVal != 1 {
		t.Fatalf("low_priv direct RLS SELECT count = %d, want 1", out.Rows[0][0].IntVal)
	}
	out = mustExec(`SELECT vault.definer_count()`)
	if out.Rows[0][0].IntVal != 3 {
		t.Fatalf("low_priv SECURITY DEFINER count = %d, want 3", out.Rows[0][0].IntVal)
	}
}

func TestGrantNonOwnerDDLAndTruncateSchemaUsage(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	mustExec := func(sqlText string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sqlText, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sqlText, err)
		}
		return out
	}

	requireErrCode := func(sqlText, code string) {
		t.Helper()
		_, err := db.exec(ctx, sqlText, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sqlText, code)
		}
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sqlText, code, got, err)
		}
	}

	mustExec(`CREATE ROLE non_owner`)
	mustExec(`CREATE TABLE owned_tbl (id int PRIMARY KEY, v int)`)
	mustExec(`CREATE INDEX owned_idx ON owned_tbl (v)`)
	mustExec(`CREATE COLLATION custom_coll (provider = icu, locale = 'en-US')`)
	mustExec(`CREATE TYPE custom_enum AS ENUM ('a', 'b')`)
	mustExec(`CREATE SCHEMA priv_sch`)
	mustExec(`CREATE TABLE priv_sch.t (id int PRIMARY KEY)`)
	mustExec(`INSERT INTO priv_sch.t VALUES (1)`)
	// Grant TRUNCATE on priv_sch.t WITHOUT granting USAGE on priv_sch.
	mustExec(`GRANT TRUNCATE ON priv_sch.t TO non_owner`)

	mustExec(`SET ROLE non_owner`)
	requireErrCode(`CREATE INDEX rogue_idx ON owned_tbl (v)`, "42501")
	requireErrCode(`DROP INDEX owned_idx`, "42501")
	requireErrCode(`ALTER INDEX owned_idx RENAME TO stolen_idx`, "42501")
	requireErrCode(`DROP COLLATION custom_coll`, "42501")
	requireErrCode(`DROP TYPE custom_enum`, "42501")
	// Without USAGE on priv_sch, TRUNCATE priv_sch.t must be denied.
	requireErrCode(`TRUNCATE priv_sch.t`, "42501")

	// After granting USAGE on priv_sch, TRUNCATE priv_sch.t succeeds.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SCHEMA priv_sch TO non_owner`)
	mustExec(`SET ROLE non_owner`)
	mustExec(`TRUNCATE priv_sch.t`)
}

func TestGrantRevokeGrantorIsolation(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	mustExec := func(sqlText string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sqlText, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sqlText, err)
		}
		return out
	}

	requireErrCode := func(sqlText, code string) {
		t.Helper()
		_, err := db.exec(ctx, sqlText, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sqlText, code)
		}
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sqlText, code, got, err)
		}
	}

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE carol`)
	mustExec(`CREATE ROLE dave`)
	mustExec(`CREATE TABLE shared_tbl (id int PRIMARY KEY)`)
	mustExec(`INSERT INTO shared_tbl VALUES (1)`)

	// Owner (supersql) grants SELECT to carol, and SELECT WITH GRANT OPTION to alice.
	mustExec(`GRANT SELECT ON shared_tbl TO carol`)
	mustExec(`GRANT SELECT ON shared_tbl TO alice WITH GRANT OPTION`)

	// alice grants SELECT to dave, then tries to revoke SELECT from both carol and dave.
	mustExec(`SET ROLE alice`)
	mustExec(`GRANT SELECT ON shared_tbl TO dave`)
	// Revoking carol must NOT strip the owner's grant to carol.
	mustExec(`REVOKE SELECT ON shared_tbl FROM carol`)
	// Revoking dave MUST strip alice's own grant to dave.
	mustExec(`REVOKE SELECT ON shared_tbl FROM dave`)

	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE carol`)
	out := mustExec(`SELECT count(*) FROM shared_tbl`)
	if out.Rows[0][0].IntVal != 1 {
		t.Fatalf("carol lost owner-granted SELECT after non-owner REVOKE: %+v", out.Rows)
	}

	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE dave`)
	requireErrCode(`SELECT count(*) FROM shared_tbl`, "42501")

	// Owner can revoke carol's grant.
	mustExec(`RESET ROLE`)
	mustExec(`REVOKE SELECT ON shared_tbl FROM carol`)
	mustExec(`SET ROLE carol`)
	requireErrCode(`SELECT count(*) FROM shared_tbl`, "42501")
}

func TestSecurityDefinerBlocksRoleChanges(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	mustExec := func(sqlText string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sqlText, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sqlText, err)
		}
		return out
	}

	requireErrCode := func(sqlText, code string) {
		t.Helper()
		_, err := db.exec(ctx, sqlText, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sqlText, code)
		}
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sqlText, code, got, err)
		}
	}

	mustExec(`CREATE ROLE caller_role`)
	mustExec(`CREATE ROLE other_role`)
	mustExec(`CREATE FUNCTION try_set_role() RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
	BEGIN
		SET ROLE other_role;
	END;
	$$`)
	mustExec(`CREATE FUNCTION try_reset_role() RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
	BEGIN
		RESET ROLE;
	END;
	$$`)
	mustExec(`CREATE FUNCTION try_reset_all() RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
	BEGIN
		RESET ALL;
	END;
	$$`)

	mustExec(`SET ROLE caller_role`)
	for _, call := range []string{
		`SELECT try_set_role()`,
		`SELECT try_reset_role()`,
		`SELECT try_reset_all()`,
	} {
		requireErrCode(call, "0A000")
		out := mustExec(`SELECT current_user`)
		if out.Rows[0][0].StrVal != "caller_role" {
			t.Fatalf("after %s: current_user = %q, want caller_role", call, out.Rows[0][0].StrVal)
		}
	}
}

func TestGrantSubtransactionNotifyRollback(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	sqlDB := sql.OpenDB(NewConnectorWithCatalog(database))
	defer func() { _ = sqlDB.Close() }()

	listener, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("listener Conn: %v", err)
	}
	defer func() { _ = listener.Close() }()

	sender, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("sender Conn: %v", err)
	}
	defer func() { _ = sender.Close() }()

	if _, err := listener.ExecContext(ctx, `LISTEN subtx_ch`); err != nil {
		t.Fatalf("LISTEN subtx_ch: %v", err)
	}

	if _, err := sender.ExecContext(ctx, `DO $$
	BEGIN
		BEGIN
			PERFORM pg_notify('subtx_ch', 'rolled_back');
			RAISE EXCEPTION 'boom';
		EXCEPTION WHEN OTHERS THEN
			PERFORM pg_notify('subtx_ch', 'kept');
		END;
	END;
	$$`); err != nil {
		t.Fatalf("DO subtransaction notify block: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	got, err := waitSQLConnNotification(waitCtx, listener)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if got.Channel != "subtx_ch" || got.Payload != "kept" {
		t.Fatalf("first notification = %+v, want channel=subtx_ch payload=kept", got)
	}

	extraCtx, extraCancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer extraCancel()
	if extra, err := waitSQLConnNotification(extraCtx, listener); err == nil {
		t.Fatalf("unexpected extra notification (rolled-back leak?): %+v", extra)
	}
}
