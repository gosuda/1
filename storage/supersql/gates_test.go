package supersql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
)

// sqlStateOf extracts the SQLSTATE from an engine error.
func sqlStateOf(err error) string {
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed.SQLState()
	}
	return ""
}

// TestEmbeddedPipelineParameters proves typed $n parameters reach the session
// pipeline through the database/sql driver surface, including a null and a
// timestamp argument.
func TestEmbeddedPipelineParameters(t *testing.T) {
	ctx := t.Context()
	database := sql.OpenDB(mustConnector(t, ctx))
	defer database.Close()

	if _, err := database.ExecContext(ctx, `CREATE TABLE params (id bigint PRIMARY KEY, note text, at timestamptz)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	stamp := time.Date(2024, 3, 14, 15, 9, 26, 0, time.UTC)
	if _, err := database.ExecContext(ctx, `INSERT INTO params (id, note, at) VALUES ($1, $2, $3)`, 7, nil, stamp); err != nil {
		t.Fatalf("parameterized insert: %v", err)
	}
	var note sql.NullString
	var at time.Time
	row := database.QueryRowContext(ctx, `SELECT note, at FROM params WHERE id = $1`, 7)
	if err := row.Scan(&note, &at); err != nil {
		t.Fatalf("parameterized select: %v", err)
	}
	if note.Valid {
		t.Fatalf("expected NULL note, got %v", note)
	}
	if !at.Equal(stamp) {
		t.Fatalf("timestamp round-trip mismatch: %v != %v", at, stamp)
	}
}

// TestEmbeddedUnsupportedProfile proves every embedded_pg18 exclusion in
// testdata/postgres/embedded_features.json rejects with SQLSTATE 0A000 and
// its feature id, rejected statements leave no partial mutation, excluded
// expressions do not advance sequences, supported neighbors still work, and
// syntax/unknown-function errors keep 42601/42883. server_transport has no
// SQL surface: it excludes wire-protocol negotiation and psql client
// commands, which never reach the engine.
func TestEmbeddedUnsupportedProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	requireCode := func(statement, code string) {
		t.Helper()
		_, err := database.Exec(ctx, statement, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil error", statement, code)
		}
		if got := sqlStateOf(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", statement, code, got, err)
		}
	}
	requireExec := func(statement string) {
		t.Helper()
		if _, err := database.Exec(ctx, statement, 0); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	for _, statement := range []string{
		`CREATE TABLE t (id bigint PRIMARY KEY, v int, j jsonb, r int4range)`,
		`CREATE TABLE parent (id int PRIMARY KEY)`,
		`CREATE SEQUENCE s`,
		`CREATE VIEW vw AS SELECT id FROM t`,
		// A partitioned table and one partition exercise the write-descriptor
		// expansion exclusion: direct writes to either are unsupported.
		`CREATE TABLE part_root (a text, b int) PARTITION BY LIST (a)`,
		`CREATE TABLE part_leaf PARTITION OF part_root FOR VALUES IN ('a')`,
	} {
		requireExec(statement)
	}

	excluded := []struct {
		feature   string
		statement string
	}{
		{"cluster_admin", `CREATE DATABASE x`},
		{"cluster_admin", `CREATE TABLESPACE ts LOCATION '/tmp/x'`},
		{"cluster_admin", `ALTER SYSTEM SET work_mem = '64MB'`},
		{"cluster_admin", `CREATE TABLE tt_ts (a int) TABLESPACE pg_default`},
		{"cluster_admin", `SELECT pg_postmaster_start_time()`},
		{"authorization", `GRANT SELECT ON t TO r`},
		{"authorization", `REVOKE SELECT ON t FROM r`},
		{"authorization", `ALTER TABLE t OWNER TO r`},
		{"authorization", `ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO r`},
		{"authorization", `SET SESSION AUTHORIZATION 'x'`},
		{"external_code", `CREATE EXTENSION hstore`},
		{"external_code", `CREATE LANGUAGE plpython3u`},
		{"external_code", `LOAD 'x'`},
		{"external_code", `SELECT pg_available_extensions()`},
		{"foreign_data", `CREATE SERVER sv FOREIGN DATA WRAPPER postgres_fdw`},
		{"foreign_data", `CREATE FOREIGN TABLE ft (a int) SERVER sv`},
		{"replication", `CREATE PUBLICATION pub FOR TABLE t`},
		{"replication", `CREATE SUBSCRIPTION sub CONNECTION 'x' PUBLICATION pub`},
		{"replication", `ALTER TABLE t REPLICA IDENTITY FULL`},
		{"replication", `SELECT pg_current_wal_lsn()`},
		{"prepared_transactions", `PREPARE TRANSACTION 'gx'`},
		{"prepared_transactions", `SELECT pg_prepared_xact()`},
		{"weaker_isolation", `BEGIN ISOLATION LEVEL REPEATABLE READ`},
		{"weaker_isolation", `BEGIN ISOLATION LEVEL READ COMMITTED`},
		{"weaker_isolation", `BEGIN ISOLATION LEVEL READ UNCOMMITTED`},
		{"notifications", `LISTEN chan`},
		{"notifications", `NOTIFY chan`},
		{"notifications", `UNLISTEN chan`},
		{"notifications", `SELECT pg_notify('chan', 'm')`},
		{"server_io", `COPY t FROM '/tmp/x'`},
		{"server_io", `COPY t TO PROGRAM 'cat'`},
		{"server_io", `SELECT pg_read_file('/tmp/x')`},
		{"server_io", `SELECT lo_create(1)`},
		{"xml", `SELECT XMLPARSE(DOCUMENT '<a/>')`},
		{"xml", `SELECT XMLELEMENT(NAME x)`},
		{"xml", `SELECT * FROM XMLTABLE('/a' PASSING '<a/>' COLUMNS b text PATH '.') xt`},
		{"xml", `SELECT xmlcomment('x')`},
		{"xml", `SELECT '<a/>'::xml`},
		{"locale_providers", `CREATE COLLATION icu_de (provider = icu, locale = 'de-DE')`},
		{"locale_providers", `ALTER COLLATION "C" RENAME TO c2`},
		{"locale_providers", `CREATE CONVERSION c FOR 'UTF8' TO 'LATIN1' FROM x`},
		{"locale_providers", `SELECT pg_collation_actual_version(1)`},
		{"physical_storage", `CREATE UNLOGGED TABLE u (a int)`},
		{"physical_storage", `CREATE TABLE tw (a int) WITH (fillfactor = 70)`},
		{"physical_storage", `SELECT ctid FROM t`},
		{"physical_storage", `SELECT t.xmin FROM t`},
		{"physical_storage", `SELECT '0-0'::tid`},
		{"physical_storage", `SELECT ctid FROM pg_catalog.pg_class`},
		{"physical_storage", `DELETE FROM t WHERE false RETURNING ctid`},
		{"non_btree_indexes", `CREATE INDEX gi ON t USING gin (j)`},
		{"non_btree_indexes", `CREATE INDEX gis ON t USING gist (r)`},
		{"non_btree_indexes", `CREATE ACCESS METHOD am TYPE INDEX HANDLER h`},
		{"non_btree_indexes", `CREATE OPERATOR FAMILY ofam USING btree`},
		{"advanced_exclusion", `CREATE TABLE ex1 (r int4range, EXCLUDE USING gist (r WITH &&))`},
		{"advanced_exclusion", `CREATE TABLE ex2 (id int, r int4range, PRIMARY KEY (id, r WITHOUT OVERLAPS))`},
		{"rewrite_hooks", `CREATE RULE rr AS ON INSERT TO t DO INSTEAD NOTHING`},
		{"rewrite_hooks", `CREATE EVENT TRIGGER et ON ddl_command_start EXECUTE FUNCTION f()`},
		{"rewrite_hooks", `SELECT pg_get_ruledef(1)`},
		{"physical_maintenance", `VACUUM t`},
		{"physical_maintenance", `CHECKPOINT`},
		{"physical_maintenance", `REINDEX DATABASE x`},
		{"physical_maintenance", `CLUSTER t USING t_pkey`},
		{"server_observability", `SELECT * FROM pg_catalog.pg_stat_activity`},
		{"server_observability", `SELECT * FROM pg_catalog.pg_authid`},
		{"server_observability", `SELECT pg_backend_pid()`},
		{"server_observability", `SELECT current_query()`},
		{"server_observability", `SHOW archive_mode`},
		{"server_observability", `SET archive_mode = 'on'`},
		{"aggregate_or_replace", `CREATE OR REPLACE AGGREGATE myavg (numeric) (stype = internal, sfunc = numeric_avg_accum, finalfunc = numeric_avg)`},
		{"session_state", `DISCARD SEQUENCES`},
		{"session_state", `DISCARD ALL`},
		{string(bind.WriteDescriptorExpansion), `INSERT INTO part_leaf VALUES ('a', 1)`},
		{string(bind.WriteDescriptorExpansion), `UPDATE part_leaf SET b = 1`},
		{string(bind.DatetimeFields), `SELECT EXTRACT(MICROSECONDS FROM DATE '2020-08-11')`},
		{string(bind.DatetimeFields), `SELECT EXTRACT(TIMEZONE FROM INTERVAL '2 days')`},
		{string(bind.EncodingConversion), `SELECT convert_from('\xbcf6c7d0', 'EUC_KR')`},
	}
	covered := make(map[string]bool)
	for _, excludedStatement := range excluded {
		_, err := database.Exec(ctx, excludedStatement.statement, 0)
		if err == nil {
			t.Fatalf("%s: expected 0A000 %s, got nil error", excludedStatement.statement, excludedStatement.feature)
		}
		if got := sqlStateOf(err); got != "0A000" {
			t.Fatalf("%s: expected SQLSTATE 0A000 %s, got %q (%v)", excludedStatement.statement, excludedStatement.feature, got, err)
		}
		var typed *model.Error
		if !errors.As(err, &typed) || typed.FeatureID != excludedStatement.feature {
			feature := ""
			if typed != nil {
				feature = typed.FeatureID
			}
			t.Fatalf("%s: expected FeatureID %q, got %q (%v)", excludedStatement.statement, excludedStatement.feature, feature, err)
		}
		covered[excludedStatement.feature] = true
	}

	// The manifest is authoritative: every unsupported feature id must have a
	// representative statement above. server_transport has none — wire
	// negotiation and client commands never reach SQL.
	manifest, err := os.ReadFile("testdata/postgres/embedded_features.json")
	if err != nil {
		t.Fatalf("read exclusion manifest: %v", err)
	}
	var features struct {
		Features []struct {
			ID          string `json:"id"`
			Disposition string `json:"disposition"`
		} `json:"features"`
	}
	if err := json.Unmarshal(manifest, &features); err != nil {
		t.Fatalf("parse exclusion manifest: %v", err)
	}
	for _, feature := range features.Features {
		if feature.Disposition != "unsupported" || feature.ID == "server_transport" {
			continue
		}
		if !covered[feature.ID] {
			t.Errorf("exclusion %q has no representative statement", feature.ID)
		}
	}

	// Rejected DDL leaves no partial mutation behind: u was never created by
	// the rejected UNLOGGED statement, so creating it now must succeed.
	requireExec(`CREATE TABLE u (a int)`)
	requireExec(`INSERT INTO u VALUES (1)`)

	// A statement rejected during binding does not advance sequence state.
	requireCode(`SELECT nextval('s'), XMLPARSE(DOCUMENT '<a/>')`, "0A000")
	result, err := database.Exec(ctx, `SELECT nextval('s')`, 0)
	if err != nil {
		t.Fatalf("nextval: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0].IntVal != 1 {
		t.Fatalf("rejected statement advanced sequence s: %+v", result.Rows)
	}

	// Physical system columns classify only against relations that expose
	// them; views, subqueries and value lists have none.
	requireCode(`SELECT xmin FROM (VALUES(1)) vv(x)`, "42703")
	requireCode(`SELECT xmin FROM vw`, "42703")
	requireCode(`SELECT xmin FROM t, t t2`, "42702")
	requireExec(`SELECT tableoid FROM t`)

	// Positive neighbors of excluded syntax remain supported.
	for _, statement := range []string{
		`SELECT '{"a":1}'::jsonb @> '{"a":1}'::jsonb`,
		`SELECT int4range(1,5) && int4range(4,10)`,
		`CREATE INDEX pi ON t (v) INCLUDE (j) WHERE id > 0`,
		`CREATE TABLE child (id int PRIMARY KEY, p int REFERENCES parent DEFERRABLE INITIALLY DEFERRED)`,
		`SELECT relname FROM pg_catalog.pg_class WHERE relname = 't'`,
		`SELECT sequencename, last_value FROM pg_catalog.pg_sequences`,
		`PREPARE p1 AS SELECT 42`,
		`EXECUTE p1`,
		`SELECT * FROM JSON_TABLE('[{"a":1},{"a":2}]', '$[*]' COLUMNS (id FOR ORDINALITY, a int PATH '$.a')) jt`,
	} {
		requireExec(statement)
	}

	// User objects shadowing excluded pg_catalog names stay usable. Unqualified
	// references still resolve to the excluded catalog object first (pg_catalog
	// leads the implicit search path), so the qualified name reaches the user
	// table.
	requireExec(`CREATE TABLE pg_stat_activity (a int)`)
	requireExec(`INSERT INTO public.pg_stat_activity VALUES (1)`)
	result, err = database.Exec(ctx, `SELECT a FROM public.pg_stat_activity`, 0)
	if err != nil {
		t.Fatalf("qualified user pg_stat_activity: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0].IntVal != 1 {
		t.Fatalf("qualified user pg_stat_activity rows: %+v", result.Rows)
	}
	requireCode(`SELECT * FROM pg_stat_activity`, "0A000")
	requireExec(`CREATE FUNCTION public.pg_backend_pid() RETURNS int LANGUAGE SQL AS 'SELECT 1'`)
	requireExec(`SELECT public.pg_backend_pid()`)

	// Malformed SQL and unknown functions keep their PostgreSQL classifications.
	requireCode(`SELEC bogus`, "42601")
	requireCode(`SELECT nosuchfunction(1)`, "42883")
}

// TestSerializableOnlyIsolation proves the embedded profile accepts only
// SERIALIZABLE: every weaker BeginTx isolation level is rejected with SQLSTATE
// 0A000, SET TRANSACTION inside an active transaction fails without poisoning
// the transaction after a savepoint rollback, SHOW reports serializable, a
// read-only transaction pins its snapshot, and a write-skew pair aborts one
// side or serializes correctly.
func TestSerializableOnlyIsolation(t *testing.T) {
	ctx := t.Context()
	database := sql.OpenDB(mustConnector(t, ctx))
	defer database.Close()
	database.SetMaxOpenConns(4)

	exec := func(c *sql.Conn, q string, args ...any) error {
		_, err := c.ExecContext(ctx, q, args...)
		return err
	}
	query1 := func(c *sql.Conn, q string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := c.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		return v
	}
	requireCode := func(err error, code, what string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil error", what, code)
		}
		if got := sqlStateOf(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", what, code, got, err)
		}
	}

	c1, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	if err := exec(c1, `CREATE TABLE t (id bigint PRIMARY KEY, v bigint)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := exec(c1, `INSERT INTO t VALUES (1, 10)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// BeginTx: by default, weaker levels automatically escalate to SERIALIZABLE.
	for _, level := range []sql.IsolationLevel{
		sql.LevelDefault, sql.LevelSerializable,
		sql.LevelSnapshot, sql.LevelRepeatableRead, sql.LevelReadCommitted,
		sql.LevelReadUncommitted, sql.LevelWriteCommitted, sql.LevelLinearizable,
	} {
		tx, err := c1.BeginTx(ctx, &sql.TxOptions{Isolation: level})
		if err != nil {
			t.Fatalf("BeginTx(%v) default escalation: %v", level, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("BeginTx(%v) rollback: %v", level, err)
		}
	}

	// Opt into strict isolation checking on c1: every weaker level rejected with 0A000.
	if err := exec(c1, `SET supersql.compat_isolation = 'off'`); err != nil {
		t.Fatalf("disable compat_isolation: %v", err)
	}
	for _, level := range []sql.IsolationLevel{
		sql.LevelSnapshot, sql.LevelRepeatableRead, sql.LevelReadCommitted,
		sql.LevelReadUncommitted, sql.LevelWriteCommitted, sql.LevelLinearizable,
	} {
		tx, err := c1.BeginTx(ctx, &sql.TxOptions{Isolation: level})
		if tx != nil {
			_ = tx.Rollback()
		}
		requireCode(err, "0A000", "BeginTx "+level.String())
	}

	// BEGIN ... ISOLATION LEVEL accepts only SERIALIZABLE when compat_isolation is off; weaker levels 0A000.
	if err := exec(c1, `BEGIN ISOLATION LEVEL SERIALIZABLE`); err != nil {
		t.Fatalf("serializable begin: %v", err)
	}
	requireCode(exec(c1, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`), "0A000", "SET TRANSACTION RR in txn")
	// The failed SET aborted the transaction; a savepoint rollback recovers it.
	if err := exec(c1, `ROLLBACK`); err != nil {
		t.Fatalf("rollback after failed SET: %v", err)
	}
	if err := exec(c1, `BEGIN`); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := exec(c1, `SAVEPOINT sp`); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	requireCode(exec(c1, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`), "0A000", "SET TRANSACTION RC in txn")
	if err := exec(c1, `ROLLBACK TO SAVEPOINT sp`); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
	if got := query1(c1, `SELECT v FROM t WHERE id = 1`); got != 10 {
		t.Fatalf("transaction not usable after savepoint rollback: v=%d", got)
	}
	if err := exec(c1, `COMMIT`); err != nil {
		t.Fatalf("commit after savepoint rollback: %v", err)
	}

	// SET SESSION CHARACTERISTICS sets the default for later transactions.
	requireCode(exec(c1, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED`), "0A000", "SET SESSION CHARACTERISTICS RC")
	if err := exec(c1, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL SERIALIZABLE`); err != nil {
		t.Fatalf("SET SESSION CHARACTERISTICS serializable: %v", err)
	}

	// SHOW reports the effective serializable isolation.
	var iso string
	if err := c1.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&iso); err != nil {
		t.Fatalf("SHOW transaction_isolation: %v", err)
	}
	if iso != "serializable" {
		t.Fatalf("transaction_isolation = %q, want serializable", iso)
	}

	// Pinned read-only snapshot: A reads 10, B commits 20, A still sees 10.
	if err := exec(c1, `BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY`); err != nil {
		t.Fatalf("read-only begin: %v", err)
	}
	if got := query1(c1, `SELECT v FROM t WHERE id = 1`); got != 10 {
		t.Fatalf("pinned read v=%d, want 10", got)
	}
	if err := exec(c2, `UPDATE t SET v = 20 WHERE id = 1`); err != nil {
		t.Fatalf("concurrent update: %v", err)
	}
	if got := query1(c1, `SELECT v FROM t WHERE id = 1`); got != 10 {
		t.Fatalf("pinned snapshot v=%d, want 10", got)
	}
	if got := query1(c2, `SELECT v FROM t WHERE id = 1`); got != 20 {
		t.Fatalf("committed read v=%d, want 20", got)
	}
	if err := exec(c1, `INSERT INTO t VALUES (2, 0)`); err == nil {
		t.Fatal("write inside READ ONLY transaction succeeded")
	}
	if err := exec(c1, `ROLLBACK`); err != nil {
		t.Fatalf("read-only rollback: %v", err)
	}

	// Write-skew: each side zeroes one account after reading the other; under
	// serializable at least one side must abort or the pair must serialize so
	// that no interleaving commits both zeroing writes against stale reads.
	if err := exec(c1, `CREATE TABLE acct (id bigint PRIMARY KEY, bal bigint)`); err != nil {
		t.Fatalf("create acct: %v", err)
	}
	if err := exec(c1, `INSERT INTO acct VALUES (1, 100), (2, 100)`); err != nil {
		t.Fatalf("seed acct: %v", err)
	}
	ready := make(chan *sql.Conn, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	transfer := func(c *sql.Conn, mine, other int64) {
		if err := exec(c, `BEGIN ISOLATION LEVEL SERIALIZABLE`); err != nil {
			done <- err
			return
		}
		var sum int64
		err := c.QueryRowContext(ctx, `SELECT bal FROM acct WHERE id = $1`, other).Scan(&sum)
		ready <- c
		<-release
		if err == nil && sum > 0 {
			err = exec(c, `UPDATE acct SET bal = 0 WHERE id = $1`, mine)
		}
		if err != nil {
			_ = exec(c, `ROLLBACK`)
			done <- err
			return
		}
		done <- exec(c, `COMMIT`)
	}
	go transfer(c1, 1, 2)
	go transfer(c2, 2, 1)
	<-ready
	<-ready
	close(release)
	err1, err2 := <-done, <-done
	aborted := err1 != nil || err2 != nil
	if aborted {
		t.Logf("write-skew serialized by abort: %v | %v", err1, err2)
	}
	var b1, b2 int64
	b1 = query1(c1, `SELECT bal FROM acct WHERE id = 1`)
	b2 = query1(c1, `SELECT bal FROM acct WHERE id = 2`)
	if b1 == 0 && b2 == 0 && !aborted {
		t.Fatal("write-skew committed both zeroing writes against stale reads")
	}
}

// TestHistoricalCatalogSnapshot proves AS OF SYSTEM TIME reads observe the
// committed snapshot at the requested certified instant, including catalog
// state from before a later DDL change.
func TestHistoricalCatalogSnapshot(t *testing.T) {
	ctx := t.Context()
	testClock := newTestClock()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: testClock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	if _, err := database.Exec(ctx, `CREATE TABLE items (id bigint PRIMARY KEY, value text)`, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := database.Exec(ctx, `INSERT INTO items VALUES (1, 'before')`, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Capture a certified instant above the committed insert.
	reading, err := testClock.Read(ctx)
	if err != nil {
		t.Fatalf("clock read: %v", err)
	}
	if _, err := database.Exec(ctx, `UPDATE items SET value = 'after' WHERE id = 1`, 0); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := database.Exec(ctx, `ALTER TABLE items ADD COLUMN extra bigint`, 0); err != nil {
		t.Fatalf("alter: %v", err)
	}
	instant := time.Unix(0, int64(reading.Latest)).UTC()
	historical, err := database.Exec(ctx,
		`SELECT value FROM items AS OF SYSTEM TIME '`+instant.Format("2006-01-02 15:04:05.999999999-07:00")+`'::timestamptz`, 0)
	if err != nil {
		t.Fatalf("historical select: %v", err)
	}
	if len(historical.Rows) != 1 || historical.Rows[0][0].StrVal != "before" {
		t.Fatalf("expected 'before' at captured instant, got %+v", historical.Rows)
	}
}

// mustConnector builds an OpenConnector against a fresh directory for
// driver-level tests.
func mustConnector(t *testing.T, ctx context.Context) *Connector {
	t.Helper()
	connector, err := OpenConnector(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	t.Cleanup(func() {
		if err := connector.Close(); err != nil {
			t.Errorf("connector close: %v", err)
		}
	})
	return connector
}
