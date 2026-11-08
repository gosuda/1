package supersql

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// matviewDB opens a connector-backed database with the shared base table used
// by the materialized-view tests.
func matviewDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := t.Context()
	database := sql.OpenDB(mustConnector(t, ctx))
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("sql.DB close: %v", err)
		}
	})
	for _, statement := range []string{
		`CREATE TABLE base (id bigint PRIMARY KEY, name text, amount bigint)`,
		`INSERT INTO base VALUES (1, 'alice', 10), (2, 'bob', 20), (3, 'carol', 30)`,
	} {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return database
}

// scanRows reads every row into strings for comparison.
func scanRows(t *testing.T, rows *sql.Rows) [][]string {
	t.Helper()
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	out := make([][]string, 0, 4)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		row := make([]string, len(columns))
		for i, value := range values {
			switch typed := value.(type) {
			case nil:
				row[i] = "NULL"
			case []byte:
				row[i] = string(typed)
			default:
				row[i] = fmt.Sprint(typed)
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestMaterializedViewScan covers the population path and the read path: the
// matview exposes exactly the view's columns (never the backing table's
// system identity column) and pg_matviews reports the populated state.
func TestMaterializedViewScan(t *testing.T) {
	ctx := t.Context()
	database := matviewDB(t)

	if _, err := database.ExecContext(ctx, `CREATE MATERIALIZED VIEW mv AS SELECT id, name FROM base WHERE amount >= 20`); err != nil {
		t.Fatalf("CREATE MATERIALIZED VIEW: %v", err)
	}
	rows, err := database.QueryContext(ctx, `SELECT * FROM mv ORDER BY id`)
	if err != nil {
		t.Fatalf("SELECT * FROM mv: %v", err)
	}
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if len(columns) != 2 || columns[0] != "id" || columns[1] != "name" {
		t.Fatalf("materialized view columns = %v, want [id name]", columns)
	}
	if got := scanRows(t, rows); len(got) != 2 || got[0][1] != "bob" || got[1][1] != "carol" {
		t.Fatalf("materialized view rows = %v", got)
	}
	var count int64
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM mv`).Scan(&count); err != nil {
		t.Fatalf("count(*): %v", err)
	}
	if count != 2 {
		t.Fatalf("count(*) = %d, want 2", count)
	}
	var populated bool
	if err := database.QueryRowContext(ctx, `SELECT ispopulated FROM pg_catalog.pg_matviews WHERE matviewname = 'mv'`).Scan(&populated); err != nil {
		t.Fatalf("pg_matviews: %v", err)
	}
	if !populated {
		t.Fatal("pg_matviews.ispopulated = false after CREATE MATERIALIZED VIEW")
	}
}

// TestMaterializedViewUnpopulatedRejectsScan pins the 55000 guard: a matview
// created WITH NO DATA cannot be read until it is refreshed, and the refresh
// makes it readable.
func TestMaterializedViewUnpopulatedRejectsScan(t *testing.T) {
	ctx := t.Context()
	database := matviewDB(t)

	if _, err := database.ExecContext(ctx, `CREATE MATERIALIZED VIEW mv AS SELECT id, name FROM base WITH NO DATA`); err != nil {
		t.Fatalf("CREATE MATERIALIZED VIEW WITH NO DATA: %v", err)
	}
	var populated bool
	if err := database.QueryRowContext(ctx, `SELECT ispopulated FROM pg_catalog.pg_matviews WHERE matviewname = 'mv'`).Scan(&populated); err != nil {
		t.Fatalf("pg_matviews: %v", err)
	}
	if populated {
		t.Fatal("pg_matviews.ispopulated = true after WITH NO DATA")
	}
	_, err := database.QueryContext(ctx, `SELECT * FROM mv`)
	if state := sqlStateOf(err); state != "55000" {
		t.Fatalf("scan of unpopulated matview = %v (SQLSTATE %q), want 55000", err, state)
	}
	var typed *model.Error
	if !errors.As(err, &typed) || !strings.Contains(typed.Message, `materialized view "mv" has not been populated`) {
		t.Fatalf("unpopulated matview message = %v", err)
	}
	if _, err := database.ExecContext(ctx, `REFRESH MATERIALIZED VIEW mv`); err != nil {
		t.Fatalf("REFRESH MATERIALIZED VIEW: %v", err)
	}
	var count int64
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM mv`).Scan(&count); err != nil {
		t.Fatalf("count(*) after refresh: %v", err)
	}
	if count != 3 {
		t.Fatalf("count(*) after refresh = %d, want 3", count)
	}
	if err := database.QueryRowContext(ctx, `SELECT ispopulated FROM pg_catalog.pg_matviews WHERE matviewname = 'mv'`).Scan(&populated); err != nil {
		t.Fatalf("pg_matviews after refresh: %v", err)
	}
	if !populated {
		t.Fatal("pg_matviews.ispopulated = false after REFRESH")
	}
}

// TestMaterializedViewRefreshPicksUpChanges proves REFRESH re-runs the view
// query against the current contents of the base table.
func TestMaterializedViewRefreshPicksUpChanges(t *testing.T) {
	ctx := t.Context()
	database := matviewDB(t)

	if _, err := database.ExecContext(ctx, `CREATE MATERIALIZED VIEW mv AS SELECT id FROM base`); err != nil {
		t.Fatalf("CREATE MATERIALIZED VIEW: %v", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO base VALUES (4, 'dave', 40)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	var count int64
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM mv`).Scan(&count); err != nil {
		t.Fatalf("count(*) before refresh: %v", err)
	}
	if count != 3 {
		t.Fatalf("count(*) before refresh = %d, want 3 (stale contents)", count)
	}
	if _, err := database.ExecContext(ctx, `REFRESH MATERIALIZED VIEW mv`); err != nil {
		t.Fatalf("REFRESH MATERIALIZED VIEW: %v", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM mv`).Scan(&count); err != nil {
		t.Fatalf("count(*) after refresh: %v", err)
	}
	if count != 4 {
		t.Fatalf("count(*) after refresh = %d, want 4", count)
	}
}

// TestMaterializedViewIndexScan proves an index declared on the materialized
// view is admitted, and that the view's rows survive the refresh that
// regenerates them, so a filtered scan reads the refreshed contents. Index
// selection itself is a cost decision the optimizer makes the same way for a
// table and a materialized view, so this test asserts admission and content,
// not the chosen access path.
func TestMaterializedViewIndexScan(t *testing.T) {
	ctx := t.Context()
	database := matviewDB(t)

	if _, err := database.ExecContext(ctx, `CREATE MATERIALIZED VIEW mv AS SELECT id, name FROM base`); err != nil {
		t.Fatalf("CREATE MATERIALIZED VIEW: %v", err)
	}
	if _, err := database.ExecContext(ctx, `CREATE INDEX mv_name_idx ON mv (name)`); err != nil {
		t.Fatalf("CREATE INDEX on materialized view: %v", err)
	}
	var hasIndexes bool
	if err := database.QueryRowContext(ctx, `SELECT hasindexes FROM pg_catalog.pg_matviews WHERE matviewname = 'mv'`).Scan(&hasIndexes); err != nil {
		t.Fatalf("pg_matviews.hasindexes: %v", err)
	}
	if !hasIndexes {
		t.Fatal("pg_matviews.hasindexes = false after CREATE INDEX on the materialized view")
	}
	rows, err := database.QueryContext(ctx, `SELECT id FROM mv WHERE name = 'bob'`)
	if err != nil {
		t.Fatalf("filtered scan: %v", err)
	}
	if got := scanRows(t, rows); len(got) != 1 || got[0][0] != "2" {
		t.Fatalf("filtered scan rows = %v", got)
	}
	if _, err := database.ExecContext(ctx, `REFRESH MATERIALIZED VIEW mv`); err != nil {
		t.Fatalf("REFRESH MATERIALIZED VIEW: %v", err)
	}
	rows, err = database.QueryContext(ctx, `SELECT id FROM mv WHERE name = 'carol'`)
	if err != nil {
		t.Fatalf("filtered scan after refresh: %v", err)
	}
	if got := scanRows(t, rows); len(got) != 1 || got[0][0] != "3" {
		t.Fatalf("filtered scan after refresh rows = %v", got)
	}
	// The index is maintained by the refresh: a lookup on the indexed column
	// after the base table changed still reports the refreshed row set.
	if _, err := database.ExecContext(ctx, `INSERT INTO base VALUES (4, 'dave', 40)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := database.ExecContext(ctx, `REFRESH MATERIALIZED VIEW mv`); err != nil {
		t.Fatalf("REFRESH MATERIALIZED VIEW: %v", err)
	}
	rows, err = database.QueryContext(ctx, `SELECT id FROM mv WHERE name = 'dave'`)
	if err != nil {
		t.Fatalf("filtered scan for the refreshed row: %v", err)
	}
	if got := scanRows(t, rows); len(got) != 1 || got[0][0] != "4" {
		t.Fatalf("filtered scan for the refreshed row = %v", got)
	}
}

// TestMaterializedViewRefreshConcurrently exercises REFRESH ... CONCURRENTLY,
// which the binder only admits for a populated matview with a unique index.
func TestMaterializedViewRefreshConcurrently(t *testing.T) {
	ctx := t.Context()
	database := matviewDB(t)

	if _, err := database.ExecContext(ctx, `CREATE MATERIALIZED VIEW mv AS SELECT id, name FROM base`); err != nil {
		t.Fatalf("CREATE MATERIALIZED VIEW: %v", err)
	}
	if _, err := database.ExecContext(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY mv`); err == nil {
		t.Fatal("CONCURRENTLY without a unique index succeeded")
	} else if state := sqlStateOf(err); state != "55000" {
		t.Fatalf("CONCURRENTLY without unique index = %v (SQLSTATE %q), want 55000", err, state)
	}
	if _, err := database.ExecContext(ctx, `CREATE UNIQUE INDEX mv_id_idx ON mv (id)`); err != nil {
		t.Fatalf("CREATE UNIQUE INDEX on materialized view: %v", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO base VALUES (4, 'dave', 40)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := database.ExecContext(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY mv`); err != nil {
		t.Fatalf("REFRESH MATERIALIZED VIEW CONCURRENTLY: %v", err)
	}
	var count int64
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM mv`).Scan(&count); err != nil {
		t.Fatalf("count(*): %v", err)
	}
	if count != 4 {
		t.Fatalf("count(*) after concurrent refresh = %d, want 4", count)
	}
}

// TestMaterializedViewConstantQuery pins the minimal form from the slice
// verification: CREATE MATERIALIZED VIEW mv AS SELECT 1 AS a.
func TestMaterializedViewConstantQuery(t *testing.T) {
	ctx := t.Context()
	database := sql.OpenDB(mustConnector(t, ctx))
	defer database.Close()

	if _, err := database.ExecContext(ctx, `CREATE MATERIALIZED VIEW mv AS SELECT 1 AS a`); err != nil {
		t.Fatalf("CREATE MATERIALIZED VIEW: %v", err)
	}
	rows, err := database.QueryContext(ctx, `SELECT * FROM mv`)
	if err != nil {
		t.Fatalf("SELECT * FROM mv: %v", err)
	}
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if len(columns) != 1 || columns[0] != "a" {
		t.Fatalf("columns = %v, want [a]", columns)
	}
	if got := scanRows(t, rows); len(got) != 1 || got[0][0] != "1" {
		t.Fatalf("rows = %v, want [[1]]", got)
	}
}

// TestCreateTableAsSelect covers the shared population path: CREATE TABLE AS
// runs the same nested INSERT ... SELECT as a materialized view.
func TestCreateTableAsSelect(t *testing.T) {
	ctx := t.Context()
	database := matviewDB(t)

	if _, err := database.ExecContext(ctx, `CREATE TABLE snapshot AS SELECT id, name FROM base WHERE amount >= 20`); err != nil {
		t.Fatalf("CREATE TABLE AS: %v", err)
	}
	rows, err := database.QueryContext(ctx, `SELECT id, name FROM snapshot ORDER BY id`)
	if err != nil {
		t.Fatalf("SELECT from CTAS table: %v", err)
	}
	if got := scanRows(t, rows); len(got) != 2 || got[0][1] != "bob" || got[1][1] != "carol" {
		t.Fatalf("CTAS rows = %v", got)
	}
	// A CTAS table is an ordinary writable table.
	if _, err := database.ExecContext(ctx, `INSERT INTO snapshot VALUES (9, 'zoe')`); err != nil {
		t.Fatalf("INSERT into CTAS table: %v", err)
	}
	var count int64
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM snapshot`).Scan(&count); err != nil {
		t.Fatalf("count(*): %v", err)
	}
	if count != 3 {
		t.Fatalf("count(*) = %d, want 3", count)
	}
}

// TestCreateTableAsSelectNoData covers the unpopulated CTAS form: the table is
// created empty and is immediately readable as an ordinary table.
func TestCreateTableAsSelectNoData(t *testing.T) {
	ctx := t.Context()
	database := matviewDB(t)

	if _, err := database.ExecContext(ctx, `CREATE TABLE empty AS SELECT id, name FROM base WITH NO DATA`); err != nil {
		t.Fatalf("CREATE TABLE AS WITH NO DATA: %v", err)
	}
	var count int64
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM empty`).Scan(&count); err != nil {
		t.Fatalf("count(*) from unpopulated CTAS table: %v", err)
	}
	if count != 0 {
		t.Fatalf("count(*) = %d, want 0", count)
	}
}
