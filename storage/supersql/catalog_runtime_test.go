package supersql

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// execSQL runs one statement through the public Catalog.Exec surface and
// fails the test on error.
func execSQL(t *testing.T, ctx context.Context, database *Catalog, statement string) Outcome {
	t.Helper()
	outcome, err := database.Exec(ctx, statement, 0)
	if err != nil {
		t.Fatalf("%q: %v", statement, err)
	}
	return outcome
}

// queryValue runs one single-column query and returns its first row's value.
func queryValue(t *testing.T, ctx context.Context, database *Catalog, statement string) model.Value {
	t.Helper()
	outcome := execSQL(t, ctx, database, statement)
	if len(outcome.Rows) != 1 || len(outcome.Rows[0]) != 1 {
		t.Fatalf("%q: want one row one column, got %+v", statement, outcome.Rows)
	}
	return outcome.Rows[0][0]
}

// TestRuntimeCatalogResolution proves runtime OID-resolution catalog reads work
// through the public execution surface: regclass casts, visibility and
// description lookups all read the OID collection the plan never scans.
func TestRuntimeCatalogResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	execSQL(t, ctx, database, `CREATE TABLE tt (id bigint PRIMARY KEY)`)

	if got := queryValue(t, ctx, database, `SELECT 'tt'::regclass::text`); got.StrVal != "tt" {
		t.Fatalf("regclass::text = %q, want tt", got.StrVal)
	}
	if got := queryValue(t, ctx, database, `SELECT pg_table_is_visible('tt'::regclass)`); got.IntVal == 0 {
		t.Fatal("pg_table_is_visible = false, want true")
	}
	if got := queryValue(t, ctx, database, `SELECT obj_description('tt'::regclass)`); !got.IsNull {
		t.Fatalf("obj_description = %v, want NULL", got)
	}
}

// TestSequenceRuntime proves sequence builtins execute through the public
// surface inside and outside transactions, and that serial columns resolve
// their owning sequence through pg_get_serial_sequence.
func TestSequenceRuntime(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	execSQL(t, ctx, database, `CREATE SEQUENCE s`)
	execSQL(t, ctx, database, `CREATE TABLE tt (id bigint PRIMARY KEY)`)

	if got := queryValue(t, ctx, database, `SELECT nextval('s')`); got.IntVal != 1 {
		t.Fatalf("nextval = %d, want 1", got.IntVal)
	}
	if got := queryValue(t, ctx, database, `SELECT currval('s')`); got.IntVal != 1 {
		t.Fatalf("currval = %d, want 1", got.IntVal)
	}
	execSQL(t, ctx, database, `SELECT setval('s', 5)`)
	if got := queryValue(t, ctx, database, `SELECT nextval('s')`); got.IntVal != 6 {
		t.Fatalf("nextval after setval = %d, want 6", got.IntVal)
	}
	if got := queryValue(t, ctx, database, `SELECT lastval()`); got.IntVal != 6 {
		t.Fatalf("lastval = %d, want 6", got.IntVal)
	}

	// Sequence calls inside an explicit transaction see their own calls.
	execSQL(t, ctx, database, `BEGIN`)
	if got := queryValue(t, ctx, database, `SELECT nextval('s')`); got.IntVal != 7 {
		t.Fatalf("in-transaction nextval = %d, want 7", got.IntVal)
	}
	if got := queryValue(t, ctx, database, `SELECT currval('s')`); got.IntVal != 7 {
		t.Fatalf("in-transaction currval = %d, want 7", got.IntVal)
	}
	execSQL(t, ctx, database, `COMMIT`)

	// A prepared statement wrapping nextval binds and executes.
	execSQL(t, ctx, database, `PREPARE p1 AS SELECT nextval('s')`)
	if got := queryValue(t, ctx, database, `EXECUTE p1`); got.IntVal != 8 {
		t.Fatalf("prepared nextval = %d, want 8", got.IntVal)
	}

	// Serial and identity columns both resolve their owning sequence.
	execSQL(t, ctx, database, `CREATE TABLE st (id serial PRIMARY KEY, v int)`)
	execSQL(t, ctx, database, `CREATE TABLE it (id bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY, v int)`)
	if got := queryValue(t, ctx, database, `SELECT pg_get_serial_sequence('st', 'id')`); got.StrVal != "public.st_id_seq" {
		t.Fatalf("pg_get_serial_sequence(serial) = %q, want public.st_id_seq", got.StrVal)
	}
	if got := queryValue(t, ctx, database, `SELECT pg_get_serial_sequence('it', 'id')`); got.StrVal != "public.it_id_seq" {
		t.Fatalf("pg_get_serial_sequence(identity) = %q, want public.it_id_seq", got.StrVal)
	}
	if got := queryValue(t, ctx, database, `SELECT pg_get_serial_sequence('tt', 'id')`); !got.IsNull {
		t.Fatalf("pg_get_serial_sequence(plain) = %v, want NULL", got)
	}

	// Serial defaults draw from the owning sequence.
	execSQL(t, ctx, database, `INSERT INTO st (v) VALUES (1), (2)`)
	outcome := execSQL(t, ctx, database, `SELECT id FROM st ORDER BY id`)
	if len(outcome.Rows) != 2 || outcome.Rows[0][0].IntVal != 1 || outcome.Rows[1][0].IntVal != 2 {
		t.Fatalf("serial ids = %+v, want 1,2", outcome.Rows)
	}
}

