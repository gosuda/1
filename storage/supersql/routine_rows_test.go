package supersql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// routineRowsDB builds the shared fixture for user-defined row sources: a
// base table, a named composite, and SQL/PLpgSQL routines exercising every
// FROM-clause result shape PostgreSQL accepts.
func routineRowsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := openTestDB(t)
	for _, q := range []string{
		`CREATE TABLE rngfunc(rngfuncid int, rngfuncsubid int, rngfuncname text)`,
		`INSERT INTO rngfunc VALUES (1,11,'Joe'),(2,22,'Ed'),(3,33,'Mary')`,
		`CREATE TYPE rngfunc_t AS (rngfuncid int, rngfuncsubid int, rngfuncname text)`,
		`CREATE FUNCTION getrngfunc1(int) RETURNS int LANGUAGE sql AS $$ SELECT $1 $$`,
		`CREATE FUNCTION getrngfunc2(int) RETURNS setof int LANGUAGE sql AS $$ SELECT rngfuncid FROM rngfunc WHERE rngfuncid <= $1 $$`,
		`CREATE FUNCTION getrngfunc4(int) RETURNS rngfunc_t LANGUAGE sql AS $$ SELECT * FROM rngfunc WHERE rngfuncid = $1 $$`,
		`CREATE FUNCTION getrngfunc5(int) RETURNS setof rngfunc_t LANGUAGE sql AS $$ SELECT * FROM rngfunc WHERE rngfuncid <= $1 $$`,
		`CREATE FUNCTION getrngfunc7(int) RETURNS setof record LANGUAGE sql AS $$ SELECT * FROM rngfunc WHERE rngfuncid <= $1 $$`,
		`CREATE FUNCTION tf(int) RETURNS TABLE(a int, b text) LANGUAGE sql AS $$ SELECT rngfuncid, rngfuncname FROM rngfunc WHERE rngfuncid <= $1 $$`,
		`CREATE FUNCTION scalread(int) RETURNS int LANGUAGE plpgsql AS $$ DECLARE r int; BEGIN SELECT rngfuncid INTO r FROM rngfunc WHERE rngfuncid = $1; RETURN r; END $$`,
		`CREATE FUNCTION plsr(int) RETURNS setof int LANGUAGE plpgsql AS $$ DECLARE r record; BEGIN FOR r IN SELECT rngfuncid FROM rngfunc WHERE rngfuncid <= $1 LOOP RETURN NEXT r.rngfuncid; END LOOP; END $$`,
		`CREATE FUNCTION nullcomp() RETURNS setof rngfunc_t LANGUAGE plpgsql AS $$ BEGIN RETURN NEXT NULL; RETURN NEXT (1,11,'Joe')::rngfunc_t; END $$`,
		`CREATE FUNCTION outs(out integer, out numeric) LANGUAGE sql AS $$ SELECT 1, 2.1 $$`,
		`CREATE FUNCTION onearr(OUT x rngfunc_t) LANGUAGE sql AS $$ SELECT * FROM rngfunc WHERE rngfuncid = 1 $$`,
		`CREATE FUNCTION badshape(out integer, out numeric) LANGUAGE sql AS $$ SELECT 1, 2 $$`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return db
}

// collectStrings runs a query and returns column names plus every row
// rendered as strings, so NULL and composite values compare uniformly.
func collectStrings(t *testing.T, db *sql.DB, q string) ([]string, [][]string) {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s columns: %v", q, err)
	}
	var out [][]string
	for rows.Next() {
		values := make([]interface{}, len(columns))
		scan := make([]interface{}, len(columns))
		for i := range values {
			scan[i] = &values[i]
		}
		if err := rows.Scan(scan...); err != nil {
			t.Fatalf("%s scan: %v", q, err)
		}
		row := make([]string, len(values))
		for i, value := range values {
			row[i] = fmt.Sprint(value)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s rows: %v", q, err)
	}
	return columns, out
}

func wantRows(t *testing.T, db *sql.DB, q string, columns []string, rows [][]string) {
	t.Helper()
	gotColumns, gotRows := collectStrings(t, db, q)
	if fmt.Sprint(gotColumns) != fmt.Sprint(columns) {
		t.Fatalf("%s columns=%v, want %v", q, gotColumns, columns)
	}
	if fmt.Sprint(gotRows) != fmt.Sprint(rows) {
		t.Fatalf("%s rows=%v, want %v", q, gotRows, rows)
	}
}

func wantCode(t *testing.T, db *sql.DB, q, code, message string) {
	t.Helper()
	rows, err := db.Query(q)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
		}
		err = rows.Err()
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != code || typed.Message != message {
		t.Fatalf("%s error=%v, want %s %q", q, err, code, message)
	}
}

// TestRoutineRowsScalar covers non-set routines in FROM — PostgreSQL evaluates
// them once and yields the single result row — plus scalar set-returning
// routines, including bodies that read base tables through the widened
// inner-statement read view.
func TestRoutineRowsScalar(t *testing.T) {
	db := routineRowsDB(t)
	wantRows(t, db, `SELECT * FROM getrngfunc1(2)`,
		[]string{"getrngfunc1"}, [][]string{{"2"}})
	wantRows(t, db, `SELECT * FROM getrngfunc1(2) AS one(v)`,
		[]string{"v"}, [][]string{{"2"}})
	wantRows(t, db, `SELECT * FROM getrngfunc2(2) ORDER BY 1`,
		[]string{"getrngfunc2"}, [][]string{{"1"}, {"2"}})
	wantRows(t, db, `SELECT * FROM getrngfunc2(0)`,
		[]string{"getrngfunc2"}, nil)
	wantRows(t, db, `SELECT * FROM scalread(2)`,
		[]string{"scalread"}, [][]string{{"2"}})
	wantRows(t, db, `SELECT * FROM plsr(2) ORDER BY 1`,
		[]string{"plsr"}, [][]string{{"1"}, {"2"}})
	wantRows(t, db, `SELECT getrngfunc2(2)`,
		[]string{"getrngfunc2"}, [][]string{{"1"}, {"2"}})
}

