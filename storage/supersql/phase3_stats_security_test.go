package supersql

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestPhase3StatsPrivilegeAndOwnershipSecurity(t *testing.T) {
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

	// Verify model.Principal zero value vs explicit roles.
	var defaultPrincipal model.Principal
	if defaultPrincipal.Name() != "embedded" || defaultPrincipal.OID() != 10 || !defaultPrincipal.IsSuperuser() {
		t.Fatalf("default principal = (%q, %d, %v), want (embedded, 10, true)", defaultPrincipal.Name(), defaultPrincipal.OID(), defaultPrincipal.IsSuperuser())
	}
	nonSuper := model.NewPrincipal("mallory", 0)
	if nonSuper.Name() != "mallory" || nonSuper.OID() != 10 || nonSuper.IsSuperuser() {
		t.Fatalf("mallory principal = (%q, %d, %v), want (mallory, 10, false)", nonSuper.Name(), nonSuper.OID(), nonSuper.IsSuperuser())
	}

	// Admin creates non-RLS table secret_tbl, extended statistics, and runs ANALYZE.
	mustExec(`CREATE ROLE mallory`)
	mustExec(`CREATE SCHEMA mallory_sch AUTHORIZATION mallory`)
	mustExec(`CREATE TABLE secret_tbl (id int PRIMARY KEY, secret text)`)
	mustExec(`INSERT INTO secret_tbl VALUES (1, 'top-secret-a'), (2, 'top-secret-b'), (3, 'top-secret-a')`)
	mustExec(`CREATE STATISTICS secret_ext_st (ndistinct, dependencies, mcv) ON id, secret FROM secret_tbl`)
	mustExec(`ANALYZE secret_tbl`)

	// Superuser sees rows in pg_stats, pg_statistic, and pg_statistic_ext_data.
	if out := mustExec(`SELECT count(*) FROM pg_stats WHERE tablename = 'secret_tbl'`); out.Rows[0][0].IntVal == 0 {
		t.Fatalf("superuser pg_stats count for secret_tbl = 0, want > 0")
	}
	if out := mustExec(`SELECT count(*) FROM pg_statistic`); out.Rows[0][0].IntVal == 0 {
		t.Fatalf("superuser pg_statistic count = 0, want > 0")
	}
	if out := mustExec(`SELECT count(*) FROM pg_statistic_ext_data`); out.Rows[0][0].IntVal == 0 {
		t.Fatalf("superuser pg_statistic_ext_data count = 0, want > 0")
	}

	// Switch to unprivileged user mallory (no SELECT on secret_tbl).
	mustExec(`SET ROLE mallory`)

	// Verify Settings.Principal() reflects active role mallory.
	if p := db.settings.Principal(); p.Name() != "mallory" || p.IsSuperuser() {
		t.Fatalf("Settings.Principal() under SET ROLE mallory = (%q, super=%v), want (mallory, false)", p.Name(), p.IsSuperuser())
	}

	// 1. Without SELECT on non-RLS secret_tbl: pg_stats, pg_statistic, pg_statistic_ext_data all return 0 rows.
	if out := mustExec(`SELECT * FROM pg_stats WHERE tablename = 'secret_tbl'`); len(out.Rows) != 0 {
		t.Fatalf("mallory without SELECT saw %d pg_stats rows for secret_tbl, want 0", len(out.Rows))
	}
	if out := mustExec(`SELECT * FROM pg_statistic`); len(out.Rows) != 0 {
		t.Fatalf("mallory saw %d pg_statistic rows, want 0", len(out.Rows))
	}
	if out := mustExec(`SELECT * FROM pg_statistic_ext_data`); len(out.Rows) != 0 {
		t.Fatalf("mallory saw %d pg_statistic_ext_data rows, want 0", len(out.Rows))
	}

	// 2. ANALYZE and statistics DDL on unowned secret_tbl fail with 42501.
	requireErrCode(`ANALYZE secret_tbl`, "42501")
	requireErrCode(`CREATE STATISTICS mallory_sch.st ON id, secret FROM secret_tbl`, "42501")
	requireErrCode(`ALTER STATISTICS secret_ext_st SET STATISTICS 100`, "42501")
	requireErrCode(`ALTER STATISTICS secret_ext_st RENAME TO hijacked_st`, "42501")
	requireErrCode(`ALTER STATISTICS secret_ext_st SET SCHEMA mallory_sch`, "42501")
	requireErrCode(`DROP STATISTICS secret_ext_st`, "42501")

	// 3. Grant SELECT on secret_tbl to mallory -> pg_stats visible, pg_statistic & pg_statistic_ext_data still 0 rows.
	mustExec(`RESET ROLE`)
	if p := db.settings.Principal(); p.Name() != "embedded" || !p.IsSuperuser() {
		t.Fatalf("Settings.Principal() after RESET ROLE = (%q, super=%v), want (embedded, true)", p.Name(), p.IsSuperuser())
	}
	mustExec(`GRANT SELECT ON secret_tbl TO mallory`)
	mustExec(`ANALYZE secret_tbl`)
	if out := mustExec(`SELECT count(*) FROM pg_stats WHERE tablename = 'secret_tbl'`); out.Rows[0][0].IntVal == 0 {
		t.Fatalf("superuser pg_stats count for secret_tbl after GRANT = 0, want > 0")
	}
	mustExec(`SET ROLE mallory`)

	if out := mustExec(`SELECT * FROM pg_stats WHERE tablename = 'secret_tbl'`); len(out.Rows) == 0 {
		t.Fatalf("mallory with SELECT saw 0 pg_stats rows for secret_tbl, want > 0")
	}
	if out := mustExec(`SELECT * FROM pg_statistic`); len(out.Rows) != 0 {
		t.Fatalf("mallory with SELECT saw %d pg_statistic rows, want 0", len(out.Rows))
	}
	if out := mustExec(`SELECT * FROM pg_statistic_ext_data`); len(out.Rows) != 0 {
		t.Fatalf("mallory with SELECT saw %d pg_statistic_ext_data rows, want 0", len(out.Rows))
	}
	// SELECT privilege alone still does not permit ANALYZE or CREATE STATISTICS.
	requireErrCode(`ANALYZE secret_tbl`, "42501")
	requireErrCode(`CREATE STATISTICS mallory_sch.st ON id, secret FROM secret_tbl`, "42501")

	// 4. Enable RLS on secret_tbl -> pg_stats hides rows from non-owner non-bypassrls mallory even with SELECT.
	mustExec(`RESET ROLE`)
	mustExec(`ALTER TABLE secret_tbl ENABLE ROW LEVEL SECURITY`)
	mustExec(`ANALYZE secret_tbl`)
	if out := mustExec(`SELECT count(*) FROM pg_stats WHERE tablename = 'secret_tbl'`); out.Rows[0][0].IntVal == 0 {
		t.Fatalf("superuser pg_stats count for RLS secret_tbl = 0, want > 0")
	}
	mustExec(`SET ROLE mallory`)

	if out := mustExec(`SELECT * FROM pg_stats WHERE tablename = 'secret_tbl'`); len(out.Rows) != 0 {
		t.Fatalf("mallory saw %d pg_stats rows for RLS-enabled secret_tbl, want 0", len(out.Rows))
	}

	// 5. Bare ANALYZE as mallory succeeds by analyzing mallory's own tables and skipping secret_tbl.
	mustExec(`CREATE TABLE mallory_sch.own_tbl (id int PRIMARY KEY, val text)`)
	mustExec(`INSERT INTO mallory_sch.own_tbl VALUES (1, 'alpha'), (2, 'beta')`)
	mustExec(`ANALYZE`)

	if out := mustExec(`SELECT * FROM pg_stats WHERE tablename = 'own_tbl'`); len(out.Rows) == 0 {
		t.Fatalf("mallory pg_stats for own_tbl after bare ANALYZE = 0 rows, want > 0")
	}
	if out := mustExec(`SELECT * FROM pg_stats WHERE tablename = 'secret_tbl'`); len(out.Rows) != 0 {
		t.Fatalf("mallory pg_stats for secret_tbl after bare ANALYZE = %d rows, want 0", len(out.Rows))
	}

	// 6. Owner statistics DDL and schema CREATE privilege enforcement.
	requireErrCode(`CREATE STATISTICS public.denied_st ON id, val FROM mallory_sch.own_tbl`, "42501")
	mustExec(`CREATE STATISTICS mallory_sch.own_st (ndistinct) ON id, val FROM mallory_sch.own_tbl`)
	mustExec(`ALTER STATISTICS mallory_sch.own_st SET STATISTICS 50`)
	requireErrCode(`ALTER STATISTICS mallory_sch.own_st SET SCHEMA public`, "42501")
	mustExec(`ALTER STATISTICS mallory_sch.own_st RENAME TO own_st_renamed`)
	mustExec(`DROP STATISTICS mallory_sch.own_st_renamed`)

	// Another non-owner cannot mutate or drop mallory's statistics object.
	mustExec(`CREATE STATISTICS mallory_sch.own_st2 (ndistinct) ON id, val FROM mallory_sch.own_tbl`)
	mustExec(`RESET ROLE`)
	mustExec(`CREATE ROLE eve`)
	mustExec(`GRANT USAGE ON SCHEMA mallory_sch TO eve`)
	mustExec(`SET ROLE eve`)
	requireErrCode(`ALTER STATISTICS mallory_sch.own_st2 SET STATISTICS 10`, "42501")
	requireErrCode(`ALTER STATISTICS mallory_sch.own_st2 RENAME TO eve_st`, "42501")
	requireErrCode(`DROP STATISTICS mallory_sch.own_st2`, "42501")
	// Bare ANALYZE as eve (owns 0 tables) also succeeds cleanly by skipping unowned tables.
	mustExec(`ANALYZE`)
}