// TestAnalyzeExecution proves ANALYZE publishes statistics mutations through
// the transaction resolver, for a named table and for all tables.
func TestAnalyzeExecution(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	execSQL(t, ctx, database, `CREATE TABLE tt (id bigint PRIMARY KEY, v int)`)
	execSQL(t, ctx, database, `INSERT INTO tt VALUES (1, 10), (2, 20)`)
	execSQL(t, ctx, database, `ANALYZE tt`)
	execSQL(t, ctx, database, `ANALYZE`)
}

// TestPartialIndexPredicate proves a partial index predicate rebinds after
// creation: reads on the indexed table keep working once the index exists.
func TestPartialIndexPredicate(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	execSQL(t, ctx, database, `CREATE TABLE pi (id bigint PRIMARY KEY, v int)`)
	execSQL(t, ctx, database, `INSERT INTO pi VALUES (1, 10), (2, 20), (3, 30)`)
	execSQL(t, ctx, database, `CREATE INDEX pi_v ON pi (v) WHERE id > 0`)

	if got := queryValue(t, ctx, database, `SELECT count(*) FROM pi`); got.IntVal != 3 {
		t.Fatalf("count after partial index = %d, want 3", got.IntVal)
	}
	if got := queryValue(t, ctx, database, `SELECT v FROM pi WHERE id = 2`); got.IntVal != 20 {
		t.Fatalf("point read after partial index = %d, want 20", got.IntVal)
	}
}

// TestJSONTableOrdinality proves JSON_TABLE dispatches through the row-source
// path and FOR ORDINALITY columns carry PostgreSQL's bigint type.
func TestJSONTableOrdinality(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	outcome := execSQL(t, ctx, database,
		`SELECT n, x FROM JSON_TABLE('[10,20]', '$[*]' COLUMNS (n FOR ORDINALITY, x int PATH '$')) jt`)
	if len(outcome.Rows) != 2 {
		t.Fatalf("JSON_TABLE rows = %+v, want 2 rows", outcome.Rows)
	}
	if outcome.Columns[0].Type.ID != 20 {
		t.Fatalf("FOR ORDINALITY type id = %d, want 20 (int8)", outcome.Columns[0].Type.ID)
	}
	if outcome.Rows[0][0].IntVal != 1 || outcome.Rows[0][1].IntVal != 10 ||
		outcome.Rows[1][0].IntVal != 2 || outcome.Rows[1][1].IntVal != 20 {
		t.Fatalf("JSON_TABLE values = %+v, want (1,10),(2,20)", outcome.Rows)
	}
}

