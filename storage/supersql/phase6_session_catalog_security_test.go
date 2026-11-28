package supersql

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
)

type noRoleCatalogSettings struct{}

func (noRoleCatalogSettings) CatalogSettings(context.Context) ([]catalogrel.Setting, error) {
	return nil, nil
}

type roleCatalogSettings struct{ role string }

func (roleCatalogSettings) CatalogSettings(context.Context) ([]catalogrel.Setting, error) {
	return nil, nil
}

func (s roleCatalogSettings) CurrentRole() string { return s.role }

func openCatalogReadView(t *testing.T, ctx context.Context, db *Catalog) (txn.ReadView, func()) {
	t.Helper()
	tx, err := db.manager.Begin(ctx, txn.TransactionOptions{Mode: txn.ReadOnly})
	if err != nil {
		t.Fatalf("Begin read-only txn: %v", err)
	}
	full := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
	var deps []txn.ReadDependency
	for _, layout := range db.catalog.Layouts() {
		deps = append(deps, txn.ReadDependency{Target: layout.Target, KeyLayout: db.catalog.KeyLayout(), Spans: full})
	}
	for _, aux := range db.source.AuxiliaryScanStores() {
		deps = append(deps, txn.ReadDependency{Target: aux.Target, KeyLayout: aux.KeyLayout, Spans: full})
	}
	view, err := tx.ReadView(ctx, deps)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ReadView: %v", err)
	}
	return view, func() {
		_ = view.Close()
		_ = tx.Rollback(ctx)
	}
}

func TestPhase6SessionCatalogCursorStatsSecurity(t *testing.T) {
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

	mustExec := func(query string) outcome {
		t.Helper()
		out, err := db.exec(ctx, query, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", query, err)
		}
		return out
	}

	fetchCursorRows := func(cursorName, selectSQL string) outcome {
		t.Helper()
		mustExec(`BEGIN`)
		mustExec(`DECLARE ` + cursorName + ` CURSOR FOR ` + selectSQL)
		out := mustExec(`FETCH ALL FROM ` + cursorName)
		mustExec(`COMMIT`)
		return out
	}

	mustExec(`CREATE ROLE mallory`)
	mustExec(`CREATE TABLE secret_tbl (id int PRIMARY KEY, secret text)`)
	mustExec(`INSERT INTO secret_tbl VALUES (1, 'top-secret-a'), (2, 'top-secret-b'), (3, 'top-secret-a')`)
	mustExec(`CREATE STATISTICS secret_ext_st (ndistinct, dependencies, mcv) ON id, secret FROM secret_tbl`)
	mustExec(`ANALYZE secret_tbl`)

	// Superuser sees rows via DECLARE CURSOR on pg_stats, pg_statistic, and pg_statistic_ext_data.
	if out := fetchCursorRows("su_stats", `SELECT * FROM pg_stats WHERE tablename = 'secret_tbl'`); len(out.Rows) == 0 {
		t.Fatalf("superuser cursor on pg_stats returned 0 rows, want > 0")
	}
	if out := fetchCursorRows("su_stat", `SELECT * FROM pg_statistic`); len(out.Rows) == 0 {
		t.Fatalf("superuser cursor on pg_statistic returned 0 rows, want > 0")
	}
	if out := fetchCursorRows("su_ext", `SELECT * FROM pg_statistic_ext_data`); len(out.Rows) == 0 {
		t.Fatalf("superuser cursor on pg_statistic_ext_data returned 0 rows, want > 0")
	}

	// Unprivileged role mallory sees 0 rows via DECLARE CURSOR (matching direct SELECT).
	mustExec(`SET ROLE mallory`)
	if out := fetchCursorRows("m_stats", `SELECT * FROM pg_stats WHERE tablename = 'secret_tbl'`); len(out.Rows) != 0 {
		t.Fatalf("mallory cursor on pg_stats returned %d rows, want 0", len(out.Rows))
	}
	if out := fetchCursorRows("m_stat", `SELECT * FROM pg_statistic`); len(out.Rows) != 0 {
		t.Fatalf("mallory cursor on pg_statistic returned %d rows, want 0", len(out.Rows))
	}
	if out := fetchCursorRows("m_ext", `SELECT * FROM pg_statistic_ext_data`); len(out.Rows) != 0 {
		t.Fatalf("mallory cursor on pg_statistic_ext_data returned %d rows, want 0", len(out.Rows))
	}

	// Grant SELECT on secret_tbl: pg_stats cursor sees rows, pg_statistic / pg_statistic_ext_data still return 0.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT SELECT ON secret_tbl TO mallory`)
	mustExec(`ANALYZE secret_tbl`)
	mustExec(`SET ROLE mallory`)
	if out := fetchCursorRows("m_stats_granted", `SELECT * FROM pg_stats WHERE tablename = 'secret_tbl'`); len(out.Rows) == 0 {
		t.Fatalf("mallory cursor on pg_stats after GRANT SELECT returned 0 rows, want > 0")
	}
	if out := fetchCursorRows("m_stat_granted", `SELECT * FROM pg_statistic`); len(out.Rows) != 0 {
		t.Fatalf("mallory cursor on pg_statistic after GRANT SELECT returned %d rows, want 0", len(out.Rows))
	}
	if out := fetchCursorRows("m_ext_granted", `SELECT * FROM pg_statistic_ext_data`); len(out.Rows) != 0 {
		t.Fatalf("mallory cursor on pg_statistic_ext_data after GRANT SELECT returned %d rows, want 0", len(out.Rows))
	}

	// Fail-closed check: non-nil catalogrel.Settings lacking RoleProvider must return 0 rows for statistics catalogs.
	mustExec(`RESET ROLE`)
	view, cleanup := openCatalogReadView(t, ctx, db)
	defer cleanup()
	scanner, err := catalogrel.AttachSettings(db.source, noRoleCatalogSettings{})
	if err != nil {
		t.Fatalf("AttachSettings: %v", err)
	}
	scanner, err = catalogrel.AttachView(scanner, view)
	if err != nil {
		t.Fatalf("AttachView: %v", err)
	}
	for _, relID := range []catalogrel.RelationID{catalogrel.IDPgStats, catalogrel.IDPgStatistic, catalogrel.IDPgStatisticExtData} {
		cur, err := scanner.OpenCatalog(ctx, catalogrel.ScanRequest{Relation: relID, BatchRows: 64, BatchBytes: 1 << 20})
		if err != nil {
			t.Fatalf("OpenCatalog(%d): %v", relID, err)
		}
		var batch columnar.Batch
		batch.MaxRows = 64
		totalRows := 0
		for {
			err := cur.Next(ctx, &batch)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = cur.Close()
				t.Fatalf("Next(%d): %v", relID, err)
			}
			totalRows += batch.Rows()
		}
		_ = cur.Close()
		if totalRows != 0 {
			t.Fatalf("fail-closed check: relation %d returned %d rows for Settings lacking RoleProvider, want 0", relID, totalRows)
		}
	}
}

