package supersql

import (
	"strings"
	"testing"
)

// TestPLpgSQLPercentType exercises %TYPE in DECLARE and routine signatures:
// relation-column references, datum references, composite-field references,
// delimited identifiers, and the failure shapes PostgreSQL defines.
func TestPLpgSQLPercentType(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	fail := func(q, want string) {
		t.Helper()
		_, err := db.Exec(q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err=%v, want %q", q, err, want)
		}
	}
	exec("CREATE TABLE ptrel (a int, b text, \"Weird.Col\" int8)")
	exec("CREATE TYPE ptrec AS (x int, y text)")

	// rel.col%TYPE in DECLARE copies the column's storage type.
	exec(`CREATE FUNCTION pt_col() RETURNS int LANGUAGE plpgsql AS $$
DECLARE v ptrel.a%TYPE; BEGIN v := 41; RETURN v + 1; END $$`)
	var n int64
	if err := db.QueryRow("SELECT pt_col()").Scan(&n); err != nil || n != 42 {
		t.Fatalf("pt_col=%d err=%v, want 42", n, err)
	}

	// var%TYPE copies an earlier datum's resolved type; later datums are
	// invisible to the reference.
	exec(`CREATE FUNCTION pt_var() RETURNS int LANGUAGE plpgsql AS $$
DECLARE v ptrel.a%TYPE; w v%TYPE; BEGIN v := 9; w := v * 11; RETURN w; END $$`)
	if err := db.QueryRow("SELECT pt_var()").Scan(&n); err != nil || n != 99 {
		t.Fatalf("pt_var=%d err=%v, want 99", n, err)
	}

	// recordvar.field%TYPE walks a declared composite field.
	exec(`CREATE FUNCTION pt_field() RETURNS text LANGUAGE plpgsql AS $$
DECLARE r ptrec%rowtype; z r.y%TYPE; BEGIN z := 'deep'; RETURN z; END $$`)
	var s string
	if err := db.QueryRow("SELECT pt_field()").Scan(&s); err != nil || s != "deep" {
		t.Fatalf("pt_field=%q err=%v, want deep", s, err)
	}

	// A delimited identifier containing a dot is one name, not a qualifier.
	exec(`CREATE FUNCTION pt_quoted() RETURNS int8 LANGUAGE plpgsql AS $$
DECLARE v ptrel."Weird.Col"%TYPE; BEGIN v := 7; RETURN v; END $$`)
	if err := db.QueryRow("SELECT pt_quoted()").Scan(&n); err != nil || n != 7 {
		t.Fatalf("pt_quoted=%d err=%v, want 7", n, err)
	}

	// %TYPE in routine parameter and SETOF return positions resolves through
	// the same catalog path as DECLARE.
	exec(`CREATE FUNCTION pt_param(ptrel.a%type) RETURNS ptrel.a%TYPE LANGUAGE plpgsql AS $$
DECLARE kept ptrel.a%TYPE; BEGIN kept := $1; RETURN kept; END $$`)
	if err := db.QueryRow("SELECT pt_param(17)").Scan(&n); err != nil || n != 17 {
		t.Fatalf("pt_param=%d err=%v, want 17", n, err)
	}
	exec(`CREATE FUNCTION pt_setof() RETURNS SETOF public.ptrel.a%TYPE LANGUAGE plpgsql AS $$
BEGIN RETURN NEXT 1; RETURN NEXT 2; RETURN; END $$`)
	rows, err := db.Query("SELECT * FROM pt_setof() ORDER BY pt_setof")
	if err != nil {
		t.Fatalf("pt_setof: %v", err)
	}
	var got []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("pt_setof scan: %v", err)
		}
		got = append(got, v)
	}
	if err := rows.Close(); err != nil || len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("pt_setof rows=%v err=%v, want [1 2]", got, err)
	}

	fail(`CREATE FUNCTION pt_bad1() RETURNS int LANGUAGE plpgsql AS $$
DECLARE v ptrel.nope%TYPE; BEGIN RETURN 1; END $$`, `column "nope" does not exist`)
	fail(`CREATE FUNCTION pt_bad2() RETURNS int LANGUAGE plpgsql AS $$
DECLARE v missing_rel.c%TYPE; BEGIN RETURN 1; END $$`, "does not exist")
	fail(`CREATE FUNCTION pt_bad3(nothere.a%type) RETURNS int LANGUAGE plpgsql AS $$
BEGIN RETURN 1; END $$`, "does not exist")
	fail(`CREATE FUNCTION pt_bad4() RETURNS int LANGUAGE plpgsql AS $$
DECLARE v ptrel%TYPE; BEGIN RETURN 1; END $$`, `"ptrel" is not a known variable`)
	fail(`CREATE FUNCTION pt_bad5() RETURNS int LANGUAGE plpgsql AS $$
DECLARE v nothere%TYPE; BEGIN RETURN 1; END $$`, `"nothere" is not a known variable`)
}

