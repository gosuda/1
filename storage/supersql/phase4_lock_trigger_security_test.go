package supersql

import (
	"fmt"
	"testing"
)

func openPhase4LockTriggerSecurityDB(t *testing.T) (*Catalog, func(string) outcome, func(string, string)) {
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

	return db, mustExec, requireErrCode
}

func TestPhase4LockTriggerSecurityCreateTriggerPrivileges(t *testing.T) {
	_, mustExec, requireErrCode := openPhase4LockTriggerSecurityDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE SCHEMA priv`)
	mustExec(`CREATE FUNCTION priv.trig_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END; $$`)
	mustExec(`REVOKE EXECUTE ON FUNCTION priv.trig_fn() FROM PUBLIC`)
	mustExec(`CREATE FUNCTION public.pub_trig_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END; $$`)
	mustExec(`REVOKE EXECUTE ON FUNCTION public.pub_trig_fn() FROM PUBLIC`)

	mustExec(`CREATE TABLE public.alice_t (id int PRIMARY KEY, body text)`)
	mustExec(`ALTER TABLE public.alice_t OWNER TO alice`)
	mustExec(`CREATE TABLE public.alice_ts_t (id int PRIMARY KEY, body text, doc tsvector)`)
	mustExec(`ALTER TABLE public.alice_ts_t OWNER TO alice`)

	// Non-owner without schema USAGE or function EXECUTE fails with 42501.
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TRIGGER t_priv BEFORE INSERT ON public.alice_t FOR EACH ROW EXECUTE FUNCTION priv.trig_fn()`, "42501")
	// Existence probing in a private schema without USAGE must return 42501 (not 42883).
	requireErrCode(`CREATE TRIGGER t_probe BEFORE INSERT ON public.alice_t FOR EACH ROW EXECUTE FUNCTION priv.nonexistent_fn()`, "42501")
	// Schema has USAGE (public), but function lacks EXECUTE -> 42501.
	requireErrCode(`CREATE TRIGGER t_pub BEFORE INSERT ON public.alice_t FOR EACH ROW EXECUTE FUNCTION public.pub_trig_fn()`, "42501")
	// Unqualified resolution via search_path into private schema without USAGE -> 42501.
	mustExec(`SET search_path = priv, public`)
	requireErrCode(`CREATE TRIGGER t_sp BEFORE INSERT ON public.alice_t FOR EACH ROW EXECUTE FUNCTION trig_fn()`, "42501")
	mustExec(`SET search_path = public`)

	// Built-in pg_catalog.tsvector_update_trigger* remain allowed.
	mustExec(`CREATE TRIGGER t_ts BEFORE INSERT OR UPDATE ON public.alice_ts_t FOR EACH ROW EXECUTE FUNCTION pg_catalog.tsvector_update_trigger(doc, 'pg_catalog.english', body)`)
	mustExec(`CREATE TRIGGER t_tsc BEFORE INSERT OR UPDATE ON public.alice_ts_t FOR EACH ROW EXECUTE FUNCTION tsvector_update_trigger_column(doc, cfg, body)`)

	// Grant schema USAGE only (still no EXECUTE on priv.trig_fn) -> still fails 42501.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SCHEMA priv TO alice`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TRIGGER t_priv BEFORE INSERT ON public.alice_t FOR EACH ROW EXECUTE FUNCTION priv.trig_fn()`, "42501")

	// Grant EXECUTE on priv.trig_fn -> CREATE TRIGGER now succeeds.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT EXECUTE ON FUNCTION priv.trig_fn() TO alice`)
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE TRIGGER t_priv BEFORE INSERT ON public.alice_t FOR EACH ROW EXECUTE FUNCTION priv.trig_fn()`)
	mustExec(`INSERT INTO public.alice_t VALUES (1, 'hello world')`)

	// Revoke EXECUTE -> new CREATE TRIGGER fails 42501.
	mustExec(`RESET ROLE`)
	mustExec(`REVOKE EXECUTE ON FUNCTION priv.trig_fn() FROM alice`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TRIGGER t_priv_upd BEFORE UPDATE ON public.alice_t FOR EACH ROW EXECUTE FUNCTION priv.trig_fn()`, "42501")

	// Re-grant EXECUTE but revoke schema USAGE -> fails 42501.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT EXECUTE ON FUNCTION priv.trig_fn() TO alice`)
	mustExec(`REVOKE USAGE ON SCHEMA priv FROM alice`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TRIGGER t_priv_no_usage BEFORE UPDATE ON public.alice_t FOR EACH ROW EXECUTE FUNCTION priv.trig_fn()`, "42501")
}

func TestPhase4LockTriggerSecurityLockTablePrivileges(t *testing.T) {
	db, mustExec, _ := openPhase4LockTriggerSecurityDB(t)
	ctx := t.Context()

	roles := []string{"unpriv", "sel_only", "ins_only", "upd_only", "del_only", "trunc_only", "no_schema_usage"}
	for _, r := range roles {
		mustExec(`CREATE ROLE ` + r)
	}
	mustExec(`CREATE SCHEMA sec`)
	mustExec(`CREATE TABLE sec.t (id int PRIMARY KEY, v int)`)
	for _, r := range []string{"unpriv", "sel_only", "ins_only", "upd_only", "del_only", "trunc_only"} {
		mustExec(`GRANT USAGE ON SCHEMA sec TO ` + r)
	}
	mustExec(`GRANT SELECT ON sec.t TO sel_only`)
	mustExec(`GRANT INSERT ON sec.t TO ins_only`)
	mustExec(`GRANT UPDATE ON sec.t TO upd_only`)
	mustExec(`GRANT DELETE ON sec.t TO del_only`)
	mustExec(`GRANT TRUNCATE ON sec.t TO trunc_only`)
	mustExec(`GRANT ALL PRIVILEGES ON sec.t TO no_schema_usage`)

	allModes := []string{
		"ACCESS SHARE",
		"ROW SHARE",
		"ROW EXCLUSIVE",
		"SHARE UPDATE EXCLUSIVE",
		"SHARE",
		"SHARE ROW EXCLUSIVE",
		"EXCLUSIVE",
		"ACCESS EXCLUSIVE",
	}

	checkLock := func(role, target, mode string, wantAllowed bool) {
		t.Helper()
		mustExec(`SET ROLE ` + role)
		defer mustExec(`RESET ROLE`)
		mustExec(`BEGIN`)
		sql := fmt.Sprintf(`LOCK TABLE %s IN %s MODE`, target, mode)
		_, err := db.exec(ctx, sql, 0)
		if wantAllowed {
			if err != nil {
				_, _ = db.exec(ctx, `ROLLBACK`, 0)
				t.Fatalf("role=%s %s: expected success, got error: %v", role, sql, err)
			}
			mustExec(`COMMIT`)
			return
		}
		_, _ = db.exec(ctx, `ROLLBACK`, 0)
		if err == nil {
			t.Fatalf("role=%s %s: expected SQLSTATE 42501, got nil", role, sql)
		}
		if got := SQLState(err); got != "42501" {
			t.Fatalf("role=%s %s: expected SQLSTATE 42501, got %q (%v)", role, sql, got, err)
		}
	}

	// Unprivileged user fails 42501 for all modes.
	for _, mode := range allModes {
		checkLock("unpriv", "sec.t", mode, false)
	}

	// SELECT only succeeds for ACCESS SHARE, fails 42501 for all other modes.
	for _, mode := range allModes {
		checkLock("sel_only", "sec.t", mode, mode == "ACCESS SHARE")
	}

	// INSERT only succeeds for ACCESS SHARE and ROW EXCLUSIVE, fails 42501 for all other modes.
	for _, mode := range allModes {
		checkLock("ins_only", "sec.t", mode, mode == "ACCESS SHARE" || mode == "ROW EXCLUSIVE")
	}

	// UPDATE, DELETE, TRUNCATE succeed for all modes.
	for _, role := range []string{"upd_only", "del_only", "trunc_only"} {
		for _, mode := range allModes {
			checkLock(role, "sec.t", mode, true)
		}
	}

	// Missing schema USAGE fails 42501 even with ALL table privileges (and prevents existence probing).
	for _, mode := range []string{"ACCESS SHARE", "ROW EXCLUSIVE", "ACCESS EXCLUSIVE"} {
		checkLock("no_schema_usage", "sec.t", mode, false)
	}
	checkLock("no_schema_usage", "sec.nonexistent", "ACCESS SHARE", false)
}

func TestPhase4LockTriggerSecuritySelectForLockPrivileges(t *testing.T) {
	_, mustExec, requireErrCode := openPhase4LockTriggerSecurityDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE SCHEMA app`)
	mustExec(`CREATE TABLE app.items (id int PRIMARY KEY, name text NOT NULL)`)
	mustExec(`INSERT INTO app.items VALUES (1, 'one'), (2, 'two')`)
	mustExec(`CREATE VIEW public.items_v AS SELECT id, name FROM app.items`)
	mustExec(`CREATE VIEW public.items_inv_v WITH (security_invoker = true) AS SELECT id, name FROM app.items`)

	mustExec(`GRANT USAGE ON SCHEMA app TO alice`)
	mustExec(`GRANT SELECT ON app.items TO alice`)
	mustExec(`GRANT SELECT ON public.items_v TO alice`)
	mustExec(`GRANT SELECT ON public.items_inv_v TO alice`)

	clauses := []string{
		"FOR UPDATE",
		"FOR NO KEY UPDATE",
		"FOR SHARE",
		"FOR KEY SHARE",
	}

	// SELECT-only user can read without row locks, but cannot acquire any row lock
	// on either the direct table, a subquery over the table, a definer view, or an invoker view.
	mustExec(`SET ROLE alice`)
	if out := mustExec(`SELECT count(*) FROM app.items`); out.Rows[0][0].IntVal != 2 {
		t.Fatalf("SELECT app.items count = %d, want 2", out.Rows[0][0].IntVal)
	}
	if out := mustExec(`SELECT count(*) FROM public.items_v`); out.Rows[0][0].IntVal != 2 {
		t.Fatalf("SELECT public.items_v count = %d, want 2", out.Rows[0][0].IntVal)
	}
	for _, clause := range clauses {
		requireErrCode(`SELECT id FROM app.items `+clause, "42501")
		requireErrCode(`SELECT id FROM (SELECT id FROM app.items) s `+clause, "42501")
		requireErrCode(`SELECT id FROM public.items_v `+clause, "42501")
		requireErrCode(`SELECT id FROM public.items_v `+clause+` OF items_v`, "42501")
		requireErrCode(`SELECT id FROM public.items_inv_v `+clause, "42501")
	}

	// Grant UPDATE on direct table only: direct table succeeds; views still fail 42501.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT UPDATE ON app.items TO alice`)
	mustExec(`SET ROLE alice`)
	for _, clause := range clauses {
		out := mustExec(`SELECT id FROM app.items ORDER BY id ` + clause)
		if len(out.Rows) != 2 {
			t.Fatalf("SELECT app.items %s rows = %d, want 2", clause, len(out.Rows))
		}
		requireErrCode(`SELECT id FROM public.items_v `+clause, "42501")
		requireErrCode(`SELECT id FROM public.items_inv_v `+clause, "42501")
	}

	// Revoke UPDATE on direct table and grant UPDATE on definer view + invoker view:
	// - definer view succeeds (runs underlying lock check as view owner)
	// - direct table fails 42501 again
	// - security_invoker view still fails 42501 because alice lacks UPDATE on app.items
	mustExec(`RESET ROLE`)
	mustExec(`REVOKE UPDATE ON app.items FROM alice`)
	mustExec(`GRANT UPDATE ON public.items_v TO alice`)
	mustExec(`GRANT UPDATE ON public.items_inv_v TO alice`)
	mustExec(`SET ROLE alice`)
	for _, clause := range clauses {
		out := mustExec(`SELECT id FROM public.items_v ORDER BY id ` + clause)
		if len(out.Rows) != 2 {
			t.Fatalf("SELECT public.items_v %s rows = %d, want 2", clause, len(out.Rows))
		}
		requireErrCode(`SELECT id FROM app.items `+clause, "42501")
		requireErrCode(`SELECT id FROM public.items_inv_v `+clause, "42501")
	}

	// Grant UPDATE on app.items as well -> security_invoker view now succeeds too.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT UPDATE ON app.items TO alice`)
	mustExec(`SET ROLE alice`)
	for _, clause := range clauses {
		out := mustExec(`SELECT id FROM public.items_inv_v ORDER BY id ` + clause)
		if len(out.Rows) != 2 {
			t.Fatalf("SELECT public.items_inv_v %s rows = %d, want 2", clause, len(out.Rows))
		}
	}
}