func TestPhase6SessionCatalogSequencePrivilegeMasking(t *testing.T) {
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

	mustExec := func(query string) outcome {
		t.Helper()
		out, err := db.exec(ctx, query, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", query, err)
		}
		return out
	}

	mustExec(`CREATE ROLE mallory`)
	mustExec(`CREATE ROLE usage_user`)
	mustExec(`CREATE ROLE select_user`)
	mustExec(`CREATE ROLE seq_owner`)
	mustExec(`GRANT CREATE ON SCHEMA public TO seq_owner`)

	mustExec(`CREATE SEQUENCE public.secret_seq`)
	mustExec(`SELECT nextval('public.secret_seq')`)
	mustExec(`SELECT nextval('public.secret_seq')`)

	mustExec(`SET ROLE seq_owner`)
	mustExec(`CREATE SEQUENCE public.owned_seq`)
	mustExec(`SELECT nextval('public.owned_seq')`)
	mustExec(`RESET ROLE`)

	secretSeqOID := model.OID(mustExec(`SELECT oid FROM pg_class WHERE relname = 'secret_seq'`).Rows[0][0].IntVal)
	ownedSeqOID := model.OID(mustExec(`SELECT oid FROM pg_class WHERE relname = 'owned_seq'`).Rows[0][0].IntVal)

	callSequenceData := func(settings catalogrel.Settings, oid model.OID) (model.Value, error) {
		t.Helper()
		view, cleanup := openCatalogReadView(t, ctx, db)
		defer cleanup()
		var scanner catalogrel.Scanner = db.source
		if settings != nil {
			var err error
			scanner, err = catalogrel.AttachSettings(scanner, settings)
			if err != nil {
				t.Fatalf("AttachSettings: %v", err)
			}
		}
		scanner, err := catalogrel.AttachView(scanner, view)
		if err != nil {
			t.Fatalf("AttachView: %v", err)
		}
		src := scanner.(*catalogrel.Source)
		return src.SequenceData(ctx, oid)
	}

	// Superuser sees last_value in pg_sequences and succeeds on pg_get_sequence_data(oid).
	out := mustExec(`SELECT last_value FROM pg_catalog.pg_sequences WHERE sequencename = 'secret_seq'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IsNull || out.Rows[0][0].IntVal != 2 {
		t.Fatalf("superuser pg_sequences.last_value = %+v, want 2", out.Rows)
	}
	val, err := callSequenceData(roleCatalogSettings{role: ""}, secretSeqOID)
	if err != nil || len(val.ArrayVal) != 2 || val.ArrayVal[0].IntVal != 2 || val.ArrayVal[1].IntVal == 0 {
		t.Fatalf("superuser SequenceData(secret_seq) = (%+v, %v), want ((2, true), nil)", val, err)
	}

	// Unprivileged mallory: pg_sequences.last_value is NULL and pg_get_sequence_data(oid) returns 42501.
	mustExec(`SET ROLE mallory`)
	out = mustExec(`SELECT last_value FROM pg_catalog.pg_sequences WHERE sequencename = 'secret_seq'`)
	if len(out.Rows) != 1 || !out.Rows[0][0].IsNull {
		t.Fatalf("mallory pg_sequences.last_value = %+v, want NULL", out.Rows)
	}
	if _, err := callSequenceData(roleCatalogSettings{role: "mallory"}, secretSeqOID); err == nil || SQLState(err) != "42501" {
		t.Fatalf("mallory SequenceData(secret_seq): err=%v (SQLSTATE=%q), want 42501", err, SQLState(err))
	}

	// Fail-closed: Settings lacking RoleProvider is denied 42501 on SequenceData.
	if _, err := callSequenceData(noRoleCatalogSettings{}, secretSeqOID); err == nil || SQLState(err) != "42501" {
		t.Fatalf("noRoleCatalogSettings SequenceData(secret_seq): err=%v (SQLSTATE=%q), want 42501", err, SQLState(err))
	}

	// Also verify DECLARE CURSOR does not bypass sequence masking.
	mustExec(`BEGIN`)
	mustExec(`DECLARE seq_cur CURSOR FOR SELECT last_value FROM pg_catalog.pg_sequences WHERE sequencename = 'secret_seq'`)
	out = mustExec(`FETCH ALL FROM seq_cur`)
	mustExec(`COMMIT`)
	if len(out.Rows) != 1 || !out.Rows[0][0].IsNull {
		t.Fatalf("mallory cursor pg_sequences.last_value = %+v, want NULL", out.Rows)
	}

	// GRANT USAGE makes pg_sequences.last_value visible, while pg_get_sequence_data still requires SELECT (42501).
	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SEQUENCE public.secret_seq TO usage_user`)
	mustExec(`SET ROLE usage_user`)
	out = mustExec(`SELECT last_value FROM pg_catalog.pg_sequences WHERE sequencename = 'secret_seq'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IsNull || out.Rows[0][0].IntVal != 2 {
		t.Fatalf("usage_user pg_sequences.last_value = %+v, want 2", out.Rows)
	}
	if _, err := callSequenceData(roleCatalogSettings{role: "usage_user"}, secretSeqOID); err == nil || SQLState(err) != "42501" {
		t.Fatalf("usage_user SequenceData(secret_seq): err=%v (SQLSTATE=%q), want 42501", err, SQLState(err))
	}

	// GRANT SELECT makes both pg_sequences.last_value and pg_get_sequence_data(oid) succeed.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT SELECT ON SEQUENCE public.secret_seq TO select_user`)
	mustExec(`SET ROLE select_user`)
	out = mustExec(`SELECT last_value FROM pg_catalog.pg_sequences WHERE sequencename = 'secret_seq'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IsNull || out.Rows[0][0].IntVal != 2 {
		t.Fatalf("select_user pg_sequences.last_value = %+v, want 2", out.Rows)
	}
	val, err = callSequenceData(roleCatalogSettings{role: "select_user"}, secretSeqOID)
	if err != nil || len(val.ArrayVal) != 2 || val.ArrayVal[0].IntVal != 2 || val.ArrayVal[1].IntVal == 0 {
		t.Fatalf("select_user SequenceData(secret_seq) = (%+v, %v), want ((2, true), nil)", val, err)
	}

	// Owner sees own sequence data, but not secret_seq.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE seq_owner`)
	out = mustExec(`SELECT last_value FROM pg_catalog.pg_sequences WHERE sequencename = 'owned_seq'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IsNull || out.Rows[0][0].IntVal != 1 {
		t.Fatalf("seq_owner owned_seq last_value = %+v, want 1", out.Rows)
	}
	val, err = callSequenceData(roleCatalogSettings{role: "seq_owner"}, ownedSeqOID)
	if err != nil || len(val.ArrayVal) != 2 || val.ArrayVal[0].IntVal != 1 || val.ArrayVal[1].IntVal == 0 {
		t.Fatalf("seq_owner SequenceData(owned_seq) = (%+v, %v), want ((1, true), nil)", val, err)
	}
	out = mustExec(`SELECT last_value FROM pg_catalog.pg_sequences WHERE sequencename = 'secret_seq'`)
	if len(out.Rows) != 1 || !out.Rows[0][0].IsNull {
		t.Fatalf("seq_owner secret_seq last_value = %+v, want NULL", out.Rows)
	}
	if _, err := callSequenceData(roleCatalogSettings{role: "seq_owner"}, secretSeqOID); err == nil || SQLState(err) != "42501" {
		t.Fatalf("seq_owner SequenceData(secret_seq): err=%v (SQLSTATE=%q), want 42501", err, SQLState(err))
	}
}

func TestPhase6SessionCatalogSetRoleSessionUser(t *testing.T) {
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

	mustExec := func(query string) outcome {
		t.Helper()
		out, err := db.exec(ctx, query, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", query, err)
		}
		return out
	}

	mustExec(`CREATE ROLE tenant_a`)
	mustExec(`CREATE ROLE tenant_b`)
	mustExec(`CREATE ROLE tenant_c`)
	mustExec(`CREATE ROLE pooler`)
	mustExec(`CREATE ROLE outsider`)
	mustExec(`GRANT tenant_a TO pooler`)
	mustExec(`GRANT tenant_b TO pooler`)

	// 1. Superuser session_user can switch directly from tenant_a to tenant_b without RESET ROLE.
	mustExec(`SET ROLE tenant_a`)
	if got := mustExec(`SHOW role`).Rows[0][0].StrVal; got != "tenant_a" {
		t.Fatalf("SHOW role = %q, want tenant_a", got)
	}
	mustExec(`SET ROLE tenant_b`)
	if got := mustExec(`SHOW role`).Rows[0][0].StrVal; got != "tenant_b" {
		t.Fatalf("SHOW role after direct switch = %q, want tenant_b", got)
	}
	mustExec(`RESET ROLE`)

	// 2. Multi-role non-superuser session_user (pooler) can switch directly between member roles,
	//    but fails with 42501 for non-member roles.
	sqlDB := sql.OpenDB(NewConnectorWithCatalog(db))
	defer sqlDB.Close()

	setSessionUser := func(conn *sql.Conn, role string) {
		t.Helper()
		err := conn.Raw(func(driverConn any) error {
			setter, ok := driverConn.(interface {
				SetSessionUser(context.Context, string) error
			})
			if !ok {
				return errors.New("driverConn does not implement SetSessionUser")
			}
			return setter.SetSessionUser(ctx, role)
		})
		if err != nil {
			t.Fatalf("SetSessionUser(%q): %v", role, err)
		}
	}

	poolConn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("sqlDB.Conn(pooler): %v", err)
	}
	defer poolConn.Close()
	setSessionUser(poolConn, "pooler")

	if _, err := poolConn.ExecContext(ctx, `SET ROLE tenant_a`); err != nil {
		t.Fatalf("pooler SET ROLE tenant_a: %v", err)
	}
	if _, err := poolConn.ExecContext(ctx, `SET ROLE tenant_b`); err != nil {
		t.Fatalf("pooler direct SET ROLE tenant_b without RESET ROLE: %v", err)
	}
	var current string
	if err := poolConn.QueryRowContext(ctx, `SHOW role`).Scan(&current); err != nil {
		t.Fatalf("pooler SHOW role: %v", err)
	}
	if current != "tenant_b" {
		t.Fatalf("pooler SHOW role = %q, want tenant_b", current)
	}
	if _, err := poolConn.ExecContext(ctx, `SET ROLE tenant_c`); err == nil || SQLState(err) != "42501" {
		t.Fatalf("pooler SET ROLE tenant_c: err=%v (SQLSTATE=%q), want 42501", err, SQLState(err))
	}
	if _, err := poolConn.ExecContext(ctx, `SET ROLE postgres`); err == nil || SQLState(err) != "42501" {
		t.Fatalf("pooler SET ROLE postgres: err=%v (SQLSTATE=%q), want 42501", err, SQLState(err))
	}

	// 3. Non-member session_user cannot SET ROLE to ungranted role.
	outsiderConn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("sqlDB.Conn(outsider): %v", err)
	}
	defer outsiderConn.Close()
	setSessionUser(outsiderConn, "outsider")
	if _, err := outsiderConn.ExecContext(ctx, `SET ROLE tenant_a`); err == nil || SQLState(err) != "42501" {
		t.Fatalf("outsider SET ROLE tenant_a: err=%v (SQLSTATE=%q), want 42501", err, SQLState(err))
	}
}

func TestPhase6SessionCatalogDiscardAllCleanup(t *testing.T) {
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

	sqlDB := sql.OpenDB(NewConnectorWithCatalog(db))
	defer sqlDB.Close()

	conn1, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("sqlDB.Conn 1: %v", err)
	}
	defer conn1.Close()

	conn2, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("sqlDB.Conn 2: %v", err)
	}
	defer conn2.Close()

	// Session 1 listens on ch and acquires session advisory lock 42.
	if _, err := conn1.ExecContext(ctx, `LISTEN ch`); err != nil {
		t.Fatalf("conn1 LISTEN ch: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, `SELECT pg_advisory_lock(42)`); err != nil {
		t.Fatalf("conn1 pg_advisory_lock(42): %v", err)
	}

	// Verify session 1 is listening and session 2 cannot acquire lock 42.
	var chName string
	if err := conn1.QueryRowContext(ctx, `SELECT * FROM pg_listening_channels()`).Scan(&chName); err != nil || chName != "ch" {
		t.Fatalf("conn1 pg_listening_channels before DISCARD ALL = (%q, %v), want (ch, nil)", chName, err)
	}
	var acquired bool
	if err := conn2.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(42)`).Scan(&acquired); err != nil || acquired {
		t.Fatalf("conn2 pg_try_advisory_lock(42) while held = (%v, %v), want (false, nil)", acquired, err)
	}

	// Execute DISCARD ALL on session 1. On the embedded connector, DISCARD ALL
	// reports 0A000 (session_state) for sequence discard while still resetting
	// session state including advisory locks and LISTEN subscriptions.
	if _, err := conn1.ExecContext(ctx, `DISCARD ALL`); err != nil && SQLState(err) != "0A000" {
		t.Fatalf("conn1 DISCARD ALL: %v", err)
	}

	// Verify session 2 can immediately acquire lock 42.
	if err := conn2.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(42)`).Scan(&acquired); err != nil || !acquired {
		t.Fatalf("conn2 pg_try_advisory_lock(42) after DISCARD ALL = (%v, %v), want (true, nil)", acquired, err)
	}
	var unlocked bool
	if err := conn2.QueryRowContext(ctx, `SELECT pg_advisory_unlock(42)`).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("conn2 pg_advisory_unlock(42) = (%v, %v), want (true, nil)", unlocked, err)
	}

	// Verify LISTEN subscription is cleared on session 1.
	rows, err := conn1.QueryContext(ctx, `SELECT * FROM pg_listening_channels()`)
	if err != nil {
		t.Fatalf("conn1 pg_listening_channels after DISCARD ALL: %v", err)
	}
	var remaining []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan pg_listening_channels: %v", err)
		}
		remaining = append(remaining, c)
	}
	_ = rows.Close()
	if len(remaining) != 0 {
		t.Fatalf("conn1 pg_listening_channels after DISCARD ALL = %v, want empty", remaining)
	}

	// Publish a notification on ch from conn2 and verify conn1 does NOT receive it.
	if _, err := conn2.ExecContext(ctx, `NOTIFY ch, 'after-discard'`); err != nil {
		t.Fatalf("conn2 NOTIFY ch: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	var got Notification
	waitErr := conn1.Raw(func(driverConn any) error {
		w, ok := driverConn.(interface {
			WaitForNotification(context.Context) (Notification, error)
		})
		if !ok {
			return errors.New("driverConn does not implement WaitForNotification")
		}
		var err error
		got, err = w.WaitForNotification(waitCtx)
		return err
	})
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("conn1 WaitForNotification after DISCARD ALL = (%+v, %v), want DeadlineExceeded", got, waitErr)
	}
}