// TestPLpgSQLPercentRowtype exercises %ROWTYPE declarations: relation and
// composite-type rows, datum references, delimited identifiers, FOR/INTO
// target shape, and PostgreSQL's error cases.
func TestPLpgSQLPercentRowtype(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	fail := func(q, want string) {
		t.Helper()
		_, err := db.Exec(q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err=%v, want %q", q, err, want)
		}
	}
	exec("CREATE TABLE rtrel (a int, b text)")
	exec("INSERT INTO rtrel VALUES (10, 'ten'), (20, 'twenty')")
	exec("CREATE TYPE rtrec AS (x int, y text)")

	// rel%rowtype keeps the relation's fixed field names through INTO.
	exec(`CREATE FUNCTION rt_table() RETURNS text LANGUAGE plpgsql AS $$
DECLARE r rtrel%ROWTYPE; BEGIN SELECT * INTO r FROM rtrel WHERE a = 20; RETURN r.b; END $$`)
	var s string
	if err := db.QueryRow("SELECT rt_table()").Scan(&s); err != nil || s != "twenty" {
		t.Fatalf("rt_table=%q err=%v, want twenty", s, err)
	}

	// compositetype%rowtype preserves the composite's field names even though
	// the durable layout codec strips them.
	exec(`CREATE FUNCTION rt_composite() RETURNS text LANGUAGE plpgsql AS $$
DECLARE r rtrec%ROWTYPE; BEGIN r := ROW(1, 'ok')::rtrec; RETURN r.y; END $$`)
	if err := db.QueryRow("SELECT rt_composite()").Scan(&s); err != nil || s != "ok" {
		t.Fatalf("rt_composite=%q err=%v, want ok", s, err)
	}

	// rowvar%rowtype reuses a declared row datum's shape.
	exec(`CREATE FUNCTION rt_ref() RETURNS int LANGUAGE plpgsql AS $$
DECLARE r rtrel%ROWTYPE; r2 r%ROWTYPE; BEGIN SELECT * INTO r2 FROM rtrel WHERE a = 10; RETURN r2.a; END $$`)
	var n int64
	if err := db.QueryRow("SELECT rt_ref()").Scan(&n); err != nil || n != 10 {
		t.Fatalf("rt_ref=%d err=%v, want 10", n, err)
	}

	// A %rowtype datum is a FOR ... IN target exactly like a record; the
	// query row must match the declared width.
	exec(`CREATE FUNCTION rt_for() RETURNS int LANGUAGE plpgsql AS $$
DECLARE r rtrel%ROWTYPE; total int := 0; BEGIN
FOR r IN SELECT * FROM rtrel ORDER BY a LOOP total := total + r.a; END LOOP;
RETURN total; END $$`)
	if err := db.QueryRow("SELECT rt_for()").Scan(&n); err != nil || n != 30 {
		t.Fatalf("rt_for=%d err=%v, want 30", n, err)
	}

	// Field assignment into an unassigned row variable fails at runtime,
	// matching PostgreSQL.
	exec(`CREATE FUNCTION rt_unassigned() RETURNS int LANGUAGE plpgsql AS $$
DECLARE r rtrel%ROWTYPE; BEGIN r.a := 1; RETURN r.a; END $$`)
	_, err := db.Exec("SELECT rt_unassigned()")
	if err == nil || !strings.Contains(err.Error(), "not assigned") {
		t.Fatalf("rt_unassigned err=%v, want unassigned-record error", err)
	}

	fail(`CREATE FUNCTION rt_bad1() RETURNS int LANGUAGE plpgsql AS $$
DECLARE v int%rowtype; BEGIN RETURN 1; END $$`, `type "int" is not composite`)
	fail(`CREATE FUNCTION rt_bad2() RETURNS int LANGUAGE plpgsql AS $$
DECLARE v norel%rowtype; BEGIN RETURN 1; END $$`, "does not exist")
	fail(`CREATE FUNCTION rt_bad3() RETURNS int LANGUAGE plpgsql AS $$
DECLARE v "RTREL"%rowtype; BEGIN RETURN 1; END $$`, "does not exist")
	// A bare record datum has no static fields for %TYPE to read.
	fail(`CREATE FUNCTION rt_bad4() RETURNS int LANGUAGE plpgsql AS $$
DECLARE z record; v z.f%TYPE; BEGIN RETURN 1; END $$`, `record has no field "f"`)
}