// TestRoutineRowsComposite covers composite results: named types expand into
// their fields, an empty non-set result is an all-NULL row, NULL composites
// emit NULL fields, and a single output parameter carries the composite whole.
func TestRoutineRowsComposite(t *testing.T) {
	db := routineRowsDB(t)
	fields := []string{"rngfuncid", "rngfuncsubid", "rngfuncname"}
	wantRows(t, db, `SELECT * FROM getrngfunc4(1)`,
		fields, [][]string{{"1", "11", "Joe"}})
	wantRows(t, db, `SELECT * FROM getrngfunc4(9)`,
		fields, [][]string{{"<nil>", "<nil>", "<nil>"}})
	wantRows(t, db, `SELECT * FROM getrngfunc5(2) ORDER BY 1`,
		fields, [][]string{{"1", "11", "Joe"}, {"2", "22", "Ed"}})
	wantRows(t, db, `SELECT * FROM nullcomp()`,
		fields, [][]string{{"<nil>", "<nil>", "<nil>"}, {"1", "11", "Joe"}})
	wantRows(t, db, `SELECT * FROM onearr()`,
		[]string{"x"}, [][]string{{"(1,11,Joe)"}})
	wantRows(t, db, `SELECT * FROM outs()`,
		[]string{"column1", "column2"}, [][]string{{"1", "2.1"}})
	wantRows(t, db, `SELECT getrngfunc5(1)`,
		[]string{"getrngfunc5"}, [][]string{{"(1,11,Joe)"}})
	wantRows(t, db, `SELECT tf(1)`,
		[]string{"tf"}, [][]string{{"(1,Joe)"}})
}

// TestRoutineRowsRecord covers bare record results (explicit column definition
// list required), RETURNS TABLE shapes declared by OUT parameters, column
// renames, ordinality, and the column-definition-list error taxonomy.
func TestRoutineRowsRecord(t *testing.T) {
	db := routineRowsDB(t)
	wantRows(t, db, `SELECT * FROM getrngfunc7(2) AS t(a int, b int, c text) ORDER BY 1`,
		[]string{"a", "b", "c"}, [][]string{{"1", "11", "Joe"}, {"2", "22", "Ed"}})
	wantRows(t, db, `SELECT * FROM tf(2) ORDER BY 1`,
		[]string{"a", "b"}, [][]string{{"1", "Joe"}, {"2", "Ed"}})
	wantRows(t, db, `SELECT * FROM tf(2) AS u(x, y) ORDER BY 1`,
		[]string{"x", "y"}, [][]string{{"1", "Joe"}, {"2", "Ed"}})
	wantRows(t, db, `SELECT * FROM getrngfunc5(2) WITH ORDINALITY AS f(a,b,c,o) ORDER BY 1`,
		[]string{"a", "b", "c", "o"}, [][]string{{"1", "11", "Joe", "1"}, {"2", "22", "Ed", "2"}})
	wantRows(t, db, `SELECT * FROM getrngfunc2(2) WITH ORDINALITY AS f(v,o) ORDER BY 1`,
		[]string{"v", "o"}, [][]string{{"1", "1"}, {"2", "2"}})

	wantCode(t, db, `SELECT * FROM getrngfunc7(1)`, "42601", `a column definition list is required for functions returning "record"`)
	wantCode(t, db, `SELECT * FROM tf(2) AS u(x int, y text)`, "42601", "a column definition list is redundant for a function with OUT parameters")
	wantCode(t, db, `SELECT * FROM getrngfunc5(1) AS t(a int, b int, c text)`, "42601", "a column definition list is redundant for a function returning a named composite type")
	wantCode(t, db, `SELECT * FROM getrngfunc1(1) AS t(a int)`, "42601", `a column definition list is only allowed for functions returning "record"`)
	wantCode(t, db, `SELECT * FROM getrngfunc7(1) AS t(a int)`, "42804", "function return row and query-specified return row do not match")
	wantCode(t, db, `SELECT * FROM badshape()`, "42804", "function return row and query-specified return row do not match")
}

// TestRoutineRowsEarlyClose verifies the streaming contract: LIMIT reads a
// prefix of a set routine and closing a live iterator mid-stream releases the
// routine's producer, claims, and inner read view without leaking work.
func TestRoutineRowsEarlyClose(t *testing.T) {
	db := routineRowsDB(t)
	wantRows(t, db, `SELECT * FROM getrngfunc2(3) LIMIT 1`,
		[]string{"getrngfunc2"}, [][]string{{"1"}})
	wantRows(t, db, `SELECT * FROM plsr(3) LIMIT 1`,
		[]string{"plsr"}, [][]string{{"1"}})

	rows, err := db.Query(`SELECT * FROM getrngfunc5(3)`)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if !rows.Next() {
		t.Fatal("stream produced no first row")
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("early close: %v", err)
	}

	// Context cancellation stops the producer between packets: the first row
	// is delivered, then the call context dies and iteration ends.
	ctx, cancel := context.WithCancel(context.Background())
	rows, err = db.QueryContext(ctx, `SELECT * FROM plsr(3)`)
	if err != nil {
		t.Fatalf("cancel stream open: %v", err)
	}
	if !rows.Next() {
		t.Fatal("cancel stream produced no first row")
	}
	cancel()
	for rows.Next() {
	}
	rows.Close()
}