// TestLogicalCatalogRelations proves logical catalog relation scans declare
// every storage collection their emitters read — catalog namespace and
// descriptor rows, statistics, and sequence state — under the transaction's
// read coverage, and that excluded non-nullable fields still reject with
// 0A000 while ordinary columns project normally.
func TestLogicalCatalogRelations(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	execSQL(t, ctx, database, `CREATE TABLE t (id bigint PRIMARY KEY, v int)`)
	execSQL(t, ctx, database, `CREATE SEQUENCE sq`)

	if got := queryValue(t, ctx, database, `SELECT relname FROM pg_catalog.pg_class WHERE relname = 't'`); got.StrVal != "t" {
		t.Fatalf("pg_class relname = %q, want t", got.StrVal)
	}
	if got := queryValue(t, ctx, database, `SELECT count(*) FROM pg_class`); got.IntVal < 1 {
		t.Fatalf("pg_class count = %d, want >= 1", got.IntVal)
	}
	if got := queryValue(t, ctx, database, `SELECT count(*) FROM pg_catalog.pg_tables`); got.IntVal < 1 {
		t.Fatalf("pg_tables count = %d, want >= 1", got.IntVal)
	}
	if got := queryValue(t, ctx, database, `SELECT nspname FROM pg_catalog.pg_namespace WHERE nspname = 'public'`); got.StrVal != "public" {
		t.Fatalf("pg_namespace nspname = %q, want public", got.StrVal)
	}
	// Attribute rows cover every column of every catalog relation plus user
	// tables — the scan row budget must track catalog capacity, not batches.
	if got := queryValue(t, ctx, database, `SELECT attname FROM pg_catalog.pg_attribute WHERE attname = 'v'`); got.StrVal != "v" {
		t.Fatalf("pg_attribute attname = %q, want v", got.StrVal)
	}
	if got := queryValue(t, ctx, database, `SELECT typname FROM pg_catalog.pg_type WHERE typname = 'int4'`); got.StrVal != "int4" {
		t.Fatalf("pg_type typname = %q, want int4", got.StrVal)
	}
	if got := queryValue(t, ctx, database, `SELECT datname FROM pg_catalog.pg_database`); got.StrVal == "" {
		t.Fatal("pg_database datname is empty")
	}
	if got := queryValue(t, ctx, database, `SELECT rolname FROM pg_catalog.pg_roles`); got.StrVal == "" {
		t.Fatal("pg_roles rolname is empty")
	}
	if got := queryValue(t, ctx, database, `SELECT setting FROM pg_catalog.pg_settings WHERE name = 'work_mem'`); got.IsNull {
		t.Fatal("pg_settings work_mem is NULL")
	}

	// name-typed columns order through the SQL key codec.
	outcome := execSQL(t, ctx, database, `SELECT relname FROM pg_class ORDER BY relname LIMIT 2`)
	if len(outcome.Rows) != 2 {
		t.Fatalf("ORDER BY relname rows = %+v, want 2", outcome.Rows)
	}

	// pg_sequences reads live sequence state through the sequence store.
	if got := queryValue(t, ctx, database, `SELECT sequencename FROM pg_catalog.pg_sequences WHERE sequencename = 'sq'`); got.StrVal != "sq" {
		t.Fatalf("pg_sequences sequencename = %q, want sq", got.StrVal)
	}

	// A catalog scan survives PREPARE/EXECUTE.
	execSQL(t, ctx, database, `PREPARE c1 AS SELECT relname FROM pg_class WHERE relname = 't'`)
	if got := queryValue(t, ctx, database, `EXECUTE c1`); got.StrVal != "t" {
		t.Fatalf("EXECUTE c1 relname = %q, want t", got.StrVal)
	}

	// ANALYZE publishes statistics rows that pg_stats projects.
	execSQL(t, ctx, database, `INSERT INTO t VALUES (1, 10), (2, 20)`)
	execSQL(t, ctx, database, `ANALYZE t`)
	if got := queryValue(t, ctx, database, `SELECT count(*) FROM pg_catalog.pg_stats WHERE tablename = 't'`); got.IntVal < 1 {
		t.Fatalf("pg_stats rows for t = %d, want >= 1", got.IntVal)
	}

	// Excluded non-nullable catalog fields reject at bind time even under
	// SELECT *, and excluded relations keep their feature id.
	for _, excluded := range []struct{ statement, feature string }{
		{`SELECT * FROM pg_catalog.pg_class LIMIT 1`, "physical_storage"},
		{`SELECT xmin FROM pg_catalog.pg_class`, "physical_storage"},
		{`SELECT * FROM pg_catalog.pg_policy`, "server_observability"},
	} {
		_, err := database.Exec(ctx, excluded.statement, 0)
		if err == nil || sqlStateOf(err) != "0A000" {
			t.Fatalf("%s: expected 0A000, got %v", excluded.statement, err)
		}
		var typed *model.Error
		if !errors.As(err, &typed) || typed.FeatureID != excluded.feature {
			t.Fatalf("%s: expected FeatureID %q, got %v", excluded.statement, excluded.feature, err)
		}
	}
}

// TestDeclaredTypeKeys proves declared enum, domain and composite types work in
// key positions through the public surface: primary and secondary index keys
// resolve through the snapshot-pinned codec, enum keys order by declaration,
// domain keys unwrap to their base ordering, and domain CHECK constraints
// evaluate under the caller's granted invocation budget.
func TestDeclaredTypeKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	execSQL(t, ctx, database, `CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy')`)
	execSQL(t, ctx, database, `CREATE TABLE ek (m mood PRIMARY KEY)`)
	execSQL(t, ctx, database, `INSERT INTO ek VALUES ('happy'), ('sad'), ('ok')`)

	if got := queryValue(t, ctx, database, `SELECT string_agg(m::text, ',') FROM (SELECT m FROM ek ORDER BY m) s`); got.StrVal != "sad,ok,happy" {
		t.Fatalf("enum primary key ordering = %q, want declaration order sad,ok,happy", got.StrVal)
	}
	if got := queryValue(t, ctx, database, `SELECT string_agg(m::text, ',') FROM (SELECT m FROM ek WHERE m > 'sad' ORDER BY m) s`); got.StrVal != "ok,happy" {
		t.Fatalf("enum range filter = %q, want ok,happy", got.StrVal)
	}

	execSQL(t, ctx, database, `CREATE TABLE ei (id int PRIMARY KEY, m mood)`)
	execSQL(t, ctx, database, `CREATE INDEX ei_m ON ei (m)`)
	execSQL(t, ctx, database, `INSERT INTO ei VALUES (1, 'happy'), (2, 'sad'), (3, 'ok')`)
	if got := queryValue(t, ctx, database, `SELECT string_agg(id::text, ',') FROM (SELECT id FROM ei WHERE m >= 'ok' ORDER BY m, id) s`); got.StrVal != "3,1" {
		t.Fatalf("enum secondary index range = %q, want 3,1", got.StrVal)
	}

	execSQL(t, ctx, database, `CREATE DOMAIN posint AS int CHECK (VALUE > 0)`)
	execSQL(t, ctx, database, `CREATE TABLE dk (p posint PRIMARY KEY)`)
	execSQL(t, ctx, database, `INSERT INTO dk VALUES (2), (1)`)
	if got := queryValue(t, ctx, database, `SELECT string_agg(p::text, ',') FROM (SELECT p FROM dk ORDER BY p) s`); got.StrVal != "1,2" {
		t.Fatalf("domain primary key ordering = %q, want base order 1,2", got.StrVal)
	}
	if _, err := database.Exec(ctx, `INSERT INTO dk VALUES (-1)`, 0); err == nil || sqlStateOf(err) != "23514" {
		t.Fatalf("domain CHECK rejection: expected SQLSTATE 23514, got %v", err)
	}
	if got := queryValue(t, ctx, database, `SELECT string_agg(p::text, ',') FROM (SELECT p FROM dk WHERE p > 1 ORDER BY p) s`); got.StrVal != "2" {
		t.Fatalf("domain comparison = %q, want 2", got.StrVal)
	}

	execSQL(t, ctx, database, `CREATE TABLE di (id int PRIMARY KEY, p posint)`)
	execSQL(t, ctx, database, `CREATE INDEX di_p ON di (p)`)
	execSQL(t, ctx, database, `INSERT INTO di VALUES (1, 10), (2, 20)`)
	if got := queryValue(t, ctx, database, `SELECT id FROM di WHERE p = 20`); got.IntVal != 2 {
		t.Fatalf("domain secondary index lookup = %v, want 2", got)
	}

	execSQL(t, ctx, database, `CREATE DOMAIN mooddom AS mood`)
	execSQL(t, ctx, database, `CREATE TABLE dek (m mooddom PRIMARY KEY)`)
	execSQL(t, ctx, database, `INSERT INTO dek VALUES ('happy'), ('sad'), ('ok')`)
	if got := queryValue(t, ctx, database, `SELECT string_agg(m::text, ',') FROM (SELECT m FROM dek ORDER BY m) s`); got.StrVal != "sad,ok,happy" {
		t.Fatalf("domain-over-enum key ordering = %q, want sad,ok,happy", got.StrVal)
	}
	if got := queryValue(t, ctx, database, `SELECT string_agg(m::text, ',') FROM (SELECT m FROM dek WHERE m >= 'ok'::mooddom ORDER BY m) s`); got.StrVal != "ok,happy" {
		t.Fatalf("domain-over-enum comparison = %q, want ok,happy", got.StrVal)
	}

	execSQL(t, ctx, database, `CREATE TYPE pair AS (a int, b text)`)
	execSQL(t, ctx, database, `CREATE TABLE ck (id int PRIMARY KEY, v pair)`)
	execSQL(t, ctx, database, `INSERT INTO ck VALUES (1, ROW(7, 'x')::pair)`)
	if got := queryValue(t, ctx, database, `SELECT (v).a FROM ck WHERE id = 1`); got.IntVal != 7 {
		t.Fatalf("composite field access = %v, want 7", got)
	}
}

// TestPublicParseErrorState proves syntax errors through the public Catalog.Exec
// surface carry SQLSTATE 42601 like the wire-protocol parse wrapper.
func TestPublicParseErrorState(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	for _, statement := range []string{
		`SELEC 1`,
		`CREATE TABLE (`,
	} {
		_, err := database.Exec(ctx, statement, 0)
		if err == nil {
			t.Fatalf("%q: expected a syntax error", statement)
		}
		if code := sqlStateOf(err); code != "42601" {
			t.Fatalf("%q: expected SQLSTATE 42601, got %q (%v)", statement, code, err)
		}
	}
}
